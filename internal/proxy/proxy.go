// Package proxy lets ViaVeritas reach what this computer can reach while the
// proxy feature runs: an HTTP request ("http" streams) or a TCP connection
// ("tcp" streams), to any host and port, with no list kept here. Switching the
// feature off is what stops it.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"bridge/internal/config"
	"bridge/internal/streams"
)

// Codes a proxy stream is closed with, beside the streams' own.
const (
	CodeUnreachable = "unreachable"
	CodeTLS         = "tls"
	CodeTimeout     = "timeout"
)

const (
	// MaxHTTP and MaxTCP open at once on this bridge.
	MaxHTTP = 4
	MaxTCP  = 4
	// DefaultTimeout and MaxTimeout of an HTTP request.
	DefaultTimeout = 30 * time.Second
	MaxTimeout     = 2 * time.Minute
	// MaxRequestBody is the largest body sent with a request.
	MaxRequestBody = 10 << 20

	resolveTimeout = 5 * time.Second
	dialTimeout    = 10 * time.Second
)

// Register offers HTTP requests and TCP connections on reg.
func Register(reg *streams.Registry) {
	reg.HandleChecked("http", config.FeatureProxy, MaxHTTP, checkHTTP, HTTP)
	reg.HandleChecked("tcp", config.FeatureProxy, MaxTCP, checkTCP, TCP)
}

// resolve finds the addresses host answers at on this computer's network.
func resolve(ctx context.Context, host string) ([]net.IP, error) {
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	rctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	addrs, err := net.DefaultResolver.LookupIPAddr(rctx, host)
	cancel()
	if err != nil || len(addrs) == 0 {
		return nil, &streams.Refusal{Code: CodeUnreachable, Message: fmt.Sprintf("%s doesn't resolve on this computer's network", host)}
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}

// dial connects to the first address that answers.
func dial(ctx context.Context, ips []net.IP, port int) (net.Conn, error) {
	d := net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	var last error
	for _, ip := range ips {
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), strconv.Itoa(port)))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, &streams.Refusal{Code: CodeUnreachable, Message: fmt.Sprintf("it couldn't be reached from this computer: %v", last)}
}

// refuse closes a stream with a refusal's code, or as an error.
func refuse(s *streams.Stream, err error) {
	if r, ok := err.(*streams.Refusal); ok {
		s.Close(r.Code, r.Message)
		return
	}
	s.Close(streams.CodeError, err.Error())
}

type tcpParams struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func checkTCP(o streams.Open) error {
	var p tcpParams
	if err := json.Unmarshal(o.Params, &p); err != nil || strings.TrimSpace(p.Host) == "" || p.Port < 1 || p.Port > 65535 {
		return &streams.Refusal{Code: streams.CodeInvalid, Message: "a connection names a host and a port"}
	}
	return nil
}

// TCP connects to a host and port, and passes bytes both ways until
// either side ends: what ViaVeritas sends goes to the target, what it answers
// comes back.
func TCP(s *streams.Stream) error {
	var p tcpParams
	_ = json.Unmarshal(s.Params, &p)
	ctx, cancel := context.WithTimeout(s.Context(), resolveTimeout+dialTimeout)
	ips, err := resolve(ctx, p.Host)
	if err != nil {
		cancel()
		refuse(s, err)
		return nil
	}
	conn, err := dial(ctx, ips, p.Port)
	cancel()
	if err != nil {
		refuse(s, err)
		return nil
	}
	defer conn.Close()
	logInfo("connected %s to %s (%s)", who(s.User), net.JoinHostPort(p.Host, strconv.Itoa(p.Port)), conn.RemoteAddr())
	s.Info(map[string]any{"address": conn.RemoteAddr().String()})

	go func() {
		<-s.Context().Done()
		_ = conn.Close()
	}()
	go func() {
		buf := make([]byte, streams.ChunkSize)
		for {
			n, err := s.Read(buf)
			if n > 0 {
				if _, werr := conn.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				if tcp, ok := conn.(*net.TCPConn); ok {
					_ = tcp.CloseWrite()
				}
				return
			}
		}
	}()
	buf := make([]byte, streams.ChunkSize)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if _, werr := s.Write(buf[:n]); werr != nil {
				return nil
			}
		}
		if err != nil {
			return nil
		}
	}
}

func who(u streams.User) string {
	switch {
	case u.Name != "":
		return u.Name
	case u.Email != "":
		return u.Email
	}
	return "ViaVeritas"
}
