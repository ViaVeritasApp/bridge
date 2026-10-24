package logs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"bridge/internal/config"
)

// run starts a program and hands each line it writes to each, until it ends or
// ctx does. A program that can't start, or ends badly before writing a line,
// is a source this computer can't read now.
func run(ctx context.Context, name string, args, env []string, each func(line []byte)) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &limited{w: &stderr, n: 4096}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return unavailable{err}
	}
	if err := cmd.Start(); err != nil {
		return unavailable{fmt.Errorf("%s can't run here: %v", name, err)}
	}
	r := bufio.NewReaderSize(out, 64<<10)
	wrote := false
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			wrote = true
			if len(line) > 1<<20 {
				line = line[:1<<20]
			}
			each(bytes.TrimRight(line, "\r\n"))
		}
		if err != nil {
			break
		}
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		return nil
	}
	if err != nil && !wrote {
		reason := strings.TrimSpace(stderr.String())
		if reason == "" {
			reason = err.Error()
		}
		return unavailable{errors.New(reason)}
	}
	return nil
}

// limited keeps the first n bytes written to it.
type limited struct {
	w io.Writer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		_, _ = l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}

// ---- systemd's journal (Linux) ----

func readJournal(ctx context.Context, src config.LogSource, lines int, follow bool, emit func(Entry)) error {
	args := []string{"--output=json", "--no-pager", "--lines=" + strconv.Itoa(lines)}
	if follow {
		args = append(args, "--follow")
	}
	if src.Target != "" {
		args = append(args, "--unit="+src.Target)
	}
	return run(ctx, "journalctl", args, nil, func(line []byte) {
		if e, ok := parseJournal(line); ok {
			emit(e)
		}
	})
}

func parseJournal(line []byte) (Entry, bool) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(line, &raw) != nil {
		return Entry{}, false
	}
	field := func(key string) string {
		var s string
		if v, ok := raw[key]; ok {
			if json.Unmarshal(v, &s) == nil {
				return s
			}
			// A message the journal keeps as bytes.
			var b []byte
			var ints []int
			if json.Unmarshal(v, &ints) == nil {
				for _, i := range ints {
					b = append(b, byte(i))
				}
				return string(b)
			}
		}
		return ""
	}
	e := Entry{Text: field("MESSAGE"), Level: "info"}
	if us, err := strconv.ParseInt(field("__REALTIME_TIMESTAMP"), 10, 64); err == nil {
		e.TS = time.UnixMicro(us).UTC().Format(time.RFC3339Nano)
	}
	switch p := field("PRIORITY"); p {
	case "0", "1", "2", "3":
		e.Level = "error"
	case "4":
		e.Level = "warn"
	case "7":
		e.Level = "debug"
	}
	for _, key := range []string{"SYSLOG_IDENTIFIER", "_COMM", "_SYSTEMD_UNIT"} {
		if v := field(key); v != "" {
			e.Origin = v
			break
		}
	}
	return e, true
}

// ---- the unified log (macOS) ----

// What `log show` reads back for a view's first lines: the unified log is
// written to all the time, so a window without a predicate is short.
const (
	macosWindow           = "2m"
	macosWindowEverything = "20s"
)

func readMacOS(ctx context.Context, src config.LogSource, lines int, follow bool, emit func(Entry)) error {
	predicate := func(args []string) []string {
		if src.Target != "" {
			return append(args, "--predicate", src.Target)
		}
		return args
	}
	if lines > 0 {
		window := macosWindowEverything
		if src.Target != "" {
			window = macosWindow
		}
		ring := make([]Entry, 0, lines)
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := run(sctx, "log", predicate([]string{"show", "--style", "ndjson", "--last", window}), nil, func(line []byte) {
			e, ok := parseMacOS(line)
			if !ok {
				return
			}
			if len(ring) == lines {
				copy(ring, ring[1:])
				ring = ring[:lines-1]
			}
			ring = append(ring, e)
		})
		cancel()
		if err != nil && ctx.Err() == nil && len(ring) == 0 {
			return err
		}
		for _, e := range ring {
			emit(e)
		}
	}
	if !follow {
		return nil
	}
	return run(ctx, "log", predicate([]string{"stream", "--style", "ndjson"}), nil, func(line []byte) {
		if e, ok := parseMacOS(line); ok {
			emit(e)
		}
	})
}

func parseMacOS(line []byte) (Entry, bool) {
	var m struct {
		Timestamp        string `json:"timestamp"`
		MessageType      string `json:"messageType"`
		EventMessage     string `json:"eventMessage"`
		ProcessImagePath string `json:"processImagePath"`
		Subsystem        string `json:"subsystem"`
		EventType        string `json:"eventType"`
	}
	if json.Unmarshal(line, &m) != nil || m.EventMessage == "" && m.EventType != "logEvent" {
		return Entry{}, false
	}
	e := Entry{Text: m.EventMessage, Level: "info"}
	if t, err := time.Parse("2006-01-02 15:04:05.000000-0700", m.Timestamp); err == nil {
		e.TS = t.UTC().Format(time.RFC3339Nano)
	}
	switch m.MessageType {
	case "Debug":
		e.Level = "debug"
	case "Error", "Fault":
		e.Level = "error"
	}
	e.Origin = filepath.Base(m.ProcessImagePath)
	if m.ProcessImagePath == "" {
		e.Origin = ""
	}
	if m.Subsystem != "" {
		e.Origin = strings.TrimSpace(e.Origin + " " + m.Subsystem)
	}
	return e, true
}

// ---- event logs (Windows) ----

// eventLogScript reads an event log's newest events, then every two seconds
// those after them, one JSON line each. What it reads comes from the
// environment, never from the script's text.
const eventLogScript = `
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$name = $env:BRIDGE_LOG_NAME
$lines = [int]$env:BRIDGE_LOG_LINES
$follow = $env:BRIDGE_LOG_FOLLOW -eq '1'
try { $null = Get-WinEvent -ListLog $name } catch { [Console]::Error.WriteLine($_.Exception.Message); exit 3 }
$ErrorActionPreference = 'SilentlyContinue'
function Emit($e) {
  $json = [pscustomobject]@{ id = $e.RecordId; ts = $e.TimeCreated.ToUniversalTime().ToString('o'); level = [int]$e.Level; provider = $e.ProviderName; event = $e.Id; text = $e.Message } | ConvertTo-Json -Compress
  [Console]::Out.WriteLine($json)
}
$last = 0
$newest = Get-WinEvent -LogName $name -MaxEvents ([Math]::Max($lines, 1))
if ($newest) {
  $sorted = @($newest | Sort-Object RecordId)
  $last = $sorted[-1].RecordId
  if ($lines -gt 0) { foreach ($e in $sorted) { Emit $e } }
}
[Console]::Out.Flush()
while ($follow) {
  Start-Sleep -Seconds 2
  $next = Get-WinEvent -LogName $name -FilterXPath "*[System[EventRecordID>$last]]" -MaxEvents 500
  if ($next) {
    foreach ($e in @($next | Sort-Object RecordId)) { Emit $e; $last = $e.RecordId }
    [Console]::Out.Flush()
  }
}
`

func readEventLog(ctx context.Context, src config.LogSource, lines int, follow bool, emit func(Entry)) error {
	followFlag := "0"
	if follow {
		followFlag = "1"
	}
	env := []string{"BRIDGE_LOG_NAME=" + src.Target, "BRIDGE_LOG_LINES=" + strconv.Itoa(lines), "BRIDGE_LOG_FOLLOW=" + followFlag}
	return run(ctx, "powershell.exe", []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", encodeCommand(eventLogScript)}, env, func(line []byte) {
		if e, ok := parseEventLog(line); ok {
			emit(e)
		}
	})
}

func parseEventLog(line []byte) (Entry, bool) {
	var m struct {
		TS       string `json:"ts"`
		Level    int    `json:"level"`
		Provider string `json:"provider"`
		Event    int    `json:"event"`
		Text     string `json:"text"`
	}
	if json.Unmarshal(bytes.TrimPrefix(line, []byte("\xef\xbb\xbf")), &m) != nil || m.TS == "" {
		return Entry{}, false
	}
	e := Entry{TS: m.TS, Text: strings.ReplaceAll(m.Text, "\r\n", "\n"), Level: "info"}
	if t, err := time.Parse(time.RFC3339Nano, m.TS); err == nil {
		e.TS = t.UTC().Format(time.RFC3339Nano)
	}
	switch m.Level {
	case 1, 2:
		e.Level = "error"
	case 3:
		e.Level = "warn"
	case 5:
		e.Level = "debug"
	}
	e.Origin = strings.TrimSpace(fmt.Sprintf("%s %d", m.Provider, m.Event))
	if e.Text == "" {
		e.Text = "(no message)"
	}
	return e, true
}

// encodeCommand is a script as -EncodedCommand takes it: UTF-16LE in base64, so
// no quote in it meets the command line's.
func encodeCommand(script string) string {
	units := utf16.Encode([]rune(script))
	b := make([]byte, len(units)*2)
	for i, u := range units {
		b[2*i], b[2*i+1] = byte(u), byte(u>>8)
	}
	return base64.StdEncoding.EncodeToString(b)
}
