package logs

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"bridge/internal/config"
	"bridge/internal/streams"
)

func init() {
	pollEvery = 20 * time.Millisecond
	globEvery = 50 * time.Millisecond
}

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

// lines is what stream id sent, one JSON line each.
func (s *socket) lines(id string) []map[string]any {
	s.mu.Lock()
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
	s.mu.Unlock()
	sort.Slice(chunks, func(a, b int) bool { return chunks[a].seq < chunks[b].seq })
	var all strings.Builder
	for _, c := range chunks {
		all.Write(c.d)
	}
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(all.String()))
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

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

func (s *socket) await(t *testing.T, name, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m := s.last(name, id); m != nil {
			return m
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %s for %s; sent so far: %v", name, id, s.lines(id))
	return nil
}

// texts waits until stream id sent want lines (beside notes and resets), and answers their texts.
func (s *socket) texts(t *testing.T, id string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		got = got[:0]
		for _, m := range s.lines(id) {
			if m["note"] == nil && m["reset"] == nil {
				got = append(got, m["text"].(string))
			}
		}
		if len(got) >= want {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("wanted %d lines from %s, got %q", want, id, got)
	return nil
}

type list struct {
	mu  sync.Mutex
	cfg config.Config
}

func (l *list) get() config.Config {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cfg
}

func setup(t *testing.T, sources ...config.LogSource) (*streams.Registry, *socket, *list) {
	t.Helper()
	l := &list{cfg: config.Defaults()}
	l.cfg.Features = map[config.Feature]bool{config.FeatureLogs: true}
	l.cfg.LogDirectory = t.TempDir()
	l.cfg.LogSources = sources
	reg := streams.New()
	reg.SetConfig(l.get)
	Register(reg, l.get)
	sock := &socket{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		reg.Detach()
	})
	reg.Attach(ctx, sock)
	return reg, sock, l
}

func open(t *testing.T, reg *streams.Registry, id string, params any) streams.OpenAnswer {
	t.Helper()
	raw, _ := json.Marshal(params)
	return reg.Open(streams.Open{ID: id, Kind: "logs", User: streams.User{Name: "Anna"}, Params: raw, Window: streams.Window})
}

func send(reg *streams.Registry, id string, seq int, data string) {
	raw, _ := json.Marshal(map[string]any{"id": id, "seq": seq, "d": base64.StdEncoding.EncodeToString([]byte(data))})
	reg.Data(raw)
}

func appendTo(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		fmt.Fprintln(f, l)
	}
}

func TestParsers(t *testing.T) {
	e := parseBridgeLine("12:30:01.250  WARN   [watch]    Scans: 2 entries can't be read", Entry{})
	if e.TS != "12:30:01.250" || e.Level != "warn" || e.Origin != "watch" || e.Text != "Scans: 2 entries can't be read" {
		t.Errorf("bridge line: %+v", e)
	}
	if c := parseBridgeLine("  at the second line", e); c.Level != "warn" || c.Origin != "watch" {
		t.Errorf("a continued line: %+v", c)
	}
	for line, level := range map[string]string{
		"2026-10-24 ERROR could not connect": "error",
		"[warning] disk 91% full":            "warn",
		`{"level":"debug","msg":"x"}`:        "debug",
		"GET /index.html 200":                "info",
		"terror is a word, not a level":      "info",
	} {
		if got := parseLine(line, Entry{}).Level; got != level {
			t.Errorf("%q read as %s, want %s", line, got, level)
		}
	}

	j, ok := parseJournal([]byte(`{"__REALTIME_TIMESTAMP":"1761300000000000","PRIORITY":"3","MESSAGE":"failed","SYSLOG_IDENTIFIER":"nginx"}`))
	if !ok || j.Level != "error" || j.Text != "failed" || j.Origin != "nginx" || !strings.HasPrefix(j.TS, "2025-10-24T") {
		t.Errorf("journal: %+v", j)
	}
	if b, _ := parseJournal([]byte(`{"PRIORITY":"6","MESSAGE":[104,105],"_COMM":"x"}`)); b.Text != "hi" || b.Level != "info" {
		t.Errorf("a journal message kept as bytes: %+v", b)
	}

	m, ok := parseMacOS([]byte(`{"timestamp":"2026-10-24 17:01:02.123456+0300","messageType":"Fault","eventMessage":"it broke","processImagePath":"/usr/libexec/thing","subsystem":"com.example","eventType":"logEvent"}`))
	if !ok || m.Level != "error" || m.Origin != "thing com.example" || m.TS != "2026-10-24T14:01:02.123456Z" {
		t.Errorf("unified log: %+v", m)
	}
	if _, ok := parseMacOS([]byte("Filtering the log data using \"process == x\"")); ok {
		t.Error("log stream's own first line read as an entry")
	}

	w, ok := parseEventLog([]byte("\xef\xbb\xbf" + `{"id":7,"ts":"2026-10-24T14:01:02.0000000Z","level":3,"provider":"Disk","event":153,"text":"retried\r\nonce"}`))
	if !ok || w.Level != "warn" || w.Origin != "Disk 153" || w.Text != "retried\nonce" {
		t.Errorf("event log: %+v", w)
	}
}

func TestFilters(t *testing.T) {
	f := Filter{Level: "warn", Query: "DISK"}
	if !f.keeps(Entry{Level: "error", Text: "disk full"}) || f.keeps(Entry{Level: "info", Text: "disk full"}) || f.keeps(Entry{Level: "error", Text: "cpu"}) {
		t.Error("level and text filter")
	}
	if !f.keeps(Entry{Note: true, Text: "now following x"}) {
		t.Error("notes are always kept")
	}
	if !(Filter{Query: "nginx"}).keeps(Entry{Level: "info", Text: "x", Origin: "nginx"}) {
		t.Error("the text filter looks at the origin too")
	}
	if strings.Count(clean(strings.Repeat("é", MaxText)), "") > MaxText+2 || !strings.HasSuffix(clean(strings.Repeat("é", MaxText)), "…") {
		t.Error("a long entry is cut on a character")
	}
}

func TestRefusals(t *testing.T) {
	reg, sock, _ := setup(t)
	if a := open(t, reg, "v1", map[string]any{"source": "nothing"}); a.OK || a.Code != CodeUnknownSource {
		t.Fatalf("an unlisted source: %+v", a)
	}
	if a := open(t, reg, "v2", map[string]any{"source": "bridge", "level": "loud"}); a.OK || a.Code != streams.CodeInvalid {
		t.Fatalf("a level that isn't one: %+v", a)
	}
	// The bridge's own log, before it was written.
	if a := open(t, reg, "v3", map[string]any{"source": "bridge"}); !a.OK {
		t.Fatalf("refused: %+v", a)
	}
	if c := sock.await(t, "stream_close", "v3"); c["code"] != CodeSourceUnavailable {
		t.Fatalf("closed %v", c)
	}
}

func TestBridgeLog(t *testing.T) {
	reg, sock, l := setup(t)
	path := l.get().LogFile()
	appendTo(t, path, "12:00:00.000  INFO   [bridge]   started", "12:00:01.000  ERROR  [sync]     failed", "12:00:02.000  OK     [sync]     sent 3")
	if a := open(t, reg, "v1", map[string]any{"source": "bridge", "lines": 2, "follow": false}); !a.OK {
		t.Fatalf("refused: %+v", a)
	}
	if c := sock.await(t, "stream_close", "v1"); c["code"] != streams.CodeDone {
		t.Fatalf("closed %v", c)
	}
	got := sock.lines("v1")
	if len(got) != 2 || got[0]["level"] != "error" || got[0]["origin"] != "sync" || got[1]["text"] != "sent 3" {
		t.Fatalf("got %v", got)
	}
}

func TestFileView(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	for i := 1; i <= 10; i++ {
		appendTo(t, path, fmt.Sprintf("line %d", i))
	}
	appendTo(t, path, "ERROR line 11")
	src := config.LogSource{ID: "file-1", Name: "App", Kind: config.LogFile, Target: path}
	reg, sock, l := setup(t, src)

	if a := open(t, reg, "v1", map[string]any{"source": "file-1", "lines": 3}); !a.OK {
		t.Fatalf("refused: %+v", a)
	}
	if got := sock.texts(t, "v1", 3); strings.Join(got, "|") != "line 9|line 10|ERROR line 11" {
		t.Fatalf("its tail: %q", got)
	}

	// What is written next arrives, a line cut in two once it is whole.
	appendTo(t, path, "line 12")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	fmt.Fprint(f, "line ")
	f.Close()
	time.Sleep(60 * time.Millisecond)
	appendTo(t, path, "13")
	if got := sock.texts(t, "v1", 5); got[3] != "line 12" || got[4] != "line 13" {
		t.Fatalf("followed: %q", got)
	}

	// A new filter reads it again under the filter.
	send(reg, "v1", 0, `{"level":"error","lines":20}`+"\n")
	deadline := time.Now().Add(5 * time.Second)
	for {
		all := sock.lines("v1")
		reset := -1
		for i, m := range all {
			if m["reset"] == true {
				reset = i
			}
		}
		if reset >= 0 && len(all) > reset+1 {
			after := all[reset+1:]
			if len(after) != 1 || after[0]["text"] != "ERROR line 11" {
				t.Fatalf("after the new filter: %v", after)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no reset: %v", all)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Cut back to nothing, it is read from its start.
	send(reg, "v1", 1, `{"level":"debug","lines":0}`+"\n")
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(path, []byte("fresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for !strings.Contains(fmt.Sprint(sock.lines("v1")), "fresh") {
		if time.Now().After(deadline) {
			t.Fatalf("not read after it was cut back: %v", sock.lines("v1"))
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Taken off the list, the view ends.
	l.mu.Lock()
	l.cfg.LogSources = nil
	l.mu.Unlock()
	if c := sock.await(t, "stream_close", "v1"); c["code"] != CodeRemoved {
		t.Fatalf("closed %v", c)
	}
}

func TestNewerFile(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "app-1.log")
	appendTo(t, old, "old one")
	src := config.LogSource{ID: "file-1", Name: "App", Kind: config.LogFile, Target: filepath.Join(dir, "*.log")}
	reg, sock, _ := setup(t, src)
	if a := open(t, reg, "v1", map[string]any{"source": "file-1"}); !a.OK {
		t.Fatalf("refused: %+v", a)
	}
	sock.texts(t, "v1", 1)
	time.Sleep(20 * time.Millisecond)
	next := filepath.Join(dir, "app-2.log")
	appendTo(t, next, "new one")
	future := time.Now().Add(time.Minute)
	_ = os.Chtimes(next, future, future)
	if got := sock.texts(t, "v1", 2); got[1] != "new one" {
		t.Fatalf("got %q", got)
	}
	noted := false
	for _, m := range sock.lines("v1") {
		if m["note"] == true && strings.Contains(m["text"].(string), "app-2.log") {
			noted = true
		}
	}
	if !noted {
		t.Fatal("the switch to the newer file wasn't said")
	}
}

func TestSourcesList(t *testing.T) {
	// A list edited down to nothing: the system's usual logs aren't offered then.
	cfg := config.Config{LogSources: []config.LogSource{}}
	kind := config.LogKindsHere()[len(config.LogKindsHere())-1]
	target := map[config.LogSourceKind]string{config.LogJournal: "nginx.service", config.LogMacOS: `subsystem == "com.example"`, config.LogEventLog: "System", config.LogFile: "/var/log/app.log"}[kind]
	cfg, src, err := cfg.WithLogSource(config.LogSource{Kind: kind, Target: target})
	if err != nil || src.Name == "" || !strings.HasPrefix(src.ID, string(kind)+"-") {
		t.Fatalf("kept %+v, %v", src, err)
	}
	if _, _, err := cfg.WithLogSource(config.LogSource{Kind: kind, Target: target}); err == nil {
		t.Error("the same log listed twice")
	}
	for _, bad := range []config.LogSource{{Kind: config.LogFile, Target: "relative.log"}, {Kind: config.LogFile, Target: "/var/log/[x"}, {Kind: "nope", Target: "x"}} {
		if _, _, err := cfg.WithLogSource(bad); err == nil {
			t.Errorf("%+v was kept", bad)
		}
	}
	if got := cfg.ListedLogSources(); len(got) != 2 || got[0].ID != config.BridgeLogID {
		t.Fatalf("listed %+v", got)
	}
	if next, found := cfg.WithoutLogSource(src.ID); !found || len(next.LogSources) != 0 {
		t.Fatal("not removed")
	}
}

// TestSystemLogLive reads this system's own log for real: run with BRIDGE_LIVE_LOGS=1 (macOS: the unified log).
func TestSystemLogLive(t *testing.T) {
	if os.Getenv("BRIDGE_LIVE_LOGS") == "" || runtime.GOOS != "darwin" {
		t.Skip("set BRIDGE_LIVE_LOGS=1 on a Mac")
	}
	mark := fmt.Sprintf("bridge-live-%d", time.Now().UnixNano())
	src := config.LogSource{ID: "macos-1", Name: "Test", Kind: config.LogMacOS, Target: fmt.Sprintf(`eventMessage CONTAINS "%s"`, mark)}
	reg, sock, _ := setup(t, src)
	if a := open(t, reg, "v1", map[string]any{"source": "macos-1", "lines": 5}); !a.OK {
		t.Fatalf("refused: %+v", a)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		_ = exec.Command("logger", mark+" says hello").Run()
		for _, m := range sock.lines("v1") {
			if strings.Contains(fmt.Sprint(m["text"]), mark) {
				t.Logf("read %v", m)
				return
			}
		}
		if c := sock.last("stream_close", "v1"); c != nil {
			t.Fatalf("closed %v", c)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("never read what logger wrote")
}
