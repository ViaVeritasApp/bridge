package logx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The log file holds plain lines whatever stdout is: colour codes would break
// what reads it (the dashboard's Activity).
func TestFileLogIsPlain(t *testing.T) {
	before, prevOut := colorOn, out
	t.Cleanup(func() { colorOn, out = before, prevOut })
	colorOn = true

	path := filepath.Join(t.TempDir(), "bridge.log")
	if err := EnableFileLogging(path); err != nil {
		t.Fatal(err)
	}
	Warn("watch", "2 entries can't be read")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "\x1b[") || !strings.Contains(string(b), "WARN   [watch]") {
		t.Fatalf("not a plain line: %q", b)
	}
}
