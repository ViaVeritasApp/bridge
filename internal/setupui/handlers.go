package setupui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"bridge/internal/config"
	"bridge/internal/pair"
	"bridge/internal/watch"
)

func (s *srv) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg, _ := config.Load() // Defaults on miss; that's fine for status display.
	urls := pair.GuessLANURLs(cfg)
	// "Installed" means a config exists and a bridge port is chosen — there's
	// no external binary to look for now that dufs is gone.
	resp := map[string]any{
		"paired":           cfg.BridgeID != "" && cfg.ClientID != "" && cfg.ClientSecret != "",
		"bridgeId":         cfg.BridgeID,
		"dataRoot":         cfg.DataDir(),
		"lanUrl":           urls.BridgeBaseURL,
		"mdnsUrl":          urls.BridgeMdnsBaseURL,
		"tlsUrl":           urls.BridgeTLSBaseURL,
		"apiBaseUrl":       config.Defaults().APIBaseURL,
		"serviceInstalled": cfg.BridgePort > 0,
		"suggestedRoot":    suggestedRoot(),
		// Current configure values to prefill the advanced section
		// (0/empty ⇒ the field shows its auto/default placeholder).
		"bridgePort":    cfg.BridgePort,
		"scanInterval":  cfg.ScanIntervalSeconds,
		"includeHidden": cfg.IncludeHidden,
		"logDir":        cfg.LogDir(),
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *srv) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct{ Parent, Name string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || strings.ContainsAny(name, `/\:*?"<>|`) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "that name can't be used for a folder"})
		return
	}
	parent, err := filepath.Abs(req.Parent)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad location"})
		return
	}
	target := filepath.Join(parent, name)
	// 0755: this folder is meant to be served by the bridge over the LAN.
	if err := os.MkdirAll(target, 0o755); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": target})
}

// handlePreview reports what the next sync would upload without contacting the
// API (same as the `preview` command), for the wizard's step 2. Response is
// newline-delimited JSON: zero or more {"scanned":N} progress lines, then a
// final {"result":{…}} — or {"error":"…"} on failure. Errors are delivered
// in-band because the HTTP status is committed once the first byte is written.
func (s *srv) handlePreview(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		// No config yet just means "nothing configured to preview".
		cfg = config.Defaults()
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)

	var last time.Time
	plan, perr := watch.PreviewProgress(r.Context(), cfg, func(scanned int) {
		// Throttle network writes; the first call always passes (zero time).
		if time.Since(last) < 120*time.Millisecond {
			return
		}
		last = time.Now()
		_ = enc.Encode(map[string]int{"scanned": scanned})
		if flusher != nil {
			flusher.Flush()
		}
	})
	if perr != nil {
		_ = enc.Encode(map[string]string{"error": perr.Error()})
		return
	}
	_ = enc.Encode(map[string]any{"result": map[string]any{
		"mode":       plan.Mode,
		"reason":     plan.Reason,
		"dataRoot":   plan.DataRoot,
		"indexFound": plan.IndexFound,
		"indexed":    plan.Indexed,
		"skipped":    plan.Skipped,
		"created":    plan.Created,
		"modified":   plan.Modified,
		"deleted":    plan.Deleted,
		"total":      plan.Total,
		"scanTook":   plan.ScanTook.String(),
	}})
}

// handleResync clears index.txt (via the `resync` subcommand, elevation
// inherited from setup) so the service re-sends every file on its next scan.
func (s *srv) handleResync(w http.ResponseWriter, r *http.Request) {
	s.runSelf(w, r, "resync")
}

func (s *srv) handleConfigure(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DataRoot      string
		BridgePort    int
		ScanInterval  int
		IncludeHidden *bool   // nil = leave unchanged; non-nil = set explicitly
		LogDir        *string // nil = leave unchanged; "" = reset to default
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.DataRoot) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "please choose a folder first"})
		return
	}

	// Only data-root is mandatory; other flags are forwarded to `configure`
	// as set, reusing runConfigure's validation and random-port fallback.
	args := []string{"configure", "--data-root", req.DataRoot}
	if req.BridgePort > 0 {
		args = append(args, "--bridge-port", strconv.Itoa(req.BridgePort))
	}
	if req.ScanInterval > 0 {
		args = append(args, "--scan-interval", strconv.Itoa(req.ScanInterval))
	}
	if req.IncludeHidden != nil {
		args = append(args, fmt.Sprintf("--include-hidden=%t", *req.IncludeHidden))
	}
	if req.LogDir != nil {
		args = append(args, "--log-dir", *req.LogDir)
	}
	s.runSelf(w, r, args...)
}

func (s *srv) handleInstall(w http.ResponseWriter, r *http.Request) {
	s.runSelf(w, r, "install")
}

func (s *srv) handleUninstall(w http.ResponseWriter, r *http.Request) {
	s.runSelf(w, r, "uninstall")
}

func (s *srv) handlePair(w http.ResponseWriter, r *http.Request) {
	var req struct{ Code, APIURL string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	code := strings.TrimSpace(req.Code)
	apiURL := strings.TrimSpace(req.APIURL)
	if code == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enter your pairing code"})
		return
	}
	if apiURL == "" {
		apiURL = config.Defaults().APIBaseURL
	}
	s.runSelf(w, r, "pair", code, "--api-url", apiURL)
}
