package heartbeat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"bridge/internal/config"
	"bridge/internal/httpx"
	"bridge/internal/logx"
	"bridge/internal/pair"
	"bridge/internal/platform"
	"bridge/internal/tlsx"
)

type beat struct {
	FreeBytes         int64  `json:"free_bytes"`
	Version           string `json:"version,omitempty"`
	BridgeBaseURL     string `json:"base_url,omitempty"`
	BridgeMdnsBaseURL string `json:"msdns_url,omitempty"`
	BridgeTlsBaseURL  string `json:"tls_base_url,omitempty"`
	// SHA-256 fingerprint (hex, no colons) of the on-disk cert. The API uses it
	// to skip resending a cert the bridge already has; "" means "send me one".
	CurrentTlsFingerprint string `json:"tls_fingerprint,omitempty"`
	// Outcome of the file-sync that preceded this beat: empty on success, else
	// the failure (with the time it failed) so the API can surface the problem.
	SyncError   string `json:"sync_error,omitempty"`
	SyncErrorAt string `json:"sync_error_at,omitempty"`
}

// beatResp is the heartbeat response. The server fills the TLS fields only when
// our CurrentTlsFingerprint differs; otherwise it returns just {"success":true}.
// tls_domain rides along with cert pushes so a zone rotation reaches the bridge
// without a re-pair.
type beatResp struct {
	Success        bool   `json:"success"`
	TLSDomain      string `json:"tls_domain,omitempty"`
	TLSCertPem     string `json:"tls_cert_pem,omitempty"`
	TLSKeyPem      string `json:"tls_key_pem,omitempty"`
	TLSFingerprint string `json:"tls_fingerprint,omitempty"`
	TLSNotAfter    string `json:"tls_not_after,omitempty"`
}

// SendNow sends a single heartbeat immediately after pairing so the API records
// the bridge and provisions/pushes TLS on the spot, rather than waiting for the
// first loop tick. Best effort — Send logs and swallows any failure.
func SendNow(cfg config.Config) {
	logx.Info("heartbeat", "sending initial heartbeat after pairing")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	url := httpx.NormalizeURL(cfg.APIBaseURL, "/bridges/heartbeat")
	Send(ctx, cfg, httpx.Client(10*time.Second), url, nil)
}

// Send posts one heartbeat to url, run right after a file-sync attempt: syncErr
// is that attempt's outcome (nil = success), forwarded so the API sees sync
// status. cfg is reloaded so a `configure` edit or changed LAN IP is reflected.
func Send(ctx context.Context, cfg config.Config, client *http.Client, url string, syncErr error) {
	// Reload so a `configure` edit or changed LAN IP is reflected; fall back to
	// the caller's snapshot on error.
	live := config.LoadOr(cfg)
	u := pair.GuessLANURLs(live)
	b := beat{
		FreeBytes:             platform.Current().DiskFreeBytes(live.DataDir()),
		Version:               live.Version,
		BridgeBaseURL:         u.BridgeBaseURL,
		BridgeMdnsBaseURL:     u.BridgeMdnsBaseURL,
		BridgeTlsBaseURL:      u.BridgeTLSBaseURL,
		CurrentTlsFingerprint: tlsx.Fingerprint(config.Dir()),
	}
	if syncErr != nil {
		msg := syncErr.Error()
		if len(msg) > 500 {
			msg = msg[:500]
		}
		b.SyncError = msg
		b.SyncErrorAt = time.Now().UTC().Format(time.RFC3339)
	}
	body, _ := json.Marshal(b)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		logx.Warn("heartbeat", "request build failed: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	httpx.SetAuth(req, httpx.Auth{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret})

	start := time.Now()
	res, err := client.Do(req)
	took := time.Since(start).Round(time.Millisecond)
	if err != nil {
		logx.Warn("heartbeat", "send failed: %v (took %s)", err, took)
		return
	}
	defer res.Body.Close()
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		logx.Debug("heartbeat", "ok (status=%d, took %s)", res.StatusCode, took)
		// Drain + decode the body before further work so the connection can be
		// reused and a malformed body never breaks the write below.
		raw, _ := io.ReadAll(res.Body)
		var r beatResp
		_ = json.Unmarshal(raw, &r) // tolerant: a non-JSON body just means "no cert this tick"
		applyCertPush(r)
		// Reload-modify-save: touch only LastHeartbeatAt so we never revert a
		// field changed on disk with this snapshot.
		if c, lerr := config.Load(); lerr == nil {
			c.LastHeartbeatAt = time.Now().UTC().Format(time.RFC3339)
			if serr := config.Save(c); serr != nil {
				logx.Warn("heartbeat", "could not persist last-heartbeat time: %v", serr)
			}
		}
	} else {
		logx.Warn("heartbeat", "non-2xx response (status=%d, took %s)", res.StatusCode, took)
	}
}

// applyCertPush stores a renewed cert when the server returns one. The server
// only sends one when our fingerprint differs, but tlsx.Store double-checks so a
// corrupt/duplicate response is a no-op rather than a needless write + listener
// restart (bridgesrv relaunches on the cert.pem mtime change). A TLSDomain
// shipped alongside is persisted (reload-modify-save) so a zone rotation needs
// no re-pair.
func applyCertPush(r beatResp) {
	if r.TLSFingerprint == "" || r.TLSCertPem == "" || r.TLSKeyPem == "" {
		return
	}
	changed, err := tlsx.Store(config.Dir(), r.TLSCertPem, r.TLSKeyPem, r.TLSFingerprint)
	if err != nil {
		logx.Warn("heartbeat", "TLS cert push rejected: %v", err)
		return
	}
	if !changed {
		return
	}
	if r.TLSDomain != "" {
		if c, lerr := config.Load(); lerr == nil && c.TLSDomain != r.TLSDomain {
			c.TLSDomain = r.TLSDomain
			if serr := config.Save(c); serr != nil {
				logx.Warn("heartbeat", "could not persist updated tls_domain: %v", serr)
			}
		}
	}
	if r.TLSNotAfter != "" {
		logx.Ok("heartbeat", "installed new TLS cert (fingerprint=%s notAfter=%s)",
			shortFp(r.TLSFingerprint), r.TLSNotAfter)
	} else {
		logx.Ok("heartbeat", "installed new TLS cert (fingerprint=%s)", shortFp(r.TLSFingerprint))
	}
}

func shortFp(fp string) string {
	if len(fp) <= 12 {
		return fp
	}
	return fp[:12] + "…"
}
