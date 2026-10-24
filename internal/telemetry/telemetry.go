// Package telemetry keeps what happens to the bridge itself (it started,
// stopped or crashed; an update started, failed, was taken back or was
// installed) and sends it to ViaVeritas over the socket, where the portal's
// Activity tab shows it. Events wait in events.jsonl in the bridge's folder
// until ViaVeritas acknowledges them, so one written just before a restart (an
// update) or while the computer is offline isn't lost.
//
// It is always on: no switch here or in ViaVeritas holds it. Nothing names a
// file on the computer: a crash's trace has its paths taken out (Redact).
package telemetry

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"bridge/internal/config"
	"bridge/internal/logx"
	"bridge/internal/utils"
)

// Kind is what happened.
type Kind string

const (
	Started          Kind = "started"
	Stopped          Kind = "stopped"
	Crashed          Kind = "crashed"
	UpdateStarted    Kind = "update_started"
	UpdateFailed     Kind = "update_failed"
	UpdateInstalled  Kind = "update_installed"
	UpdateRolledBack Kind = "update_rolled_back"
)

// Event is one thing that happened, as ViaVeritas keeps it.
type Event struct {
	ID      string         `json:"id"`
	At      string         `json:"at"`
	Kind    Kind           `json:"kind"`
	Version string         `json:"version"`
	Detail  map[string]any `json:"detail,omitempty"`
}

const (
	// keep is how many events wait at most: the oldest go first.
	keep = 200
	// maxText is the longest text a detail keeps (a crash's trace).
	maxText = 12 << 10
	// batch is how many go in one message.
	batch = 50
	// every is how often waiting events are tried again while the socket is up.
	every = time.Minute
)

// Store keeps the events in a file until they are sent.
type Store struct {
	mu      sync.Mutex
	path    string
	nudge   chan struct{}
	version string
}

// Default is the service's store; it keeps nothing until Setup.
var Default = &Store{nudge: make(chan struct{}, 1)}

// Setup keeps events in dir.
func (s *Store) Setup(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.path = filepath.Join(dir, "events.jsonl")
	if s.version == "" {
		s.version = config.DefaultVersion
	}
}

// Record keeps an event to send.
func Record(kind Kind, detail map[string]any) { Default.Record(kind, detail) }

// Record keeps an event to send.
func (s *Store) Record(kind Kind, detail map[string]any) {
	s.mu.Lock()
	if s.path == "" {
		s.mu.Unlock()
		return
	}
	ev := Event{ID: newID(), At: time.Now().UTC().Format(time.RFC3339Nano), Kind: kind, Version: s.version, Detail: clamp(detail)}
	events := append(s.read(), ev)
	if len(events) > keep {
		events = events[len(events)-keep:]
	}
	err := s.write(events)
	s.mu.Unlock()
	if err != nil {
		logx.Warn("telemetry", "could not keep the %s event: %v", kind, err)
		return
	}
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}

// Pending is what waits to be sent, oldest first.
func (s *Store) Pending() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read()
}

// Drop forgets the events ViaVeritas acknowledged.
func (s *Store) Drop(ids []string) {
	if len(ids) == 0 {
		return
	}
	gone := map[string]bool{}
	for _, id := range ids {
		gone[id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var left []Event
	for _, ev := range s.read() {
		if !gone[ev.ID] {
			left = append(left, ev)
		}
	}
	if err := s.write(left); err != nil {
		logx.Warn("telemetry", "could not forget the events sent: %v", err)
	}
}

// Flush asks Run to send what waits now (the socket connected).
func (s *Store) Flush() {
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}

// Run sends what waits through send until ctx ends: when an event is kept,
// when asked (Flush), and every minute. An event leaves the file only once
// send returns without an error.
func (s *Store) Run(ctx context.Context, send func(ctx context.Context, events []Event) error) {
	for {
		for {
			events := s.Pending()
			if len(events) == 0 {
				break
			}
			if len(events) > batch {
				events = events[:batch]
			}
			if err := send(ctx, events); err != nil {
				break
			}
			ids := make([]string, len(events))
			for i, ev := range events {
				ids[i] = ev.ID
			}
			s.Drop(ids)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.nudge:
		case <-time.After(every):
		}
	}
}

// read is the file's events; a line it can't read is skipped. Holds s.mu.
func (s *Store) read() []Event {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return nil
	}
	var events []Event
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var ev Event
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.ID != "" {
			events = append(events, ev)
		}
	}
	return events
}

// write replaces the file with events. Holds s.mu.
func (s *Store) write(events []Event) error {
	if len(events) == 0 {
		if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	var buf bytes.Buffer
	for _, ev := range events {
		line, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return utils.AtomicWrite(s.path, buf.Bytes(), 0o600)
}

// clamp cuts long texts in a detail, so an event stays small.
func clamp(detail map[string]any) map[string]any {
	if len(detail) == 0 {
		return nil
	}
	out := make(map[string]any, len(detail))
	for k, v := range detail {
		if text, ok := v.(string); ok && len(text) > maxText {
			v = text[:maxText] + "…"
		}
		out[k] = v
	}
	return out
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
