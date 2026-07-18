// Package tlsx owns the bridge's on-disk TLS material (cert.pem/key.pem/meta.json
// under <cfgDir>/tls/) and the *tls.Config that serves HTTPS, storing renewed
// per-bridge wildcard certs so bridgesrv can reload them without restarting the
// listener. Files are written key → cert → meta so a half-written pair is never
// visible to Loaded() and mtime watchers fire only once both PEMs exist.
package tlsx

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bridge/internal/utils"
)

// PublicHost returns the IP-encoded hostname the API issues certs for, e.g.
// PublicHost("192.168.1.50","abc123hex","lan.viaveritas.app") →
// "192-168-1-50.abc123hex.lan.viaveritas.app". The wildcard *.<subdomain>.<zone>
// covers any IP in the left-most label without re-issuance.
func PublicHost(ip, subdomain, zone string) string {
	return strings.ReplaceAll(ip, ".", "-") + "." + subdomain + "." + zone
}

func tlsDir(dir string) string  { return filepath.Join(dir, "tls") }
func certPath(dir string) string { return filepath.Join(tlsDir(dir), "cert.pem") }
func keyPath(dir string) string  { return filepath.Join(tlsDir(dir), "key.pem") }
func metaPath(dir string) string { return filepath.Join(tlsDir(dir), "meta.json") }

type meta struct {
	Fingerprint string `json:"fingerprint"`
}

// Loaded reports whether both cert.pem and key.pem exist and returns their
// paths. Missing meta.json is fine — fingerprint is treated as "" (forces the
// next heartbeat to claim a fresh cert).
func Loaded(dir string) (certFile, keyFile string, ok bool) {
	cf, kf := certPath(dir), keyPath(dir)
	if _, err := os.Stat(cf); err != nil {
		return "", "", false
	}
	if _, err := os.Stat(kf); err != nil {
		return "", "", false
	}
	return cf, kf, true
}

// Fingerprint returns the stored cert fingerprint, or "" if absent. Sent in
// every heartbeat so the server can skip resending a matching cert.
func Fingerprint(dir string) string {
	b, err := os.ReadFile(metaPath(dir))
	if err != nil {
		return ""
	}
	var m meta
	if err := json.Unmarshal(b, &m); err != nil {
		return ""
	}
	return m.Fingerprint
}

// CertMtime returns cert.pem's mtime, or zero if absent. Supervisors poll this
// to detect a rotated cert without reading the PEM body every tick.
func CertMtime(dir string) time.Time {
	st, err := os.Stat(certPath(dir))
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// Store writes a new keypair + fingerprint atomically. changed=false means the
// on-disk fingerprint already matched and nothing was rewritten.
//
// Write order is key → cert → meta so Loaded only sees the new pair after both
// files exist, and mtime watchers fire only once key.pem is in place.
func Store(dir, certPEM, keyPEM, fingerprint string) (bool, error) {
	if certPEM == "" || keyPEM == "" || fingerprint == "" {
		return false, errors.New("tlsx.Store: cert/key/fingerprint must be non-empty")
	}
	if _, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM)); err != nil {
		// Validate before clobbering the on-disk pair — a malformed push must
		// not break a bridge currently serving HTTPS.
		return false, fmt.Errorf("tlsx.Store: invalid keypair: %w", err)
	}
	if Fingerprint(dir) == fingerprint {
		if _, _, ok := Loaded(dir); ok {
			return false, nil
		}
	}
	if err := os.MkdirAll(tlsDir(dir), 0o700); err != nil {
		return false, err
	}
	if err := utils.AtomicWrite(keyPath(dir), []byte(keyPEM), 0o600); err != nil {
		return false, fmt.Errorf("write key.pem: %w", err)
	}
	if err := utils.AtomicWrite(certPath(dir), []byte(certPEM), 0o644); err != nil {
		return false, fmt.Errorf("write cert.pem: %w", err)
	}
	mb, _ := json.Marshal(meta{Fingerprint: fingerprint})
	if err := utils.AtomicWrite(metaPath(dir), mb, 0o644); err != nil {
		return false, fmt.Errorf("write meta.json: %w", err)
	}
	return true, nil
}

// TLSConfig returns a *tls.Config whose GetCertificate lazily reloads the
// keypair when cert.pem's mtime advances — the in-place renewal path that keeps
// bridgesrv up while the cert rotates. HTTP↔HTTPS transitions are handled by
// bridgesrv.watchTLSState.
func TLSConfig(dir string) *tls.Config {
	c := &certCache{dir: dir}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return c.get()
		},
	}
}

type certCache struct {
	dir   string
	mu    sync.Mutex
	mtime time.Time
	cert  *tls.Certificate
}

func (c *certCache) get() (*tls.Certificate, error) {
	cf, kf, ok := Loaded(c.dir)
	if !ok {
		return nil, errors.New("tlsx: no cert on disk")
	}
	st, err := os.Stat(cf)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cert != nil && !st.ModTime().After(c.mtime) {
		return c.cert, nil
	}
	pair, err := tls.LoadX509KeyPair(cf, kf)
	if err != nil {
		return nil, err
	}
	c.cert = &pair
	c.mtime = st.ModTime()
	return c.cert, nil
}
