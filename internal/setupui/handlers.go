package setupui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"bridge/internal/config"
	"bridge/internal/control"
	"bridge/internal/pair"
	"bridge/internal/platform"
	"bridge/internal/watch"
)

func (s *srv) serviceState() string {
	if s.hooks.ServiceState == nil {
		return "unknown"
	}
	return s.hooks.ServiceState()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (s *srv) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg, _ := config.Load() // Defaults on miss; that's fine for status display.
	urls := pair.GuessLANURLs(cfg)
	installed := config.InstalledVersion()
	// Installed: the service manager knows the service and its program is in the
	// install folder; elsewhere: it knows one that runs from another place (a
	// bridge set up before installs copied themselves), which installing moves.
	service := s.serviceState()
	installState := "not_installed"
	if service == "running" || service == "stopped" {
		installState = "installed"
		if !fileExists(config.InstalledBinary()) {
			installState = "elsewhere"
		}
	}
	isInstalled := false
	if exe, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		if a, aerr := os.Stat(exe); aerr == nil {
			if b, berr := os.Stat(config.InstalledBinary()); berr == nil {
				isInstalled = os.SameFile(a, b)
			}
		}
	}
	// "Installed" means a config exists and a bridge port is chosen — there's
	// no external binary to look for now that dufs is gone.
	resp := map[string]any{
		"paired":           cfg.BridgeID != "" && cfg.ClientID != "" && cfg.ClientSecret != "",
		"bridgeId":         cfg.BridgeID,
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
		// Where it installs, and how this copy stands to what is there.
		"installDir":       config.InstallDir(),
		"installedBinary":  config.InstalledBinary(),
		"version":          config.DefaultVersion,
		"installedVersion": installed,
		"programInstalled": installed != "" || fileExists(config.InstalledBinary()),
		"isInstalledCopy":  isInstalled,
		"newerInstalled":   !isInstalled && config.CompareVersions(installed, config.DefaultVersion) > 0,
		"updateAvailable":  !isInstalled && installed != "" && config.CompareVersions(config.DefaultVersion, installed) > 0,
		// For the dashboard.
		"service":         service,
		"installState":    installState,
		"lastHeartbeatAt": cfg.LastHeartbeatAt,
		"socketReady":     cfg.BridgeID != "" && cfg.OrganizationID != "",
		"signingKey":      cfg.SigningKey != "",
		"logFile":         cfg.LogFile(),
		"os":              runtime.GOOS,
		"filesOn":         cfg.Effective(config.FeatureFiles),
		"filesHere":       cfg.Allows(config.FeatureFiles),
	}
	// The running service's own word on its socket, when it answers.
	if st, err := control.Ask(); err == nil {
		resp["socket"], resp["openStreams"] = st.Socket, len(st.Streams)
	}
	n, at, perFolder, indexed := s.index.read()
	if indexed {
		resp["indexed"], resp["indexedAt"] = n, at.UTC().Format(time.RFC3339)
	}
	// Each folder as it stands, and the least free space among them: the disk that fills first.
	folders := make([]map[string]any, 0, len(cfg.Folders))
	protected := false
	var leastFree *int64
	for _, f := range cfg.Folders {
		item := map[string]any{"id": f.ID, "name": f.Name, "path": f.Path, "files": perFolder[f.Name], "protected": platform.Protected(f.Path)}
		if info, err := os.Stat(f.Path); err == nil && info.IsDir() {
			free := platform.Current().DiskFreeBytes(f.Path)
			item["available"], item["freeBytes"] = true, free
			if leastFree == nil || free < *leastFree {
				leastFree = &free
			}
		} else {
			item["available"] = false
		}
		protected = protected || platform.Protected(f.Path)
		folders = append(folders, item)
	}
	resp["folders"], resp["protectedRoot"] = folders, protected
	if leastFree != nil {
		resp["freeBytes"] = *leastFree
	}
	if until, ok := certUntil(config.Dir()); ok {
		resp["tlsUntil"] = until.UTC().Format(time.RFC3339)
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
		"mode":        plan.Mode,
		"reason":      plan.Reason,
		"folders":     plan.Folders,
		"unavailable": plan.Unavailable,
		"indexFound":  plan.IndexFound,
		"indexed":     plan.Indexed,
		"skipped":     plan.Skipped,
		"created":     plan.Created,
		"modified":    plan.Modified,
		"deleted":     plan.Deleted,
		"total":       plan.Total,
		"scanTook":    plan.ScanTook.String(),
	}})
}

// handleResync clears index.txt (via the `resync` subcommand, elevation
// inherited from setup) so the service re-sends every file on its next scan.
func (s *srv) handleResync(w http.ResponseWriter, r *http.Request) {
	s.runSelf(w, r, "resync")
}

func (s *srv) handleConfigure(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BridgePort    int
		ScanInterval  int
		IncludeHidden *bool   // nil = leave unchanged; non-nil = set explicitly
		LogDir        *string // nil = leave unchanged; "" = reset to default
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}

	// Flags are forwarded to `configure` as set, reusing runConfigure's
	// validation and random-port fallback.
	args := []string{"configure"}
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
		args = append(args, "--log-dir="+*req.LogDir)
	}
	s.runSelf(w, r, args...)
}

// The folders it syncs, changed through the `folder` subcommand. "--" ends its
// flags, so a path or a name starting with a dash is taken as one.
func (s *srv) handleFolderAdd(w http.ResponseWriter, r *http.Request) {
	var req struct{ Path, Name string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Path) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "choose a folder"})
		return
	}
	args := []string{"folder", "add"}
	if name := strings.TrimSpace(req.Name); name != "" {
		args = append(args, "--name="+name)
	}
	s.runSelf(w, r, append(args, "--", req.Path)...)
}

func (s *srv) handleFolderRename(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name, NewName string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || strings.TrimSpace(req.NewName) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "give it a name"})
		return
	}
	s.runSelf(w, r, "folder", "rename", "--", req.Name, strings.TrimSpace(req.NewName))
}

func (s *srv) handleFolderRemove(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	s.runSelf(w, r, "folder", "remove", "--", req.Name)
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
