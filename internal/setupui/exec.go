package setupui

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"bridge/internal/platform"
)

// suggestedRoot is a friendly starting directory for the folder browser.
func suggestedRoot() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return platform.Current().FilesystemRoot()
}

// runSelf re-executes this binary with the given subcommand. `setup` is
// elevated, so the child inherits root/Administrator to do the privileged work.
// Combined output feeds the wizard's activity log; NO_COLOR strips ANSI.
func (s *srv) runSelf(w http.ResponseWriter, r *http.Request, args ...string) {
	exe, err := os.Executable()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "can't locate the program"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	out, runErr := cmd.CombinedOutput()
	text := strings.TrimRight(string(out), "\n")

	if runErr != nil {
		msg := lastMeaningfulLine(text)
		if msg == "" {
			msg = runErr.Error()
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error":  msg,
			"output": text,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"output": text})
}

// lastMeaningfulLine pulls the most useful line out of a failed command's
// output for the inline error banner (the log card still shows everything).
func lastMeaningfulLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		if idx := strings.LastIndex(l, "  "); idx >= 0 && strings.Contains(l, "ERROR") {
			l = strings.TrimSpace(l[idx:])
		}
		return l
	}
	return ""
}
