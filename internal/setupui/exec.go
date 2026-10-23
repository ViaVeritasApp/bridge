package setupui

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"bridge/internal/config"

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
	recordAction(args, text, runErr)

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

// recordAction writes what an action ran and printed into the bridge's log, so
// Activity (and `bridge logs`) shows it beside the service's own lines. It
// appends: the service, writing the same file, only ever appends too.
func recordAction(args []string, output string, runErr error) {
	cfg, _ := config.Load()
	path := cfg.LogFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	line := func(level, msg string) string {
		return fmt.Sprintf("%s  %-6s %-12s %s\n", time.Now().Format("15:04:05.000"), level, "[setup]", msg)
	}
	var b strings.Builder
	b.WriteString(line("INFO", "bridge "+strings.Join(args, " ")))
	if output != "" {
		b.WriteString(output + "\n")
	}
	if runErr != nil {
		b.WriteString(line("ERROR", fmt.Sprintf("bridge %s failed: %v", args[0], runErr)))
	}
	_, _ = f.WriteString(b.String())
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
