package telemetry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s := &Store{nudge: make(chan struct{}, 1), version: "v1.2.3"}
	s.Setup(dir)
	return s, dir
}

func TestKeptUntilAcknowledged(t *testing.T) {
	s, _ := newStore(t)
	s.Record(Started, map[string]any{"after": "clean"})
	s.Record(UpdateFailed, map[string]any{"stage": "download"})
	events := s.Pending()
	if len(events) != 2 || events[0].Kind != Started || events[1].Kind != UpdateFailed || events[0].Version != "v1.2.3" || events[0].ID == events[1].ID {
		t.Fatalf("pending %+v", events)
	}
	s.Drop([]string{events[0].ID})
	if left := s.Pending(); len(left) != 1 || left[0].ID != events[1].ID {
		t.Fatalf("after dropping one: %+v", left)
	}
}

func TestKeepsTheNewest(t *testing.T) {
	s, _ := newStore(t)
	for i := 0; i < keep+5; i++ {
		s.Record(Started, map[string]any{"n": i})
	}
	events := s.Pending()
	if len(events) != keep || events[0].Detail["n"].(float64) != 5 {
		t.Fatalf("kept %d, first n=%v", len(events), events[0].Detail["n"])
	}
}

func TestRunSendsAndForgets(t *testing.T) {
	s, _ := newStore(t)
	var mu sync.Mutex
	failed, sent := false, 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx, func(_ context.Context, events []Event) error {
		mu.Lock()
		defer mu.Unlock()
		if !failed {
			failed = true
			return errors.New("offline")
		}
		sent += len(events)
		return nil
	})
	s.Record(Started, nil)
	time.Sleep(50 * time.Millisecond)
	s.Record(Stopped, nil)
	for deadline := time.Now().Add(2 * time.Second); len(s.Pending()) > 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if left := s.Pending(); len(left) != 0 || !failed || sent != 2 {
		t.Fatalf("failed=%v sent=%d still waiting=%+v", failed, sent, left)
	}
}

func TestBeginReadsTheCrash(t *testing.T) {
	s, dir := newStore(t)
	trace := "panic: open /Users/anna/Scans/a.pdf: permission denied\n\ngoroutine 1 [running]:\nmain.main()\n\t/home/runner/work/bridge/cmd/bridge/main.go:42 +0x1c\n"
	if err := os.WriteFile(filepath.Join(dir, "crash.log"), []byte(trace), 0o600); err != nil {
		t.Fatal(err)
	}
	s.Begin(dir)
	events := s.Pending()
	if len(events) != 2 || events[0].Kind != Crashed || events[1].Kind != Started || events[1].Detail["after"] != "crash" {
		t.Fatalf("events %+v", events)
	}
	got := events[0].Detail["trace"].(string)
	if strings.Contains(got, "anna") || !strings.Contains(got, "open <path>: permission denied") || !strings.Contains(got, "bridge/main.go:42") {
		t.Fatalf("trace not redacted: %s", got)
	}

	// The next start without a clean stop in between.
	s.Drop([]string{events[0].ID, events[1].ID})
	_ = os.WriteFile(filepath.Join(dir, "crash.log"), nil, 0o600)
	s.Begin(dir)
	if events := s.Pending(); len(events) != 1 || events[0].Detail["after"] != "unclean" {
		t.Fatalf("after an unclean stop: %+v", events)
	}
	s.End(dir, "stopped")
	if _, err := os.Stat(filepath.Join(dir, "running")); !os.IsNotExist(err) {
		t.Fatal("the running mark stayed after a clean stop")
	}
}

func TestRedact(t *testing.T) {
	cases := map[string]string{
		`open C:\Users\anna\Desktop\x.txt: denied`:            `open <path>: denied`,
		`dial https://api.viaveritas.app/bridges/sync failed`: `dial https://api.viaveritas.app/bridges/sync failed`,
		`at /opt/viaveritas/bridge/internal/watch/scan.go:7`:  `at watch/scan.go:7`,
		`bridge/internal/watch/scan.go:7 +0x1`:                `bridge/internal/watch/scan.go:7 +0x1`,
		`share \\nas\scans\a.pdf gone`:                        `share <path> gone`,
	}
	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}
