package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"bridge/internal/logx"
	"bridge/internal/streams"
)

type httpParams struct {
	Method         string      `json:"method"`
	URL            string      `json:"url"`
	Headers        [][2]string `json:"headers,omitempty"`
	BodyBytes      int64       `json:"body_bytes,omitempty"`
	TimeoutSeconds int         `json:"timeout_seconds,omitempty"`
	// TLSPin, when given, is the SHA-256 of the certificate HTTPS must present
	// (a server with a certificate of its own); without it, the system's authorities check it.
	TLSPin string `json:"tls_pin,omitempty"`
}

// Head is the first line an HTTP stream sends back, as JSON, before the body.
type Head struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers"`
	// Address is the address reached.
	Address string   `json:"address,omitempty"`
	TLS     *TLSInfo `json:"tls,omitempty"`
}

// TLSInfo is the certificate the target presented.
type TLSInfo struct {
	Version  string `json:"version"`
	Subject  string `json:"subject"`
	Issuer   string `json:"issuer"`
	NotAfter string `json:"not_after"`
	SHA256   string `json:"sha256"`
}

var methods = map[string]bool{http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true, http.MethodOptions: true}

// Headers a hop answers for itself, never passed on.
var hopByHop = map[string]bool{"connection": true, "keep-alive": true, "proxy-connection": true, "proxy-authenticate": true, "proxy-authorization": true, "te": true, "trailer": true, "transfer-encoding": true, "upgrade": true, "host": true}

func target(raw string) (*url.URL, string, int, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return nil, "", 0, &streams.Refusal{Code: streams.CodeInvalid, Message: "a request goes to an http:// or https:// address, without a user or a password in it"}
	}
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if p := u.Port(); p != "" {
		if port, err = strconv.Atoi(p); err != nil || port < 1 || port > 65535 {
			return nil, "", 0, &streams.Refusal{Code: streams.CodeInvalid, Message: "the address's port isn't one"}
		}
	}
	return u, u.Hostname(), port, nil
}

func checkHTTP(o streams.Open) error {
	var p httpParams
	if err := json.Unmarshal(o.Params, &p); err != nil {
		return &streams.Refusal{Code: streams.CodeInvalid, Message: "the request couldn't be read"}
	}
	if !methods[strings.ToUpper(p.Method)] {
		return &streams.Refusal{Code: streams.CodeInvalid, Message: fmt.Sprintf("%q isn't a method the bridge sends", p.Method)}
	}
	if p.BodyBytes < 0 || p.BodyBytes > MaxRequestBody {
		return &streams.Refusal{Code: streams.CodeInvalid, Message: "the request's body is too large"}
	}
	if p.TLSPin != "" && !isPin(p.TLSPin) {
		return &streams.Refusal{Code: streams.CodeInvalid, Message: "a certificate's pin is its SHA-256, in hex"}
	}
	_, _, _, err := target(p.URL)
	return err
}

func isPin(pin string) bool {
	if len(pin) != 64 {
		return false
	}
	_, err := hex.DecodeString(pin)
	return err == nil
}

// HTTP sends one request and streams back its answer: a JSON line (Head) then
// the body. Redirects are answered as they are, never followed; HTTPS is
// checked against the system's authorities, or only against the pin the
// request gives. The request's body is what ViaVeritas sends first, BodyBytes of it.
func HTTP(s *streams.Stream) error {
	var p httpParams
	_ = json.Unmarshal(s.Params, &p)
	u, host, port, _ := target(p.URL)
	timeout := DefaultTimeout
	if p.TimeoutSeconds > 0 {
		timeout = min(time.Duration(p.TimeoutSeconds)*time.Second, MaxTimeout)
	}
	ctx, cancel := context.WithTimeout(s.Context(), timeout)
	defer cancel()

	ips, err := resolve(ctx, host)
	if err != nil {
		refuse(s, err)
		return nil
	}
	var reached string
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, err := dial(ctx, ips, port)
			if err == nil {
				reached = conn.RemoteAddr().String()
			}
			return conn, err
		},
		TLSClientConfig:       tlsConfig(host, strings.ToLower(p.TLSPin)),
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	var body io.Reader
	if p.BodyBytes > 0 {
		body = io.LimitReader(s, p.BodyBytes)
	}
	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(p.Method), u.String(), body)
	if err != nil {
		refuse(s, err)
		return nil
	}
	req.ContentLength = p.BodyBytes
	for _, h := range p.Headers {
		if !hopByHop[strings.ToLower(h[0])] {
			req.Header.Add(h[0], h[1])
		}
	}
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		code, message := classify(err, ctx)
		s.Close(code, message)
		return nil
	}
	defer resp.Body.Close()

	head := Head{Status: resp.StatusCode, Headers: map[string][]string{}, Address: reached, TLS: tlsInfo(resp.TLS)}
	for k, v := range resp.Header {
		if !hopByHop[strings.ToLower(k)] {
			head.Headers[k] = v
		}
	}
	line, _ := json.Marshal(head)
	if _, err := s.Write(append(line, '\n')); err != nil {
		return nil
	}
	n, err := io.Copy(s, resp.Body)
	logInfo("%s %s for %s: %d, %d bytes in %s", req.Method, redacted(u), who(s.User), resp.StatusCode, n, time.Since(started).Round(time.Millisecond))
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		s.Close(CodeTimeout, fmt.Sprintf("its answer took longer than %s", timeout))
	}
	return nil
}

// tlsConfig checks the target's certificate: as usual, or only against the pin (a server with a certificate of its own).
func tlsConfig(serverName, pin string) *tls.Config {
	c := &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	if pin == "" {
		return c
	}
	c.InsecureSkipVerify = true // the pin is the check
	c.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return errors.New("it presented no certificate")
		}
		sum := sha256.Sum256(raw[0])
		if got := hex.EncodeToString(sum[:]); got != pin {
			return fmt.Errorf("its certificate (%s) isn't the one pinned", got)
		}
		return nil
	}
	return c
}

func tlsInfo(state *tls.ConnectionState) *TLSInfo {
	if state == nil || len(state.PeerCertificates) == 0 {
		return nil
	}
	cert := state.PeerCertificates[0]
	sum := sha256.Sum256(cert.Raw)
	return &TLSInfo{
		Version:  tls.VersionName(state.Version),
		Subject:  cert.Subject.String(),
		Issuer:   cert.Issuer.String(),
		NotAfter: cert.NotAfter.UTC().Format(time.RFC3339),
		SHA256:   hex.EncodeToString(sum[:]),
	}
}

// classify names why a request failed, without what it carried.
func classify(err error, ctx context.Context) (string, string) {
	var refusal *streams.Refusal
	var certErr *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	switch {
	case errors.As(err, &refusal):
		return refusal.Code, refusal.Message
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return CodeTimeout, "it didn't answer in time"
	case errors.As(err, &certErr), errors.As(err, &unknownAuthority), errors.As(err, &hostname), strings.Contains(err.Error(), "certificate"), strings.Contains(err.Error(), "tls:"):
		return CodeTLS, fmt.Sprintf("its certificate wasn't trusted: %v", unwrapURL(err))
	}
	return CodeUnreachable, fmt.Sprintf("it couldn't be reached: %v", unwrapURL(err))
}

func unwrapURL(err error) error {
	var u *url.Error
	if errors.As(err, &u) {
		return u.Err
	}
	return err
}

// redacted is an address for the log: without its query, which may carry secrets.
func redacted(u *url.URL) string {
	c := *u
	c.RawQuery, c.Fragment = "", ""
	return c.String()
}

func logInfo(format string, args ...any) { logx.Info("proxy", format, args...) }
