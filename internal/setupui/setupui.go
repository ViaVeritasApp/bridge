// Package setupui serves a local-only web wizard that walks a non-technical
// operator through setup (choose a data folder, install the service, pair with
// ViaVeritas). Privileged steps re-execute this elevated binary's own
// subcommands; it binds 127.0.0.1 only and gates every state-changing endpoint
// behind a per-run CSRF token, so nothing off-host can drive root operations.
package setupui

import (
	"context"
	_ "embed"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"bridge/internal/logx"
	"bridge/internal/netx"
	"bridge/internal/platform"
)

//go:embed index.html
var indexHTML string

// Serve starts the setup web server, opens the operator's browser at it, and
// blocks until ctx is cancelled (Ctrl+C) or the server stops. The caller must
// do the elevation check first.
func Serve(ctx context.Context, hooks Hooks) error {
	token := newToken()
	page := strings.ReplaceAll(indexHTML, "__CSRF__", token)

	host := "127.0.0.1"
	port, err := netx.RandomFreePort(host, 7700, 7799)
	if err != nil {
		return fmt.Errorf("pick setup port: %w", err)
	}
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	url := "http://" + addr + "/"

	// Cancellable so the page (via /api/bye or the idle watchdog on tab close)
	// can stop the server too, not just the caller's Ctrl+C.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s := &srv{token: token, page: page, stop: cancel, hooks: hooks}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/status", s.guard(s.handleStatus))
	mux.HandleFunc("/api/browse", s.guard(s.handleBrowse))
	mux.HandleFunc("/api/mkdir", s.guard(s.handleMkdir))
	mux.HandleFunc("/api/preview", s.guard(s.handlePreview))
	mux.HandleFunc("/api/resync", s.guard(s.handleResync))
	mux.HandleFunc("/api/configure", s.guard(s.handleConfigure))
	mux.HandleFunc("/api/install", s.guard(s.handleInstall))
	mux.HandleFunc("/api/uninstall", s.guard(s.handleUninstall))
	mux.HandleFunc("/api/pair", s.guard(s.handlePair))
	mux.HandleFunc("/api/logs", s.guard(s.handleLogs))
	mux.HandleFunc("/api/folders/add", s.guard(s.handleFolderAdd))
	mux.HandleFunc("/api/folders/rename", s.guard(s.handleFolderRename))
	mux.HandleFunc("/api/folders/remove", s.guard(s.handleFolderRemove))
	mux.HandleFunc("/api/restart", s.guard(s.handleRestart))
	mux.HandleFunc("/api/remote", s.guard(s.handleRemote))
	mux.HandleFunc("/api/features", s.guard(s.handleFeature))
	mux.HandleFunc("/api/pause", s.guard(s.handlePause))
	mux.HandleFunc("/api/sessions/end", s.guard(s.handleSessionEnd))
	mux.HandleFunc("/api/updates/now", s.guard(s.handleUpdateNow))
	mux.HandleFunc("/api/sessions/run-as", s.guard(s.handleRunAs))
	mux.HandleFunc("/api/sessions/notices", s.guard(s.handleNotices))
	mux.HandleFunc("/api/logs/add", s.guard(s.handleLogAdd))
	mux.HandleFunc("/api/logs/remove", s.guard(s.handleLogRemove))
	// ping/bye drive auto-shutdown on browser close. They only stop this local
	// server, so loopback-only (no CSRF) is enough — sendBeacon can't set the
	// CSRF header anyway.
	mux.HandleFunc("/api/ping", s.loopback(s.handlePing))
	mux.HandleFunc("/api/bye", s.loopback(s.handleBye))

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = httpSrv.Shutdown(shutCtx)
	}()
	go s.watchdog(ctx)

	logx.Ok("setup", "setup wizard ready — open %s", url)
	fmt.Printf("\n  ViaVeritas Bridge setup is running.\n  Open this address in a browser if it didn't open automatically:\n\n      %s\n\n  Press Ctrl+C here when you're done.\n\n", url)
	platform.Current().OpenBrowser(url)

	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

type srv struct {
	token string
	page  string
	stop  context.CancelFunc

	// Browser-page liveness. seen flips true on the first /api/ping; the
	// watchdog only acts once it has, so a server whose browser never connected
	// stays up (Ctrl+C still works).
	seen     atomic.Bool
	lastSeen atomic.Int64 // UnixNano of the most recent ping
	hooks    Hooks
	index    indexCount
}

const pageCSP = "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func (s *srv) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	// Host-pin the page so a DNS-rebinding origin can't read the embedded CSRF token.
	if !hostAllowed(r) {
		http.Error(w, "local access only", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// The page is self-contained: nothing from elsewhere loads, and no other page frames it.
	w.Header().Set("Content-Security-Policy", pageCSP)
	_, _ = w.Write([]byte(s.page))
}

func (s *srv) handlePing(w http.ResponseWriter, _ *http.Request) {
	s.seen.Store(true)
	s.lastSeen.Store(time.Now().UnixNano())
	w.WriteHeader(http.StatusNoContent)
}

func (s *srv) handleBye(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
	logx.Info("setup", "setup page closed; stopping the setup server")
	if s.stop != nil {
		s.stop()
	}
}

// watchdog stops the server when the page goes silent (tab closed, close beacon
// missed). Only fires after at least one ping, so it never shuts a server whose
// browser hasn't connected yet.
func (s *srv) watchdog(ctx context.Context) {
	const idle = 60 * time.Second
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !s.seen.Load() {
				continue
			}
			last := s.lastSeen.Load()
			if last > 0 && time.Since(time.Unix(0, last)) > idle {
				logx.Info("setup", "no browser heartbeat for %s; stopping the setup server", idle)
				if s.stop != nil {
					s.stop()
				}
				return
			}
		}
	}
}
