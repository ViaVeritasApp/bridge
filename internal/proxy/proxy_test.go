package proxy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
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
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m := s.last(name, id); m != nil {
			return m
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %s for %s; output so far: %q", name, id, s.output(id))
	return nil
}

// list is the computer's config, changed by a test while streams are open.
type list struct {
	mu  sync.Mutex
	cfg config.Config
}

func (l *list) get() config.Config {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cfg
}

func setup(t *testing.T) (*streams.Registry, *socket, *list) {
	t.Helper()
	l := &list{cfg: config.Defaults()}
	l.cfg.Features = map[config.Feature]bool{config.FeatureProxy: true}
	reg := streams.New()
	reg.SetConfig(l.get)
	Register(reg)
	sock := &socket{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		reg.Detach()
	})
	reg.Attach(ctx, sock)
	return reg, sock, l
}

func open(t *testing.T, reg *streams.Registry, id, kind string, params any) streams.OpenAnswer {
	t.Helper()
	raw, _ := json.Marshal(params)
	return reg.Open(streams.Open{ID: id, Kind: kind, User: streams.User{Name: "Anna"}, Params: raw, Window: streams.Window})
}

func send(reg *streams.Registry, id string, seq int, data string) {
	raw, _ := json.Marshal(map[string]any{"id": id, "seq": seq, "d": base64.StdEncoding.EncodeToString([]byte(data))})
	reg.Data(raw)
}

func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, p, _ := net.SplitHostPort(addr)
	port, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// head splits an HTTP stream's output into its head line and the body.
func head(t *testing.T, out string) (Head, string) {
	t.Helper()
	line, body, ok := strings.Cut(out, "\n")
	if !ok {
		t.Fatalf("no head line in %q", out)
	}
	var h Head
	if err := json.Unmarshal([]byte(line), &h); err != nil {
		t.Fatalf("head %q: %v", line, err)
	}
	return h, body
}

// Anything is reached while the feature runs: only a request that can't be read is refused.
func TestOnlyBadRequestsAreRefused(t *testing.T) {
	reg, _, _ := setup(t)
	for i, params := range []map[string]any{
		{"method": "CONNECT", "url": "https://10.0.0.5/"},
		{"method": "GET", "url": "https://user:secret@10.0.0.5/"},
		{"method": "GET", "url": "ftp://10.0.0.5/"},
		{"method": "GET", "url": "https://10.0.0.5/", "tls_pin": "not-a-pin"},
	} {
		if a := open(t, reg, "h"+strconv.Itoa(i), "http", params); a.OK || a.Code != streams.CodeInvalid {
			t.Fatalf("%v: answer %+v, want %s", params, a, streams.CodeInvalid)
		}
	}
	if a := open(t, reg, "t1", "tcp", map[string]any{"host": "", "port": 443}); a.OK || a.Code != streams.CodeInvalid {
		t.Fatalf("answer %+v, want %s", a, streams.CodeInvalid)
	}
	for i, params := range []map[string]any{
		{"method": "GET", "url": "http://169.254.169.254/latest/"},
		{"method": "GET", "url": "http://127.0.0.1:1/"},
		{"method": "GET", "url": "https://printer.invalid/"},
	} {
		if a := open(t, reg, "ok"+strconv.Itoa(i), "http", params); !a.OK {
			t.Fatalf("%v refused: %+v", params, a)
		}
	}
}

func TestSwitchedOffIsRefused(t *testing.T) {
	reg, _, l := setup(t)
	l.mu.Lock()
	l.cfg.Features = map[config.Feature]bool{config.FeatureProxy: false}
	l.mu.Unlock()
	if a := open(t, reg, "s1", "tcp", map[string]any{"host": "10.0.0.5", "port": 443}); a.OK || a.Code != streams.CodeFeatureOff {
		t.Fatalf("answer %+v, want %s", a, streams.CodeFeatureOff)
	}
}

func TestHTTPRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Seen", r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("X-Asked"))
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("got " + string(body)))
	}))
	defer srv.Close()
	reg, sock, _ := setup(t)

	body := "hello"
	a := open(t, reg, "h1", "http", map[string]any{"method": "post", "url": srv.URL + "/things?x=1", "headers": [][2]string{{"X-Asked", "yes"}, {"Host", "evil"}}, "body_bytes": len(body)})
	if !a.OK {
		t.Fatalf("refused: %+v", a)
	}
	send(reg, "h1", 0, body)
	closed := sock.await(t, "stream_close", "h1")
	if closed["code"] != streams.CodeDone {
		t.Fatalf("closed %v", closed)
	}
	h, got := head(t, sock.output("h1"))
	if h.Status != http.StatusCreated || got != "got hello" {
		t.Fatalf("status %d body %q", h.Status, got)
	}
	if seen := h.Headers["X-Seen"]; len(seen) != 1 || seen[0] != "POST /things?x=1 yes" {
		t.Fatalf("the server saw %v", seen)
	}
	if _, ok := h.Headers["Connection"]; ok {
		t.Fatal("a hop's own header came back")
	}
	if !strings.HasPrefix(h.Address, "127.0.0.1:") {
		t.Fatalf("address %q", h.Address)
	}
}

func TestHTTPDoesNotFollowRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()
	reg, sock, _ := setup(t)
	if a := open(t, reg, "h1", "http", map[string]any{"method": "GET", "url": srv.URL}); !a.OK {
		t.Fatalf("refused: %+v", a)
	}
	sock.await(t, "stream_close", "h1")
	h, _ := head(t, sock.output("h1"))
	if h.Status != http.StatusFound || h.Headers["Location"][0] != "http://169.254.169.254/latest/meta-data/" {
		t.Fatalf("head %+v", h)
	}
}

func TestHTTPSAndPins(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("secure"))
	}))
	defer srv.Close()
	sum := sha256.Sum256(srv.Certificate().Raw)
	pin := hex.EncodeToString(sum[:])

	// The test server's certificate is its own: trusted only through the pin the request gives.
	reg, sock, _ := setup(t)
	if a := open(t, reg, "h1", "http", map[string]any{"method": "GET", "url": srv.URL}); !a.OK {
		t.Fatalf("refused: %+v", a)
	}
	if closed := sock.await(t, "stream_close", "h1"); closed["code"] != CodeTLS {
		t.Fatalf("an untrusted certificate closed with %v", closed)
	}

	if a := open(t, reg, "h2", "http", map[string]any{"method": "GET", "url": srv.URL, "tls_pin": strings.ToUpper(pin)}); !a.OK {
		t.Fatalf("refused: %+v", a)
	}
	if closed := sock.await(t, "stream_close", "h2"); closed["code"] != streams.CodeDone {
		t.Fatalf("a pinned certificate closed with %v", closed)
	}
	h, body := head(t, sock.output("h2"))
	if body != "secure" || h.TLS == nil || h.TLS.SHA256 != pin {
		t.Fatalf("head %+v body %q", h, body)
	}

	if a := open(t, reg, "h3", "http", map[string]any{"method": "GET", "url": srv.URL, "tls_pin": strings.Repeat("ab", 32)}); !a.OK {
		t.Fatalf("refused: %+v", a)
	}
	if closed := sock.await(t, "stream_close", "h3"); closed["code"] != CodeTLS {
		t.Fatalf("another pin closed with %v", closed)
	}
}

func TestTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					_, _ = conn.Write([]byte(strings.ToUpper(line)))
				}
			}()
		}
	}()
	port := portOf(t, ln.Addr().String())
	reg, sock, l := setup(t)

	if a := open(t, reg, "t1", "tcp", map[string]any{"host": "127.0.0.1", "port": port}); !a.OK {
		t.Fatalf("refused: %+v", a)
	}
	info := sock.await(t, "stream_info", "t1")
	if !strings.HasPrefix(info["address"].(string), "127.0.0.1:") {
		t.Fatalf("info %v", info)
	}
	send(reg, "t1", 0, "one\n")
	send(reg, "t1", 1, "two\n")
	deadline := time.Now().Add(5 * time.Second)
	for sock.output("t1") != "ONE\nTWO\n" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := sock.output("t1"); got != "ONE\nTWO\n" {
		t.Fatalf("got %q", got)
	}

	// Switched off on this computer, the connection ends.
	l.mu.Lock()
	l.cfg.Features = map[config.Feature]bool{config.FeatureProxy: false}
	next := l.cfg
	l.mu.Unlock()
	reg.Enforce(next)
	if closed := sock.await(t, "stream_close", "t1"); closed["code"] != streams.CodeFeatureOff {
		t.Fatalf("closed %v", closed)
	}
}

func TestTCPUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := portOf(t, ln.Addr().String())
	ln.Close()
	reg, sock, _ := setup(t)
	if a := open(t, reg, "t1", "tcp", map[string]any{"host": "127.0.0.1", "port": port}); !a.OK {
		t.Fatalf("refused: %+v", a)
	}
	if closed := sock.await(t, "stream_close", "t1"); closed["code"] != CodeUnreachable {
		t.Fatalf("closed %v", closed)
	}
}
