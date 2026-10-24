// Package logx is a small pretty leveled logger for the bridge. Format:
// "15:04:05.000  LEVEL  [component]  message"; color when stdout is a TTY and
// NO_COLOR is unset; Debug gated by BRIDGE_DEBUG.
package logx

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type level int

const (
	lvlDebug level = iota
	lvlInfo
	lvlOk
	lvlWarn
	lvlError
)

const (
	cReset  = "\x1b[0m"
	cDim    = "\x1b[2m"
	cBlue   = "\x1b[34m"
	cGreen  = "\x1b[32m"
	cYellow = "\x1b[33m"
	cRed    = "\x1b[31m"
	cMag    = "\x1b[35m"
	cCyan   = "\x1b[36m"
	cBold   = "\x1b[1m"
)

var (
	mu        sync.Mutex
	out       io.Writer = os.Stdout
	colorOn             = detectColor()
	debugOn             = os.Getenv("BRIDGE_DEBUG") != ""
	compWidth           = 11
)

func detectColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func paint(s, color string) string {
	if !colorOn {
		return s
	}
	return color + s + cReset
}

func levelTag(l level) string {
	switch l {
	case lvlDebug:
		return paint("DEBUG", cMag)
	case lvlInfo:
		return paint("INFO ", cBlue)
	case lvlOk:
		return paint("OK   ", cGreen)
	case lvlWarn:
		return paint("WARN ", cYellow)
	case lvlError:
		return paint("ERROR", cRed)
	}
	return "?????"
}

func emit(l level, comp, format string, args ...any) {
	if l == lvlDebug && !debugOn {
		return
	}
	ts := time.Now().Format("15:04:05.000")
	tag := "[" + comp + "]"
	if pad := compWidth - len(tag); pad > 0 {
		tag = tag + strings.Repeat(" ", pad)
	}
	msg := fmt.Sprintf(format, args...)

	line := fmt.Sprintf("%s  %s  %s  %s\n",
		paint(ts, cDim),
		levelTag(l),
		paint(tag, cCyan),
		msg,
	)

	mu.Lock()
	_, _ = io.WriteString(out, line)
	mu.Unlock()

	// Fan the plain formatted line to an optional extra sink (the socket log
	// forwarder). Called outside mu; the sink must not block.
	if s := logSink.Load(); s != nil {
		(*s)(line)
	}
}

// logSink is an optional extra destination for every emitted line, installed via
// SetSink. Used by internal/apisocket to stream logs to ViaVeritas.
var logSink atomic.Pointer[func(string)]

// SetSink installs fn as an extra destination for every emitted line (nil to
// remove). fn is called from emit and must not block — the socket forwarder
// hands the line to a buffered channel and returns immediately.
func SetSink(fn func(string)) {
	if fn == nil {
		logSink.Store(nil)
		return
	}
	logSink.Store(&fn)
}

// bestEffortWriter swallows write errors so a dead writer (e.g. os.Stdout on a
// Windows service with no console) cannot abort io.MultiWriter before the file.
type bestEffortWriter struct{ w io.Writer }

func (b bestEffortWriter) Write(p []byte) (int, error) {
	_, _ = b.w.Write(p)
	return len(p), nil
}

// EnableFileLogging tees log output to a size-rotating file at path (plus stdout)
// for a durable, tailable log on every platform. Call once at startup; on failure
// it leaves output unchanged and returns the error.
func EnableFileLogging(path string) error {
	rf := newRotatingFile(path, 10<<20, 3) // 10 MiB x 3 backups (~40 MiB cap)
	// Probe write access now so a permissions problem surfaces immediately.
	if _, err := rf.Write(nil); err != nil {
		return err
	}
	mu.Lock()
	// Wrap each writer so a failing one (a Windows service has no console, so
	// os.Stdout errors) can't stop MultiWriter from reaching the other.
	out = io.MultiWriter(bestEffortWriter{os.Stdout}, bestEffortWriter{rf})
	// The file is read as plain lines (the dashboard, `bridge logs`). A service
	// has no terminal, though its stdout (/dev/null) passes for one.
	colorOn = false
	mu.Unlock()
	return nil
}

// Info logs an informational event.
func Info(comp, format string, args ...any) { emit(lvlInfo, comp, format, args...) }

// Ok logs a successful event (green).
func Ok(comp, format string, args ...any) { emit(lvlOk, comp, format, args...) }

// Warn logs a recoverable problem.
func Warn(comp, format string, args ...any) { emit(lvlWarn, comp, format, args...) }

// Error logs a hard failure.
func Error(comp, format string, args ...any) { emit(lvlError, comp, format, args...) }

// Debug logs verbose diagnostic output; only printed when BRIDGE_DEBUG is set.
func Debug(comp, format string, args ...any) { emit(lvlDebug, comp, format, args...) }

// Styling helpers for non-log pretty output (e.g. the `status` report). They
// honor the same TTY / NO_COLOR detection, so piped output stays plain.

// Heading styles a section heading like "[pairing]" (bold cyan).
func Heading(s string) string { return paint(s, cBold+cCyan) }

// Comment styles a muted comment line (dim).
func Comment(s string) string { return paint(s, cDim) }

// Key styles a key name (green).
func Key(s string) string { return paint(s, cGreen) }

// Str styles a quoted string value (yellow).
func Str(s string) string { return paint(s, cYellow) }

// Scalar styles a bare scalar like a bool/number (magenta).
func Scalar(s string) string { return paint(s, cMag) }
