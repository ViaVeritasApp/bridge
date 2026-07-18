// Package bridgesrv is the bridge's embedded HTTP API (the /files endpoints
// that replaced dufs, plus /healthz). It's intentionally unauthenticated at
// the handler layer: file endpoints forward the caller's Authorization to the
// API (the source of truth), and Get validates an HMAC the API issued with the
// shared per-bridge token.
package bridgesrv

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"bridge/internal/config"
	"bridge/internal/files"
	"bridge/internal/httpx"
	"bridge/internal/logx"
	"bridge/internal/platform"
	"bridge/internal/tlsx"
)

// Supervise runs Serve in a loop, relaunching on exit (typically a TLS state
// change that self-cancels Serve to rebuild with fresh cfg.TLSEnabled()).
// Backoff 1s → 30s, reset after >60s healthy. Returns only when ctx cancels.
func Supervise(ctx context.Context, cfg config.Config) error {
	const (
		minBackoff    = 1 * time.Second
		maxBackoff    = 30 * time.Second
		healthyWindow = 60 * time.Second
	)
	backoff := minBackoff
	for ctx.Err() == nil {
		start := time.Now()
		live := config.LoadOr(cfg)
		err := Serve(ctx, live)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		uptime := time.Since(start).Round(time.Millisecond)
		if uptime > healthyWindow {
			backoff = minBackoff
		}
		if err != nil {
			logx.Warn("bridgesrv", "exited after %s (err=%v); restarting in %s", uptime, err, backoff)
		} else {
			logx.Info("bridgesrv", "exited after %s; restarting in %s", uptime, backoff)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
	return ctx.Err()
}

// Serve runs the bridge HTTP API on cfg.BridgeAddr() until ctx cancels or the
// TLS state changes. With TLS on, GetCertificate picks up in-place renewals
// without restarting; an HTTP↔HTTPS transition needs a fresh http.Server, so
// watchTLSState cancels runCtx and Supervise relaunches.
func Serve(ctx context.Context, cfg config.Config) error {
	addr := cfg.BridgeAddr()
	if addr == "" {
		logx.Warn("bridgesrv", "no bridge port configured; HTTP API disabled (run `bridge configure --bridge-port <n>`)")
		return nil
	}
	// 30s covers the API authorize round-trip, not large uploads (those stream
	// through HandleUpload without an outer deadline).
	client := httpx.Client(30 * time.Second)

	// Non-upload handlers get a 45s budget so a wedged data root/API surfaces an
	// error rather than hanging. Upload is unbounded here — governed by the
	// upload gate's LockDuration(size) and the TCP connection's life.
	const handlerTimeout = 45 * time.Second
	bounded := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			c, cancel := context.WithTimeout(r.Context(), handlerTimeout)
			defer cancel()
			h(w, r.WithContext(c))
		}
	}

	mux := http.NewServeMux()
	// All four file endpoints share one path; the method dispatcher routes them,
	// so CORS preflight advertises just one resource.
	mux.HandleFunc("/files", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			// No outer timeout — uploads can take minutes; the upload gate caps
			// lock duration.
			files.HandleUpload(w, r, cfg, client)
		case http.MethodGet, http.MethodHead:
			bounded(func(w http.ResponseWriter, r *http.Request) {
				files.HandleGet(w, r, cfg)
			})(w, r)
		case http.MethodDelete:
			bounded(func(w http.ResponseWriter, r *http.Request) {
				files.HandleDelete(w, r, cfg, client)
			})(w, r)
		case http.MethodPatch:
			bounded(func(w http.ResponseWriter, r *http.Request) {
				files.HandleMove(w, r, cfg, client)
			})(w, r)
		default:
			w.Header().Set("Allow", "GET, HEAD, POST, PATCH, DELETE, OPTIONS")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	// /healthz (k8s-style) and /health (CRM/monitoring alias) share one handler.
	mux.HandleFunc("/healthz", bounded(handleHealthz(cfg)))
	mux.HandleFunc("/health", bounded(handleHealthz(cfg)))

	tlsOn := cfg.TLSEnabled()
	srv := &http.Server{
		Addr:              addr,
		Handler:           withCORS(cfg, mux),
		ReadHeaderTimeout: 10 * time.Second,
		// ReadTimeout/WriteTimeout unset: POST /files can take minutes; per-handler
		// context.WithTimeout caps the smaller endpoints and ServeContent handles
		// Range downloads. IdleTimeout still reaps idle keep-alive connections.
		IdleTimeout: 120 * time.Second,
	}
	if tlsOn {
		srv.TLSConfig = tlsx.TLSConfig(config.Dir())
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	go watchTLSState(runCtx, cancelRun)

	errc := make(chan error, 1)
	go func() {
		if tlsOn {
			logx.Ok("bridgesrv", "listening on https://%s (/files, /healthz)", addr)
			errc <- srv.ListenAndServeTLS("", "") // cert via GetCertificate
			return
		}
		logx.Ok("bridgesrv", "listening on http://%s (/files, /healthz)", addr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case <-runCtx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		if ctx.Err() != nil {
			logx.Info("bridgesrv", "HTTP API stopped (ctx cancelled)")
		} else {
			logx.Info("bridgesrv", "HTTP API stopping to pick up TLS state change")
		}
		return nil
	case err := <-errc:
		if err != nil && err != http.ErrServerClosed {
			logx.Error("bridgesrv", "HTTP API exited: %v", err)
			return err
		}
		return nil
	}
}

// watchTLSState cancels runCtx when the on-disk cert fingerprint changes —
// covering HTTP↔HTTPS transitions and cert swaps. Same-fingerprint renewals
// need no restart; GetCertificate handles those.
func watchTLSState(ctx context.Context, cancel context.CancelFunc) {
	const interval = 30 * time.Second
	initial := tlsx.Fingerprint(config.Dir())
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if tlsx.Fingerprint(config.Dir()) != initial {
				logx.Info("bridgesrv", "TLS state changed on disk; restarting listener")
				cancel()
				return
			}
		}
	}
}

// withCORS lets the CRM call the bridge from a browser. It reflects only origins
// in cfg.AllowedOrigin (credentialed); any other origin gets no CORS headers, so
// a browser can't read file data cross-origin (curl/server-to-server unaffected).
// There is deliberately no wildcard `Allow-Origin: *` fallback. Vary: Origin is
// always set; cfg is reloaded per request so AllowedOrigin edits apply live.
func withCORS(cfg config.Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		origin := r.Header.Get("Origin")
		if origin != "" && originAllowed(origin, config.LoadOr(cfg).AllowedOrigin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Accept, Authorization, Content-Type, Origin, X-Requested-With")
			w.Header().Set("Access-Control-Expose-Headers", "X-Bridge-Path, X-Bridge-Name, X-Bridge-Filetype, X-Bridge-Device-File-Ino, X-Bridge-File-Id")
			w.Header().Set("Access-Control-Max-Age", "86400")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originAllowed reports whether origin is in the allowlist (trimmed exact
// match, no wildcard/subdomain logic). Empty allowlist disables reflection.
func originAllowed(origin string, allowed []string) bool {
	if len(allowed) == 0 {
		return false
	}
	for _, a := range allowed {
		if strings.TrimSpace(a) == origin {
			return true
		}
	}
	return false
}

// handleHealthz is the readiness probe; any 200 means the listener is up and
// the data root is readable.
func handleHealthz(cfg config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD, OPTIONS")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		c := config.LoadOr(cfg)
		root := c.DataDir()
		resp := map[string]any{
			"ok":        true,
			"version":   c.Version,
			"bridgeId":  c.BridgeID,
			"dataRoot":  root,
			"freeBytes": platform.Current().DiskFreeBytes(root),
			"tls":       c.TLSEnabled(),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}
}
