package files

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// symlinkContained is the BR-6 defence: a symlink under the data root that
// resolves outside it must be rejected even though the lexical path looks clean.
func TestSymlinkContained(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	t.Run("plain nested path stays contained", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		abs := filepath.Join(root, "sub", "new.txt") // not yet created
		if err := symlinkContained(root, abs); err != nil {
			t.Errorf("contained path rejected: %v", err)
		}
	})
	t.Run("symlink escaping root is rejected", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		abs := filepath.Join(root, "link", "target.txt")
		if err := symlinkContained(root, abs); err == nil {
			t.Error("symlink pointing outside root was accepted; want rejection")
		}
	})
	t.Run("symlink pointing back inside root is allowed", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "link")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		abs := filepath.Join(root, "link", "inside.txt")
		if err := symlinkContained(root, abs); err != nil {
			t.Errorf("in-root symlink target rejected: %v", err)
		}
	})
}

func TestLooksLikeWindowsAbsolute(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{`C:\Windows\System32`, true},
		{"c:/users/x", true},
		{"Z:", true},
		{"/etc/passwd", false},
		{"relative/path.txt", false},
		{"http://example", false},
		{"", false},
	}
	for _, c := range cases {
		if got := looksLikeWindowsAbsolute(c.in); got != c.want {
			t.Errorf("looksLikeWindowsAbsolute(%q) = %v; want %v", c.in, got, c.want)
		}
	}
}
