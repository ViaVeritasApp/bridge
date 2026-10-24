package telemetry

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
)

// Begin is the service starting: a crash's trace the run before left is kept
// as a crash, then the start, saying how the run before ended (after: clean, a
// crash, or unclean when it stopped without saying so: a power cut, a kill).
// From now on, the Go runtime writes a fatal error's trace to crash.log in dir.
func (s *Store) Begin(dir string) {
	crash := filepath.Join(dir, "crash.log")
	running := filepath.Join(dir, "running")

	after := "clean"
	if trace, err := os.ReadFile(crash); err == nil && len(strings.TrimSpace(string(trace))) > 0 {
		s.Record(Crashed, map[string]any{"trace": Redact(string(trace))})
		after = "crash"
	} else if _, err := os.Stat(running); err == nil {
		after = "unclean"
	}
	s.Record(Started, map[string]any{"after": after})
	_ = os.WriteFile(running, []byte(strconv.Itoa(os.Getpid())), 0o600)

	f, err := os.OpenFile(crash, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	// The runtime keeps its own copy of the descriptor.
	_ = debug.SetCrashOutput(f, debug.CrashOptions{})
	_ = f.Close()
}

// End is the service stopping on purpose: reason is "stopped", or "update"
// when it ends to start again as the version just put in place.
func (s *Store) End(dir, reason string) {
	s.Record(Stopped, map[string]any{"reason": reason})
	if err := os.Remove(filepath.Join(dir, "running")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
}

// A path on the computer: absolute on Unix or Windows, or a share.
var pathPattern = regexp.MustCompile(`(?:[A-Za-z]:[\\/]|\\\\|/)(?:[^\s"'<>|*?:\\/]+[\\/])+[^\s"'<>|*?:\\/]*`)

// Redact takes the paths of the computer out of a text: a Go source file keeps
// its package folder and name (where in the bridge it happened), anything else
// becomes <path>. An address on the web stays.
func Redact(text string) string {
	var out strings.Builder
	last := 0
	for _, m := range pathPattern.FindAllStringIndex(text, -1) {
		start, end := m[0], m[1]
		// "https://host/x" is the path of an address, and "bridge/internal/x.go" a
		// relative one: only a path that starts a word is the computer's.
		if start > 0 && inWord(text[start-1]) {
			continue
		}
		out.WriteString(text[last:start])
		out.WriteString(redactPath(text[start:end]))
		last = end
	}
	out.WriteString(text[last:])
	return out.String()
}

func redactPath(p string) string {
	if !strings.HasSuffix(p, ".go") {
		return "<path>"
	}
	parts := strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, "/")
}

func inWord(c byte) bool {
	return c == ':' || c == '/' || c == '\\' || c == '.' || c == '_' || c == '-' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
