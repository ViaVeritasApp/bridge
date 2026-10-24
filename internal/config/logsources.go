package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"
)

// LogSourceKind is where a log ViaVeritas may read comes from.
type LogSourceKind string

const (
	// LogBridge is the bridge's own log: always listed, never removed.
	LogBridge LogSourceKind = "bridge"
	// LogFile is a file, or the newest of the files a glob matches (a log that rotates).
	LogFile LogSourceKind = "file"
	// LogJournal is systemd's journal (Linux), every unit's or one's.
	LogJournal LogSourceKind = "journal"
	// LogMacOS is the unified log (macOS), everything or what a predicate keeps.
	LogMacOS LogSourceKind = "macos"
	// LogEventLog is one of Windows' event logs (System, Application…).
	LogEventLog LogSourceKind = "eventlog"
)

// BridgeLogID is the bridge's own log's id.
const BridgeLogID = "bridge"

// MaxLogSources is the longest list kept, the bridge's own log aside.
const MaxLogSources = 50

// LogSource is a log this computer lets ViaVeritas read, while logs are on.
// Target is what it reads: a file's path or a glob, a journal unit ("" for
// every unit), a unified log predicate ("" for everything), an event log's name.
// ViaVeritas names a source by its id only, never by a path.
type LogSource struct {
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	Kind   LogSourceKind `json:"kind"`
	Target string        `json:"target,omitempty"`
}

// LogKindsHere are the kinds this system can read.
func LogKindsHere() []LogSourceKind {
	switch runtime.GOOS {
	case "linux":
		return []LogSourceKind{LogFile, LogJournal}
	case "darwin":
		return []LogSourceKind{LogFile, LogMacOS}
	case "windows":
		return []LogSourceKind{LogFile, LogEventLog}
	}
	return []LogSourceKind{LogFile}
}

func (k LogSourceKind) here() bool {
	for _, it := range LogKindsHere() {
		if it == k {
			return true
		}
	}
	return false
}

// defaultLog is one of the logs a system usually has, offered while it exists here.
type defaultLog struct {
	source LogSource
	exists func() bool
}

func fileExists(path string) func() bool {
	return func() bool {
		info, err := os.Stat(path)
		return err == nil && !info.IsDir()
	}
}

func dirExists(path string) func() bool {
	return func() bool {
		info, err := os.Stat(path)
		return err == nil && info.IsDir()
	}
}

func always() bool { return true }

// defaultLogs are the most read logs of each system. Their ids stay the same,
// so ViaVeritas keeps naming them across restarts.
var defaultLogs = map[string][]defaultLog{
	"linux": {
		{LogSource{ID: "journal-system", Name: "System journal", Kind: LogJournal}, func() bool { _, err := exec.LookPath("journalctl"); return err == nil }},
		{LogSource{ID: "file-syslog", Name: "Syslog", Kind: LogFile, Target: "/var/log/syslog"}, fileExists("/var/log/syslog")},
		{LogSource{ID: "file-messages", Name: "System messages", Kind: LogFile, Target: "/var/log/messages"}, fileExists("/var/log/messages")},
		{LogSource{ID: "file-nginx-error", Name: "Nginx errors", Kind: LogFile, Target: "/var/log/nginx/error.log"}, fileExists("/var/log/nginx/error.log")},
		{LogSource{ID: "file-nginx-access", Name: "Nginx access", Kind: LogFile, Target: "/var/log/nginx/access.log"}, fileExists("/var/log/nginx/access.log")},
		{LogSource{ID: "file-apache-error", Name: "Apache errors", Kind: LogFile, Target: "/var/log/apache2/error.log"}, fileExists("/var/log/apache2/error.log")},
		{LogSource{ID: "file-apache-access", Name: "Apache access", Kind: LogFile, Target: "/var/log/apache2/access.log"}, fileExists("/var/log/apache2/access.log")},
		{LogSource{ID: "file-httpd-error", Name: "Apache errors", Kind: LogFile, Target: "/var/log/httpd/error_log"}, fileExists("/var/log/httpd/error_log")},
		{LogSource{ID: "file-httpd-access", Name: "Apache access", Kind: LogFile, Target: "/var/log/httpd/access_log"}, fileExists("/var/log/httpd/access_log")},
	},
	"darwin": {
		{LogSource{ID: "macos-system", Name: "System log", Kind: LogMacOS}, always},
		{LogSource{ID: "file-install", Name: "Installs and updates", Kind: LogFile, Target: "/var/log/install.log"}, fileExists("/var/log/install.log")},
	},
	"windows": {
		{LogSource{ID: "eventlog-system", Name: "System", Kind: LogEventLog, Target: "System"}, always},
		{LogSource{ID: "eventlog-application", Name: "Application", Kind: LogEventLog, Target: "Application"}, always},
		{LogSource{ID: "file-iis", Name: "IIS", Kind: LogFile, Target: `C:\inetpub\logs\LogFiles\W3SVC1\*.log`}, dirExists(`C:\inetpub\logs\LogFiles\W3SVC1`)},
	},
}

// DefaultLogSources are this system's usual logs that exist here: what is
// listed until someone edits the list on this computer.
func DefaultLogSources() []LogSource {
	out := []LogSource{}
	for _, d := range defaultLogs[runtime.GOOS] {
		if d.exists() {
			out = append(out, d.source)
		}
	}
	return out
}

// ConfiguredLogSources is the list beside the bridge's own: as edited here, or
// the system's usual logs while nobody has.
func (c Config) ConfiguredLogSources() []LogSource {
	if c.LogSources == nil {
		return DefaultLogSources()
	}
	return c.LogSources
}

// ListedLogSources is every log ViaVeritas may read here: the bridge's own first.
func (c Config) ListedLogSources() []LogSource {
	out := []LogSource{{ID: BridgeLogID, Name: "Bridge", Kind: LogBridge}}
	return append(out, c.ConfiguredLogSources()...)
}

// LogSourceByID is the listed source with that id.
func (c Config) LogSourceByID(id string) (LogSource, bool) {
	for _, s := range c.ListedLogSources() {
		if s.ID == id {
			return s, true
		}
	}
	return LogSource{}, false
}

// ValidateLogSource checks a source before it is kept.
func ValidateLogSource(s LogSource) error {
	if n := utf8.RuneCountInString(s.Name); n < 1 || n > 80 {
		return fmt.Errorf("a log's name is 1 to 80 characters")
	}
	if !s.Kind.here() {
		return fmt.Errorf("this computer can't read %q logs: choose one of %s", s.Kind, joinKinds(LogKindsHere()))
	}
	t := s.Target
	if strings.ContainsAny(t, "\x00\r\n") || len(t) > 1024 {
		return fmt.Errorf("%q can't be read as a log", t)
	}
	switch s.Kind {
	case LogFile:
		if !filepath.IsAbs(t) {
			return fmt.Errorf("a log file is named by its full path, or a pattern of them: %q isn't one", t)
		}
		if _, err := filepath.Match(t, ""); err != nil {
			return fmt.Errorf("%q isn't a pattern of files: %v", t, err)
		}
	case LogJournal:
		if t != "" && (strings.HasPrefix(t, "-") || len(t) > 256 || strings.Trim(t, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@._:-\\") != "") {
			return fmt.Errorf("%q isn't a systemd unit", t)
		}
	case LogEventLog:
		if t == "" || len(t) > 256 || strings.Trim(t, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 -_./") != "" {
			return fmt.Errorf("%q isn't an event log's name, such as System or Application", t)
		}
	}
	return nil
}

func joinKinds(kinds []LogSourceKind) string {
	names := make([]string, len(kinds))
	for i, k := range kinds {
		names[i] = string(k)
	}
	return strings.Join(names, ", ")
}

// DefaultLogName is what a source is called when its name isn't given.
func DefaultLogName(kind LogSourceKind, target string) string {
	switch kind {
	case LogFile:
		return filepath.Base(target)
	case LogJournal:
		if target != "" {
			return target
		}
		return "System journal"
	case LogMacOS:
		if target != "" {
			return "System log (" + truncate(target, 40) + ")"
		}
		return "System log"
	case LogEventLog:
		return target
	}
	return string(kind)
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// WithLogSource is c with s listed under a new id, and that source.
func (c Config) WithLogSource(s LogSource) (Config, LogSource, error) {
	s.Target = strings.TrimSpace(s.Target)
	if s.Kind == LogFile {
		s.Target = filepath.Clean(s.Target)
	}
	s.Name = strings.TrimSpace(s.Name)
	if s.Name == "" {
		s.Name = DefaultLogName(s.Kind, s.Target)
	}
	if err := ValidateLogSource(s); err != nil {
		return c, s, err
	}
	listed := c.ConfiguredLogSources()
	if len(listed) >= MaxLogSources {
		return c, s, fmt.Errorf("the list holds %d at most: remove one first", MaxLogSources)
	}
	for _, it := range listed {
		if it.Kind == s.Kind && it.Target == s.Target {
			return c, s, fmt.Errorf("%s is listed already, as %q", s.Target, it.Name)
		}
	}
	raw := make([]byte, 3)
	_, _ = rand.Read(raw)
	s.ID = string(s.Kind) + "-" + hex.EncodeToString(raw)
	next := make([]LogSource, 0, len(listed)+1)
	c.LogSources = append(append(next, listed...), s)
	return c, s, nil
}

// WithoutLogSource is c without the source with that id, and whether there was
// one. Taking one of the usual logs off keeps the list as edited from then on.
func (c Config) WithoutLogSource(id string) (Config, bool) {
	listed := c.ConfiguredLogSources()
	next := make([]LogSource, 0, len(listed))
	for _, it := range listed {
		if it.ID != id {
			next = append(next, it)
		}
	}
	if len(next) == len(listed) {
		return c, false
	}
	c.LogSources = next
	return c, true
}
