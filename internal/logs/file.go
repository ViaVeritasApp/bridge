package logs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"bridge/internal/config"
)

const (
	// tailWindow is the most of a file's end read for its last lines.
	tailWindow = 4 << 20
	// readChunk is the most read of a file each time it is looked at.
	readChunk = 1 << 20
)

var (
	pollEvery = 500 * time.Millisecond
	globEvery = 5 * time.Second
)

// newest is the file a source reads: its path, or the newest regular file its pattern matches.
func newest(pattern string) (string, error) {
	if !hasMeta(pattern) {
		info, err := os.Stat(pattern)
		if err != nil {
			return "", fmt.Errorf("%s can't be read: %v", pattern, err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("%s isn't a file", pattern)
		}
		return pattern, nil
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}
	best, bestAt := "", time.Time{}
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if best == "" || info.ModTime().After(bestAt) {
			best, bestAt = m, info.ModTime()
		}
	}
	if best == "" {
		return "", fmt.Errorf("no file matches %s", pattern)
	}
	return best, nil
}

func hasMeta(path string) bool { return strings.ContainsAny(path, `*?[`) }

// readFile reads a file's last lines, then follows what is written to it: a
// file cut back is read from its start, and a pattern's newer file taken up
// in place of the one before (a log that rotates).
func readFile(ctx context.Context, src config.LogSource, lines int, follow bool, emit func(Entry)) error {
	pattern, parse := src.Target, parseLine
	if src.Kind == config.LogBridge {
		pattern, parse = configOf().LogFile(), parseBridgeLine
	}
	path, err := newest(pattern)
	if err != nil {
		return unavailable{err}
	}
	f := &follower{path: path, parse: parse, emit: emit}
	if err := f.tail(lines); err != nil {
		return unavailable{err}
	}
	if !follow {
		return nil
	}
	poll := time.NewTicker(pollEvery)
	defer poll.Stop()
	lastGlob := time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-poll.C:
		}
		f.more()
		if hasMeta(pattern) && time.Since(lastGlob) >= globEvery {
			lastGlob = time.Now()
			if next, err := newest(pattern); err == nil && next != f.path {
				f.more()
				f.flushRest()
				emit(Entry{Note: true, Text: "now following " + next, Origin: next})
				f = &follower{path: next, parse: parse, emit: emit}
			}
		}
	}
}

// follower reads one file from an offset, a line at a time.
type follower struct {
	path   string
	offset int64
	rest   []byte
	parse  func(line string, last Entry) Entry
	last   Entry
	emit   func(Entry)
}

func (f *follower) tail(lines int) error {
	file, err := os.Open(f.path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	from := max(0, size-tailWindow)
	buf := make([]byte, size-from)
	if _, err := file.ReadAt(buf, from); err != nil && err != io.EOF {
		return err
	}
	f.offset = size
	if from > 0 {
		// The first line read may be cut: start after it.
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	if lines <= 0 {
		return nil
	}
	all := splitLines(buf)
	if len(all) > lines {
		// Parse a few more first, so a line continuing an entry knows its level.
		for _, l := range all[max(0, len(all)-lines-20) : len(all)-lines] {
			f.last = f.parse(l, f.last)
		}
		all = all[len(all)-lines:]
	}
	for _, l := range all {
		f.send(l)
	}
	return nil
}

func (f *follower) more() {
	info, err := os.Stat(f.path)
	if err != nil {
		return
	}
	size := info.Size()
	if size < f.offset {
		f.flushRest()
		f.emit(Entry{Note: true, Text: "the file was cut back: reading it from its start", Origin: f.path})
		f.offset = 0
	}
	if size == f.offset {
		return
	}
	file, err := os.Open(f.path)
	if err != nil {
		return
	}
	defer file.Close()
	buf := make([]byte, min(size-f.offset, readChunk))
	n, err := file.ReadAt(buf, f.offset)
	if n == 0 && err != nil {
		return
	}
	f.offset += int64(n)
	data := append(f.rest, buf[:n]...)
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		f.rest = data
		if len(f.rest) > MaxText*2 {
			f.flushRest()
		}
		return
	}
	for _, l := range splitLines(data[:end+1]) {
		f.send(l)
	}
	f.rest = append([]byte(nil), data[end+1:]...)
}

func (f *follower) flushRest() {
	if len(f.rest) > 0 {
		f.send(string(f.rest))
		f.rest = nil
	}
}

func (f *follower) send(line string) {
	f.last = f.parse(line, f.last)
	f.emit(f.last)
}

func splitLines(b []byte) []string {
	text := strings.TrimRight(string(b), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// The bridge's own lines: "15:04:05.000  LEVEL  [component]  message".
var bridgeLine = regexp.MustCompile(`^(\d\d:\d\d:\d\d\.\d{3})  (DEBUG|INFO |OK   |WARN |ERROR)  \[([^\]]*)\]\s*(.*)$`)

func parseBridgeLine(line string, last Entry) Entry {
	m := bridgeLine.FindStringSubmatch(strings.TrimRight(line, "\r"))
	if m == nil {
		// A message's further lines.
		return Entry{TS: last.TS, Level: orInfo(last.Level), Text: line, Origin: last.Origin}
	}
	level := "info"
	switch strings.TrimSpace(m[2]) {
	case "DEBUG":
		level = "debug"
	case "WARN":
		level = "warn"
	case "ERROR":
		level = "error"
	}
	return Entry{TS: m[1], Level: level, Text: m[4], Origin: m[3]}
}

func orInfo(level string) string {
	if level == "" {
		return "info"
	}
	return level
}

var (
	errorWords = regexp.MustCompile(`(?i)\b(error|err|fatal|crit|critical|panic|emerg|emergency|alert|severe|exception)\b`)
	warnWords  = regexp.MustCompile(`(?i)\b(warn|warning)\b`)
	debugWords = regexp.MustCompile(`(?i)\b(debug|trace|verbose)\b`)
)

// parseLine reads any other file's line: its level guessed from the words near its start.
func parseLine(line string, _ Entry) Entry {
	line = strings.TrimRight(line, "\r")
	head := line
	if len(head) > 160 {
		head = head[:160]
	}
	level := "info"
	switch {
	case errorWords.MatchString(head):
		level = "error"
	case warnWords.MatchString(head):
		level = "warn"
	case debugWords.MatchString(head):
		level = "debug"
	}
	return Entry{Level: level, Text: line}
}
