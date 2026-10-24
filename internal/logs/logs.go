// Package logs lets ViaVeritas read, live, the logs this computer lists
// (config.LogSources, the logs feature), the bridge's own among them: the last
// lines of one, then what it writes, filtered here by level and text.
//
// A "logs" stream names a source by its id and sends back one JSON line per
// entry (Entry), or a note of the bridge's own (Entry with Note set). What
// ViaVeritas sends is a new filter, one JSON line (Params without Source): the
// view is read again under it, after a {"reset":true} line.
package logs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"bridge/internal/config"
	"bridge/internal/logx"
	"bridge/internal/streams"
)

// Codes a log view is refused or closed with, beside the streams' own.
const (
	CodeUnknownSource     = "unknown_source"
	CodeSourceUnavailable = "source_unavailable"
	CodeRemoved           = "removed"
)

const (
	// MaxViews open at once on this bridge.
	MaxViews = 4
	// MaxTime is the longest a view stays open.
	MaxTime = 4 * time.Hour
	// DefaultLines and MaxLines of what a view starts with.
	DefaultLines = 200
	MaxLines     = 2000
	// MaxText is the longest entry sent, in bytes.
	MaxText = 8 << 10
	// MaxQuery is the longest text filter.
	MaxQuery = 200

	flushEvery = 150 * time.Millisecond
	flushBytes = 16 << 10
)

var configOf = config.LoadOrDefaults

// Register offers log views on reg; cfg is where the list of sources is read.
func Register(reg *streams.Registry, cfg func() config.Config) {
	configOf = cfg
	reg.HandleChecked("logs", config.FeatureLogs, MaxViews, check, View)
}

// Levels, lowest first.
var levels = map[string]int{"debug": 0, "info": 1, "warn": 2, "error": 3}

// Entry is one line of a log, as ViaVeritas reads it.
type Entry struct {
	// TS is when it was written, RFC 3339, when the log says.
	TS    string `json:"ts,omitempty"`
	Level string `json:"level,omitempty"`
	Text  string `json:"text"`
	// Origin is what wrote it: a file's path, a unit, a process, a provider.
	Origin string `json:"origin,omitempty"`
	// Note marks a line of the bridge's own about the view (now following another file).
	Note bool `json:"note,omitempty"`
}

// Filter is what a view keeps: entries at Level or above, holding Query (any case).
type Filter struct {
	Level string `json:"level,omitempty"`
	Query string `json:"query,omitempty"`
}

func (f Filter) keeps(e Entry) bool {
	if e.Note {
		return true
	}
	if f.Level != "" && levels[e.Level] < levels[f.Level] {
		return false
	}
	return f.Query == "" || strings.Contains(strings.ToLower(e.Text+"\x00"+e.Origin), strings.ToLower(f.Query))
}

// Params is what a view opens with, and what a new filter sends.
type Params struct {
	Source string `json:"source,omitempty"`
	// Lines is how many of the newest entries it starts with (before filtering).
	Lines  *int  `json:"lines,omitempty"`
	Follow *bool `json:"follow,omitempty"`
	Filter
}

func (p Params) lines() int {
	if p.Lines == nil {
		return DefaultLines
	}
	return min(max(*p.Lines, 0), MaxLines)
}

func (p Params) valid() error {
	if p.Level != "" {
		if _, ok := levels[p.Level]; !ok {
			return fmt.Errorf("%q isn't a level: debug, info, warn or error", p.Level)
		}
	}
	if len(p.Query) > MaxQuery {
		return fmt.Errorf("the text filter is %d characters at most", MaxQuery)
	}
	return nil
}

// A reader sends what a source holds: its newest `lines` entries, then, when
// following, what it writes, until ctx ends. An error before anything was sent
// says the source can't be read here.
type reader func(ctx context.Context, src config.LogSource, lines int, follow bool, emit func(Entry)) error

func readerFor(src config.LogSource) reader {
	switch src.Kind {
	case config.LogBridge, config.LogFile:
		return readFile
	case config.LogJournal:
		return readJournal
	case config.LogMacOS:
		return readMacOS
	case config.LogEventLog:
		return readEventLog
	}
	return nil
}

// unavailable is a source this computer can't read now.
type unavailable struct{ error }

func check(o streams.Open) error {
	var p Params
	if err := json.Unmarshal(o.Params, &p); err != nil || p.Source == "" {
		return &streams.Refusal{Code: streams.CodeInvalid, Message: "a log view names the log to read"}
	}
	if err := p.valid(); err != nil {
		return &streams.Refusal{Code: streams.CodeInvalid, Message: err.Error()}
	}
	src, ok := configOf().LogSourceByID(p.Source)
	if !ok || readerFor(src) == nil {
		return &streams.Refusal{Code: CodeUnknownSource, Message: "this computer doesn't share that log"}
	}
	return nil
}

// View sends a source's newest entries, then what it writes, as filtered;
// a new filter from ViaVeritas reads it again. It ends with the stream, when
// the source leaves the list, or after MaxTime.
func View(s *streams.Stream) error {
	var p Params
	_ = json.Unmarshal(s.Params, &p)
	src, _ := configOf().LogSourceByID(p.Source)
	read := readerFor(src)
	out := newBatch(s)
	defer out.stop()
	logx.Info("logs", "%s is reading %s", who(s.User), src.Name)

	filters := make(chan Params, 1)
	go func() {
		sc := bufio.NewScanner(s)
		sc.Buffer(make([]byte, 4096), 64<<10)
		for sc.Scan() {
			var next Params
			if json.Unmarshal(sc.Bytes(), &next) != nil || next.valid() != nil {
				continue
			}
			select {
			case <-filters:
			default:
			}
			filters <- next
		}
	}()

	longest := time.NewTimer(MaxTime)
	defer longest.Stop()
	listed := time.NewTicker(2 * time.Second)
	defer listed.Stop()

	for first := true; ; first = false {
		ctx, cancel := context.WithCancel(s.Context())
		keep := p.Filter
		var sent atomic.Bool
		done := make(chan error, 1)
		follow := p.Follow == nil || *p.Follow
		go func() {
			done <- read(ctx, src, p.lines(), follow, func(e Entry) {
				e.Text = clean(e.Text)
				if keep.keeps(e) {
					sent.Store(true)
					out.add(e)
				}
			})
		}()
		stop := func() {
			cancel()
			<-done
			out.flush()
		}

	waiting:
		for {
			select {
			case err := <-done:
				cancel()
				out.flush()
				var u unavailable
				switch {
				case errors.As(err, &u) && first && !sent.Load():
					s.Close(CodeSourceUnavailable, u.Error())
				case err != nil && s.Context().Err() == nil:
					s.Close(streams.CodeError, err.Error())
				}
				// Read whole without following, or a source that stopped by itself: the view is over.
				return nil
			case next := <-filters:
				stop()
				next.Source = p.Source
				if next.Follow == nil {
					next.Follow = p.Follow
				}
				if next.Lines == nil {
					next.Lines = p.Lines
				}
				p = next
				out.add(map[string]bool{"reset": true})
				break waiting
			case <-listed.C:
				if _, ok := configOf().LogSourceByID(src.ID); !ok {
					stop()
					s.Close(CodeRemoved, "its computer stopped sharing this log")
					return nil
				}
			case <-longest.C:
				stop()
				s.Close(streams.CodeTooLong, "a log view stays open four hours at most")
				return nil
			case <-s.Context().Done():
				cancel()
				<-done
				return nil
			}
		}
	}
}

// clean makes an entry's text sendable: valid UTF-8, without its line end, MaxText at most.
func clean(text string) string {
	text = strings.TrimRight(strings.ToValidUTF8(text, "�"), "\r\n")
	if len(text) <= MaxText {
		return text
	}
	cut := MaxText
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}

func who(u streams.User) string {
	switch {
	case u.Name != "":
		return u.Name
	case u.Email != "":
		return u.Email
	}
	return "ViaVeritas"
}

// batch gathers lines and writes them together: every flushEvery, or once
// flushBytes wait. A write waits for room on the stream, which holds back
// whoever adds (backpressure to the source).
type batch struct {
	mu   sync.Mutex
	s    *streams.Stream
	buf  []byte
	quit chan struct{}
	wg   sync.WaitGroup
}

func newBatch(s *streams.Stream) *batch {
	b := &batch{s: s, quit: make(chan struct{})}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		tick := time.NewTicker(flushEvery)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				b.flush()
			case <-b.quit:
				return
			}
		}
	}()
	return b
}

func (b *batch) add(v any) {
	line, err := json.Marshal(v)
	if err != nil {
		return
	}
	b.mu.Lock()
	b.buf = append(append(b.buf, line...), '\n')
	full := len(b.buf) >= flushBytes
	b.mu.Unlock()
	if full {
		b.flush()
	}
}

func (b *batch) flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buf) == 0 {
		return
	}
	_, _ = b.s.Write(b.buf)
	b.buf = b.buf[:0]
}

func (b *batch) stop() {
	close(b.quit)
	b.wg.Wait()
	b.flush()
}
