//go:build darwin || linux

package terminal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"bridge/internal/config"
	"bridge/internal/streams"
)

// socket stands in for the bridge's socket: it keeps what the streams emit.
type socket struct {
	mu     sync.Mutex
	events []map[string]any
	names  []string
}

func (s *socket) Emit(_ context.Context, event string, data any, _ bool) error {
	b, _ := json.Marshal(data)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	s.mu.Lock()
	s.events = append(s.events, m)
	s.names = append(s.names, event)
	s.mu.Unlock()
	return nil
}

// output is what stream id wrote, in order.
func (s *socket) output(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	type chunk struct {
		seq int
		d   []byte
	}
	var chunks []chunk
	for i, m := range s.events {
		if s.names[i] == "stream_data" && m["id"] == id {
			d, _ := base64.StdEncoding.DecodeString(m["d"].(string))
			chunks = append(chunks, chunk{int(m["seq"].(float64)), d})
		}
	}
	sort.Slice(chunks, func(a, b int) bool { return chunks[a].seq < chunks[b].seq })
	var out strings.Builder
	for _, c := range chunks {
		out.Write(c.d)
	}
	return out.String()
}

// last is the last event of a name for stream id, or nil.
func (s *socket) last(name, id string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.events) - 1; i >= 0; i-- {
		if s.names[i] == name && s.events[i]["id"] == id {
			return s.events[i]
		}
	}
	return nil
}

func (s *socket) await(t *testing.T, name, id string, within time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if m := s.last(name, id); m != nil {
			return m
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %s for %s in %s; output so far: %q", name, id, within, s.output(id))
	return nil
}

func setup(t *testing.T) (*streams.Registry, *socket) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Features = map[config.Feature]bool{config.FeatureTerminal: true}
	cfg.HideSessionNotices = true
	reg := streams.New()
	reg.SetConfig(func() config.Config { return cfg })
	Register(reg, func() config.Config { return cfg })
	sock := &socket{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		reg.Detach()
	})
	reg.Attach(ctx, sock)
	return reg, sock
}

func open(t *testing.T, reg *streams.Registry, id, kind string, params any) streams.OpenAnswer {
	t.Helper()
	raw, _ := json.Marshal(params)
	return reg.Open(streams.Open{ID: id, Kind: kind, User: streams.User{Name: "Anna"}, Params: raw, Window: streams.Window})
}

func send(reg *streams.Registry, id string, seq int, text string) {
	raw, _ := json.Marshal(map[string]any{"id": id, "seq": seq, "d": base64.StdEncoding.EncodeToString([]byte(text))})
	reg.Data(raw)
}

func TestTerminalRunsAShell(t *testing.T) {
	reg, sock := setup(t)
	if a := open(t, reg, "t1", "terminal", map[string]int{"cols": 100, "rows": 30}); !a.OK {
		t.Fatalf("open: %+v", a)
	}
	info := sock.await(t, "stream_info", "t1", 5*time.Second)
	acct, _ := Resolve(config.RunAsUser)
	if info["run_as"] != acct.Label() {
		t.Errorf("runs as %v, want %s", info["run_as"], acct.Label())
	}
	send(reg, "t1", 0, "echo hello-$((40+2)); stty size\n")
	send(reg, "t1", 1, "exit 3\n")
	closed := sock.await(t, "stream_close", "t1", 10*time.Second)
	if closed["exit_code"] != float64(3) || closed["code"] != "done" {
		t.Errorf("closed with %v", closed)
	}
	out := sock.output("t1")
	if !strings.Contains(out, "hello-42") || !strings.Contains(out, "30 100") {
		t.Errorf("output %q", out)
	}
	if closed["bytes_out"].(float64) < float64(len("hello-42")) {
		t.Errorf("bytes out %v", closed["bytes_out"])
	}
}

func TestTerminalEndsWhenClosed(t *testing.T) {
	reg, sock := setup(t)
	if a := open(t, reg, "t2", "terminal", nil); !a.OK {
		t.Fatalf("open: %+v", a)
	}
	sock.await(t, "stream_info", "t2", 5*time.Second)
	send(reg, "t2", 0, "echo $$\n")
	time.Sleep(500 * time.Millisecond)
	raw, _ := json.Marshal(map[string]string{"id": "t2", "code": "done"})
	reg.Close(raw)
	for deadline := time.Now().Add(5 * time.Second); len(reg.List()) > 0 && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(reg.List()); n != 0 {
		t.Fatalf("%d streams still open after ViaVeritas closed it", n)
	}
}

func TestCommandOutputAndExitCode(t *testing.T) {
	reg, sock := setup(t)
	if a := open(t, reg, "c1", "exec", map[string]any{"command": "echo out; echo err 1>&2; exit 4"}); !a.OK {
		t.Fatalf("open: %+v", a)
	}
	closed := sock.await(t, "stream_close", "c1", 10*time.Second)
	if closed["exit_code"] != float64(4) {
		t.Errorf("closed with %v", closed)
	}
	if out := sock.output("c1"); !strings.Contains(out, "out\n") || !strings.Contains(out, "err\n") {
		t.Errorf("output %q", out)
	}
}

func TestCommandTimesOut(t *testing.T) {
	reg, sock := setup(t)
	started := time.Now()
	if a := open(t, reg, "c2", "exec", map[string]any{"command": "sleep 30", "timeout_seconds": 1}); !a.OK {
		t.Fatalf("open: %+v", a)
	}
	closed := sock.await(t, "stream_close", "c2", 10*time.Second)
	if closed["code"] != "timeout" || time.Since(started) > 8*time.Second {
		t.Errorf("closed with %v after %s", closed, time.Since(started))
	}
}

func TestRefusals(t *testing.T) {
	reg, _ := setup(t)
	if a := open(t, reg, "c3", "exec", map[string]any{"command": "  "}); a.OK || a.Code != streams.CodeUnavailable {
		t.Errorf("an empty command: %+v", a)
	}
	if a := open(t, reg, "c4", "exec", map[string]any{"command": strings.Repeat("x", MaxCommand+1)}); a.OK || a.Code != streams.CodeUnavailable {
		t.Errorf("a command too long: %+v", a)
	}
	for i := 0; i < MaxTerminals; i++ {
		if a := open(t, reg, fmt.Sprintf("t%d", 10+i), "terminal", nil); !a.OK {
			t.Fatalf("terminal %d: %+v", i, a)
		}
	}
	if a := open(t, reg, "t99", "terminal", nil); a.OK || a.Code != streams.CodeLimit {
		t.Errorf("one terminal too many: %+v", a)
	}
}
