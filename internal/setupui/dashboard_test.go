package setupui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"bridge/internal/config"
)

type logsAnswer struct {
	Offset int64  `json:"offset"`
	Text   string `json:"text"`
	Reset  bool   `json:"reset"`
}

func readLogs(t *testing.T, s *srv, query string) logsAnswer {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleLogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs?"+query, nil))
	var a logsAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		t.Fatalf("%s: %v", rec.Body.String(), err)
	}
	return a
}

// The page reads the last lines, then only what was written since, and starts over when the log does.
func TestLogsTailAndFollow(t *testing.T) {
	dir := t.TempDir()
	config.SetPath(filepath.Join(dir, "config.json"))
	t.Cleanup(func() { config.SetPath("") })
	logFile := filepath.Join(dir, "logs", "bridge.log")
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logFile, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &srv{}

	first := readLogs(t, s, "lines=2")
	if first.Text != "two\nthree\n" || first.Offset != 14 {
		t.Fatalf("tail: %+v", first)
	}

	f, _ := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString("four\n")
	f.Close()
	next := readLogs(t, s, "after="+strconv.FormatInt(first.Offset, 10))
	if next.Text != "four\n" || next.Reset {
		t.Fatalf("follow: %+v", next)
	}

	if err := os.WriteFile(logFile, []byte("fresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rotated := readLogs(t, s, "after="+strconv.FormatInt(next.Offset, 10))
	if !rotated.Reset || rotated.Text != "fresh\n" {
		t.Fatalf("rotated: %+v", rotated)
	}
}

func TestIndexCountLeavesOutTheHeader(t *testing.T) {
	dir := t.TempDir()
	config.SetPath(filepath.Join(dir, "config.json"))
	t.Cleanup(func() { config.SetPath("") })
	if err := os.WriteFile(filepath.Join(dir, "index.txt"), []byte("bridge index v3\n1,1,a,t,x,/Scans/a\n1,1,b,t,y,/Scans/2026/b,c\n1,1,c,t,z,/Drive/c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var c indexCount
	n, _, per, ok := c.read()
	if !ok || n != 3 || per["Scans"] != 2 || per["Drive"] != 1 {
		t.Fatalf("got %d %v %v", n, per, ok)
	}
}

// What the setup runs lands in the service's log, as lines Activity reads.
func TestActionsGoToTheLog(t *testing.T) {
	dir := t.TempDir()
	config.SetPath(filepath.Join(dir, "config.json"))
	t.Cleanup(func() { config.SetPath("") })
	recordAction([]string{"folder", "add", "--", "/Users/anna/Scans"}, "22:33:33.128  OK     [bridge]     now syncing", errors.New("exit status 1"))
	b, err := os.ReadFile(filepath.Join(dir, "logs", "bridge.log"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, want := range []string{"INFO   [setup]      bridge folder add -- /Users/anna/Scans", "[bridge]     now syncing", "ERROR  [setup]      bridge folder failed: exit status 1"} {
		if !strings.Contains(text, want) {
			t.Errorf("log lacks %q:\n%s", want, text)
		}
	}
}
