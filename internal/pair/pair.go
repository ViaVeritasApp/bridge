package pair

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"bridge/internal/config"
	"bridge/internal/httpx"
	"bridge/internal/logx"
	"bridge/internal/tlsx"
	"bridge/internal/utils"
)

type claimReq struct {
	Code    string `json:"code"`
	BaseURL string `json:"base_url"`
	Name    string `json:"name,omitempty"`
}

type claimResp struct {
	ID             string   `json:"id"`
	OrganizationID string   `json:"organizationId"`
	AllowedOrigin  []string `json:"allowed_origin"`
	ClientID       string   `json:"client_id"`
	ClientSecret   string   `json:"client_secret"`
	SigningKey     string   `json:"signing_key"`
	Subdomain      string   `json:"subdomain"`
	TLSEnabled     bool     `json:"tls_enabled"`
	TLSDomain      string   `json:"tls_domain,omitempty"`
	// Nullable on the wire: null while ACME is pending (the cert then arrives via
	// the heartbeat response). Pointers distinguish "API returned null" from
	// "empty string".
	TLSCertPEM     *string `json:"tls_cert_pem"`
	TLSKeyPEM      *string `json:"tls_key_pem"`
	TLSFingerprint *string `json:"tls_fingerprint"`
	TLSNotAfter    *string `json:"tls_not_after"`
}

// Claim swaps a pairing code for a device row + config from ViaVeritas.
func Claim(apiBaseURL, code, baseURL string) (config.Config, error) {
	apiBaseURL = strings.TrimRight(apiBaseURL, "/")
	cfg := config.Defaults()
	cfg.APIBaseURL = apiBaseURL

	hostname, _ := os.Hostname()
	body, _ := json.Marshal(claimReq{
		Code:    code,
		BaseURL: baseURL,
		Name:    hostname,
	})

	url := apiBaseURL + "/bridges/claim"
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return cfg, err
	}
	req.Header.Set("Content-Type", "application/json")

	// The API gets the bridge its certificate before answering, which waits up to a minute for DNS:
	// a client that gave up sooner would leave the API with a bridge this one never heard of.
	client := httpx.Client(150 * time.Second)
	res, err := client.Do(req)
	if err != nil {
		return cfg, fmt.Errorf("claim request failed: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode >= 400 {
		return cfg, fmt.Errorf("claim failed (HTTP %d): %s", res.StatusCode, httpx.ErrorSnippet(res, 1024))
	}

	var parsed claimResp
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return cfg, fmt.Errorf("invalid claim response: %w", err)
	}

	cfg.BridgeID = parsed.ID
	cfg.OrganizationID = parsed.OrganizationID
	cfg.ClientID = parsed.ClientID
	cfg.ClientSecret = parsed.ClientSecret
	cfg.SigningKey = parsed.SigningKey
	cfg.AllowedOrigin = parsed.AllowedOrigin
	// TLS info is best-effort — empty when ACME failed at claim time; the bridge
	// serves HTTP until the renewal cron pushes a cert on a later heartbeat.
	cfg.TLSDomain = parsed.TLSDomain
	cfg.Subdomain = parsed.Subdomain
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return cfg, fmt.Errorf("claim response did not include client credentials")
	}

	// Persist the cert now when the API shipped it, so the bridge can boot
	// straight into HTTPS. Any null cert/key/fingerprint means ACME hasn't
	// completed — skip silently; the heartbeat path retries.
	if parsed.TLSCertPEM != nil && parsed.TLSKeyPEM != nil && parsed.TLSFingerprint != nil &&
		*parsed.TLSCertPEM != "" && *parsed.TLSKeyPEM != "" && *parsed.TLSFingerprint != "" {
		if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
			logx.Warn("pair", "could not create config dir for TLS store: %v", err)
		} else if _, err := tlsx.Store(config.Dir(), *parsed.TLSCertPEM, *parsed.TLSKeyPEM, *parsed.TLSFingerprint); err != nil {
			logx.Warn("pair", "could not store TLS cert at claim time: %v (renewal cron will retry)", err)
		}
	}
	return cfg, nil
}

// lanIPv4 returns the machine's LAN IPv4 — the address other devices use to
// reach this host. It "dials" a UDP socket (no packet sent) so the OS picks the
// source IP via the routing table, choosing the interface that actually reaches
// the network. That is reliable on Windows, where virtual adapters (Hyper-V,
// WSL, Docker, VPN) enumerate ahead of the physical NIC. Falls back to an
// interface scan with no default route; "" when nothing usable.
//
// Single source of truth so the HTTP LAN URL, the dashed TLS hostname, and any
// LAN-IP logic agree on the advertised IP.
func lanIPv4() string {
	if conn, err := net.Dial("udp4", "8.8.8.8:80"); err == nil {
		defer conn.Close()
		if ua, ok := conn.LocalAddr().(*net.UDPAddr); ok {
			if ip4 := ua.IP.To4(); ip4 != nil && !ip4.IsLoopback() {
				return ip4.String()
			}
		}
	}
	return fallbackLANIPv4()
}

// fallbackLANIPv4 scans interfaces for a private IPv4 when the routing-table
// trick fails. Skips down/loopback/point-to-point (VPN) interfaces and
// link-local (169.254.x) addresses.
func fallbackLANIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 ||
			iface.Flags&net.FlagLoopback != 0 ||
			iface.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			if ip4.IsPrivate() {
				return ip4.String()
			}
		}
	}
	return ""
}

// GuessBaseURL returns the IP-based bridge URL (e.g. http://192.168.1.50:5000),
// falling back to localhost when no LAN interface is found.
func GuessBaseURL(bindAddr string) string {
	port := utils.PortOf(bindAddr, "5050")
	if ip := lanIPv4(); ip != "" {
		return fmt.Sprintf("http://%s:%s", ip, port)
	}
	_ = runtime.GOOS
	return fmt.Sprintf("http://localhost:%s", port)
}

// GuessMdnsBaseURL returns the mDNS hostname-based bridge URL (e.g.
// http://kwstas-pc.local:5000). macOS, Windows 10+, and Linux+avahi answer
// <hostname>.local. Returns "" if no usable hostname can be derived.
func GuessMdnsBaseURL(bindAddr string) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return ""
	}
	// Strip any trailing ".local" the OS already added, then re-append exactly one.
	for _, suffix := range []string{".local.", ".local", ".lan", ".home"} {
		if len(host) > len(suffix) && host[len(host)-len(suffix):] == suffix {
			host = host[:len(host)-len(suffix)]
		}
	}
	// Lowercase, strip anything non-DNS-safe.
	clean := make([]byte, 0, len(host))
	for i := 0; i < len(host); i++ {
		c := host[i]
		switch {
		case c >= 'A' && c <= 'Z':
			clean = append(clean, c+32)
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-':
			clean = append(clean, c)
		}
	}
	if len(clean) == 0 {
		return ""
	}
	return fmt.Sprintf("http://%s.local:%s", string(clean), utils.PortOf(bindAddr, "5050"))
}

// LANURLs holds the URLs the bridge advertises for its HTTP listener: IP-based
// HTTP, mDNS HTTP, and (when TLS is configured) the IP-encoded HTTPS hostname
// covered by the per-bridge wildcard cert. Any field is "" when it cannot be
// derived (no LAN IP, no bridge port, TLS not enabled).
type LANURLs struct {
	BridgeBaseURL     string
	BridgeMdnsBaseURL string
	BridgeTLSBaseURL  string
}

// GuessLANURLs derives every advertised URL from cfg. TLS fields are populated
// only when cfg.TLSEnabled() (TLS hostname assigned AND cert on disk); otherwise
// the TLS field stays empty and the CRM falls back to bridgeBaseUrl/bridgeMdnsBaseUrl.
func GuessLANURLs(cfg config.Config) LANURLs {
	u := LANURLs{}
	ba := cfg.BridgeAddr()
	if ba == "" {
		return u // bridge port not configured yet
	}
	u.BridgeBaseURL = GuessBaseURL(ba)
	u.BridgeMdnsBaseURL = GuessMdnsBaseURL(ba)
	if cfg.TLSEnabled() {
		if ip := lanIPv4(); ip != "" {
			host := tlsx.PublicHost(ip, cfg.Subdomain, cfg.TLSDomain)
			u.BridgeTLSBaseURL = fmt.Sprintf("https://%s:%s", host, utils.PortOf(ba, ""))
		}
	}
	return u
}
