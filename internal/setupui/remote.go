package setupui

import (
	"encoding/json"
	"net/http"
	"runtime"
	"strings"

	"bridge/internal/config"
	"bridge/internal/control"
	"bridge/internal/streams"
)

// remoteFeature is a feature as the Features view shows it.
type remoteFeature struct {
	Key    config.Feature `json:"key"`
	Remote bool           `json:"remote"`
	Here   bool           `json:"here"`
	Portal bool           `json:"portal"`
	Paused bool           `json:"paused"`
	Runs   bool           `json:"runs"`
}

// handleRemote is what this bridge does and what ViaVeritas has open on it:
// its switches here and in ViaVeritas, and the streams the service holds.
func (s *srv) handleRemote(w http.ResponseWriter, _ *http.Request) {
	cfg := config.LoadOrDefaults()
	features := make([]remoteFeature, 0, len(config.Built))
	anyRemote := false
	for _, f := range config.Built {
		anyRemote = anyRemote || f.Remote()
		features = append(features, remoteFeature{
			Key: f, Remote: f.Remote(), Here: cfg.Allows(f), Portal: cfg.PortalAllows(f), Paused: cfg.Paused(f), Runs: cfg.Effective(f),
		})
	}
	updates := map[string]any{}
	resp := map[string]any{
		"features":  features,
		"paused":    cfg.RemotePaused,
		"anyRemote": anyRemote,
		"running":   false,
		"version":   config.DefaultVersion,
		"streams":   []streams.Info{},
		"updates":   updates,
		"sessions":  map[string]any{"runAs": cfg.SessionsRunAs(), "notices": cfg.SessionNotices()},
		"logSources": map[string]any{
			"sources": cfg.ListedLogSources(), "kinds": config.LogKindsHere(), "here": cfg.Allows(config.FeatureLogs),
			"bridgeLog": cfg.LogFile(), "windows": runtime.GOOS == "windows",
		},
	}
	if st, err := control.Ask(); err == nil {
		resp["running"], resp["socket"], resp["streams"], resp["serviceVersion"] = true, st.Socket, st.Streams, st.Version
		updates["status"] = st.Update
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleFeature switches a feature on this computer, through `features`.
func (s *srv) handleFeature(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string
		On  bool
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !config.Feature(req.Key).Known() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no such feature"})
		return
	}
	verb := "disable"
	if req.On {
		verb = "enable"
	}
	s.runSelf(w, r, "features", verb, req.Key)
}

// handlePause pauses or resumes all remote access, through `pause`/`resume`.
func (s *srv) handlePause(w http.ResponseWriter, r *http.Request) {
	var req struct{ Paused bool }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	if req.Paused {
		s.runSelf(w, r, "pause")
		return
	}
	s.runSelf(w, r, "resume")
}

// handleSessionEnd ends a stream ViaVeritas has open, through `sessions end`.
func (s *srv) handleSessionEnd(w http.ResponseWriter, r *http.Request) {
	var req struct{ ID string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	s.runSelf(w, r, "sessions", "end", "--", req.ID)
}

// handleUpdateNow installs the newer version ViaVeritas offers, through `updates now`.
func (s *srv) handleUpdateNow(w http.ResponseWriter, r *http.Request) {
	s.runSelf(w, r, "updates", "now")
}

// handleRunAs chooses who terminals and commands run as, through `sessions run-as`.
func (s *srv) handleRunAs(w http.ResponseWriter, r *http.Request) {
	var req struct{ RunAs string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !config.RunAs(req.RunAs).Known() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no such choice"})
		return
	}
	s.runSelf(w, r, "sessions", "run-as", req.RunAs)
}

// handleNotices chooses whether the screen says when ViaVeritas opens something, through `sessions notices`.
func (s *srv) handleNotices(w http.ResponseWriter, r *http.Request) {
	var req struct{ On bool }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	on := "off"
	if req.On {
		on = "on"
	}
	s.runSelf(w, r, "sessions", "notices", on)
}

// handleLogAdd lists a log ViaVeritas may read, through `log-sources add`.
func (s *srv) handleLogAdd(w http.ResponseWriter, r *http.Request) {
	var req struct{ Kind, Target, Name string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	// Checked here first, so a mistake is said without running anything.
	if _, _, err := (config.Config{}).WithLogSource(config.LogSource{Kind: config.LogSourceKind(req.Kind), Target: req.Target, Name: req.Name}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	args := []string{"log-sources", "add"}
	if name := strings.TrimSpace(req.Name); name != "" {
		args = append(args, "--name", name)
	}
	args = append(args, "--", req.Kind)
	if target := strings.TrimSpace(req.Target); target != "" {
		args = append(args, target)
	}
	s.runSelf(w, r, args...)
}

// handleLogRemove stops ViaVeritas reading a log, through `log-sources remove`.
func (s *srv) handleLogRemove(w http.ResponseWriter, r *http.Request) {
	var req struct{ ID string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	s.runSelf(w, r, "log-sources", "remove", "--", req.ID)
}
