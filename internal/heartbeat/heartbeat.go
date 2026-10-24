// Package heartbeat tells ViaVeritas how the bridge stands, every Interval and
// whenever something it reports changes (Nudge): its addresses, version,
// folders, the last sync's outcome, this computer's feature switches and how
// it updates. The answer brings ViaVeritas's switches, the key that checks
// download links, the certificate for the secure address, and the release to
// update to when there is a newer one.
package heartbeat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"runtime"
	"slices"
	"sync"
	"time"

	"bridge/internal/config"
	"bridge/internal/httpx"
	"bridge/internal/logx"
	"bridge/internal/pair"
	"bridge/internal/platform"
	"bridge/internal/selfupdate"
	"bridge/internal/tlsx"
	"bridge/internal/watch"
)

// Interval is how often the service heartbeats when nothing changes.
const Interval = 30 * time.Second

var (
	syncMu       sync.Mutex
	syncErr      error
	nudges       = make(chan struct{}, 1)
	updateStatus func() selfupdate.Status
)

// SetUpdateStatus says where heartbeats read how updating stands.
func SetUpdateStatus(fn func() selfupdate.Status) {
	syncMu.Lock()
	updateStatus = fn
	syncMu.Unlock()
}

// SetSyncResult keeps the last sync's outcome for the next heartbeat, and
// reports whether it differs from the one before.
func SetSyncResult(err error) bool {
	syncMu.Lock()
	defer syncMu.Unlock()
	changed := (syncErr == nil) != (err == nil) || (err != nil && syncErr.Error() != err.Error())
	syncErr = err
	return changed
}

// SyncResult is the last sync's outcome (nil: it succeeded, or nothing syncs).
func SyncResult() error {
	syncMu.Lock()
	defer syncMu.Unlock()
	return syncErr
}

// Nudge asks for a heartbeat now rather than at the next interval.
func Nudge() {
	select {
	case nudges <- struct{}{}:
	default:
	}
}

// Nudged fires after Nudge.
func Nudged() <-chan struct{} { return nudges }

type beatFolder struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	FreeBytes *int64 `json:"free_bytes,omitempty"`
}

type beat struct {
	// The least free space among its folders: the one that fills first.
	FreeBytes         *int64 `json:"free_bytes,omitempty"`
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
	// Without a signing key the bridge can't check download links; the API
	// answers with one.
	HasSigningKey bool `json:"has_signing_key"`
	// What the latest scan found: files indexed and entries it couldn't read.
	// Left out until the first scan finishes.
	Files   *int `json:"files,omitempty"`
	Skipped *int `json:"skipped,omitempty"`
	// The first folder, for readers from before Folders.
	DataRoot string `json:"data_root,omitempty"`
	// Each synced folder, its free space left out while it can't be read.
	Folders  []beatFolder `json:"folders,omitempty"`
	OS       string       `json:"os,omitempty"`
	Arch     string       `json:"arch,omitempty"`
	Hostname string       `json:"hostname,omitempty"`
	// What the socket speaks (config.Protocol), this computer's switch for every
	// feature, whether remote access is paused, and what this version can do.
	Protocol     int                     `json:"protocol"`
	Features     map[config.Feature]bool `json:"features"`
	Paused       bool                    `json:"paused"`
	Capabilities []string                `json:"capabilities"`
	// How the update in progress, or the last one that failed, stands.
	Update *selfupdate.Status `json:"update,omitempty"`
	// Who terminals and commands run as here, and whether its screen says when one starts.
	RunAs          config.RunAs `json:"run_as"`
	SessionNotices bool         `json:"session_notices"`
	// The logs ViaVeritas may read here, the bridge's own first, as listed here alone.
	LogSources []config.LogSource `json:"log_sources"`
}

// beatResp is the heartbeat response. The server fills the TLS fields only when
// our CurrentTlsFingerprint differs; otherwise it returns just {"success":true}.
// tls_domain rides along with cert pushes so a zone rotation reaches the bridge
// without a re-pair.
type beatResp struct {
	Success        bool     `json:"success"`
	SigningKey     string   `json:"signing_key,omitempty"`
	AllowedOrigin  []string `json:"allowed_origin,omitempty"`
	Subdomain      string   `json:"subdomain,omitempty"`
	TLSDomain      string   `json:"tls_domain,omitempty"`
	TLSCertPem     string   `json:"tls_cert_pem,omitempty"`
	TLSKeyPem      string   `json:"tls_key_pem,omitempty"`
	TLSFingerprint string   `json:"tls_fingerprint,omitempty"`
	TLSNotAfter    string   `json:"tls_not_after,omitempty"`
	// ViaVeritas's switch for each feature; an API from before them leaves it out.
	PortalFeatures map[config.Feature]bool `json:"portal_features,omitempty"`
	// Update is the newer release that suits this computer, when there is one.
	Update *selfupdate.Offer `json:"update,omitempty"`
}

// Answer is what a heartbeat brought back: OK once ViaVeritas took it, with
// the release to update to when there is a newer one.
type Answer struct {
	OK     bool
	Update *selfupdate.Offer
}

// SendNow sends a single heartbeat immediately after pairing so the API records
// the bridge and provisions/pushes TLS on the spot, rather than waiting for the
// first loop tick. Best effort — Send logs and swallows any failure.
func SendNow(cfg config.Config) {
	logx.Info("heartbeat", "sending initial heartbeat after pairing")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	url := httpx.NormalizeURL(cfg.APIBaseURL, "/bridges/heartbeat")
	_ = Send(ctx, cfg, httpx.Client(10*time.Second), url, nil)
}

// Send posts one heartbeat to url, run right after a file-sync attempt: syncErr
// is that attempt's outcome (nil = success), forwarded so the API sees sync
// status. cfg is reloaded so a `configure` edit or changed LAN IP is reflected.
func Send(ctx context.Context, cfg config.Config, client *http.Client, url string, syncErr error) Answer {
	// Reload so a `configure` edit or changed LAN IP is reflected; fall back to
	// the caller's snapshot on error.
	live := config.LoadOr(cfg)
	u := pair.GuessLANURLs(live)
	b := beat{
		Version:               live.Version,
		BridgeBaseURL:         u.BridgeBaseURL,
		BridgeMdnsBaseURL:     u.BridgeMdnsBaseURL,
		BridgeTlsBaseURL:      u.BridgeTLSBaseURL,
		CurrentTlsFingerprint: tlsx.Fingerprint(config.Dir()),
		HasSigningKey:         live.SigningKey != "",
		OS:                    runtime.GOOS,
		Arch:                  config.ReleaseArch(),
		Protocol:              config.Protocol,
		Features:              live.ComputerFeatures(),
		Paused:                live.RemotePaused,
		Capabilities:          config.Capabilities(),
		RunAs:                 live.SessionsRunAs(),
		SessionNotices:        live.SessionNotices(),
		LogSources:            live.ListedLogSources(),
	}
	syncMu.Lock()
	status := updateStatus
	syncMu.Unlock()
	if status != nil {
		if st := status(); st.Phase != selfupdate.PhaseIdle {
			b.Update = &st
		}
	}
	for i, f := range live.Folders {
		bf := beatFolder{Name: f.Name, Path: f.Path}
		if info, err := os.Stat(f.Path); err == nil && info.IsDir() {
			free := platform.Current().DiskFreeBytes(f.Path)
			bf.FreeBytes = &free
			if b.FreeBytes == nil || free < *b.FreeBytes {
				b.FreeBytes = &free
			}
		}
		if i == 0 {
			b.DataRoot = f.Path
		}
		b.Folders = append(b.Folders, bf)
	}
	if files, skipped, ok := watch.LastScan(); ok {
		b.Files, b.Skipped = &files, &skipped
	}
	if host, err := os.Hostname(); err == nil {
		b.Hostname = host
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
		return Answer{}
	}
	req.Header.Set("Content-Type", "application/json")
	httpx.SetAuth(req, httpx.Auth{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret})

	start := time.Now()
	res, err := client.Do(req)
	took := time.Since(start).Round(time.Millisecond)
	if err != nil {
		logx.Warn("heartbeat", "send failed: %v (took %s)", err, took)
		return Answer{}
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
		// Reload-modify-save: touch only what the answer carries so we never
		// revert a field changed on disk with this snapshot.
		if c, lerr := config.Load(); lerr == nil {
			c.LastHeartbeatAt = time.Now().UTC().Format(time.RFC3339)
			if r.SigningKey != "" && r.SigningKey != c.SigningKey {
				c.SigningKey = r.SigningKey
				logx.Ok("heartbeat", "received the key that checks download links")
			}
			if len(r.AllowedOrigin) > 0 && !slices.Equal(r.AllowedOrigin, c.AllowedOrigin) {
				c.AllowedOrigin = r.AllowedOrigin
				logx.Info("heartbeat", "allowed origins now %v", r.AllowedOrigin)
			}
			if r.PortalFeatures != nil {
				if next, changed := c.WithPortalFeatures(r.PortalFeatures); changed {
					c = next
					logx.Info("heartbeat", "ViaVeritas's feature switches changed")
				}
			}
			if serr := config.Save(c); serr != nil {
				logx.Warn("heartbeat", "could not persist the heartbeat's answer: %v", serr)
			}
		}
		return Answer{OK: true, Update: r.Update}
	}
	logx.Warn("heartbeat", "non-2xx response (status=%d, took %s)", res.StatusCode, took)
	return Answer{}
}

// applyCertPush stores a renewed cert when the server returns one. The server
// only sends one when our fingerprint differs, but tlsx.Store double-checks so a
// corrupt/duplicate response is a no-op rather than a needless write + listener
// restart (bridgesrv relaunches on the cert.pem mtime change). A TLSDomain and
// Subdomain shipped alongside are persisted (reload-modify-save) so a zone
// rotation, or a bridge paired before TLS was on, needs no re-pair.
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
	if c, lerr := config.Load(); lerr == nil {
		changed := false
		if r.TLSDomain != "" && c.TLSDomain != r.TLSDomain {
			c.TLSDomain, changed = r.TLSDomain, true
		}
		if r.Subdomain != "" && c.Subdomain != r.Subdomain {
			c.Subdomain, changed = r.Subdomain, true
		}
		if changed {
			if serr := config.Save(c); serr != nil {
				logx.Warn("heartbeat", "could not persist the TLS hostname: %v", serr)
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
