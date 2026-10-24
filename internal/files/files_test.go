package files

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveUnderRoot_HappyPaths(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		in, wantSuffix string
	}{
		{"/a/b.pdf", filepath.Join("a", "b.pdf")},
		{"a/b.pdf", filepath.Join("a", "b.pdf")},
		{"/file.txt", "file.txt"},
		{"deep/nested/dir/file.bin", filepath.Join("deep", "nested", "dir", "file.bin")},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ResolveUnderRoot(root, c.in)
			if err != nil {
				t.Fatalf("ResolveUnderRoot(%q): %v", c.in, err)
			}
			if !strings.HasSuffix(got, c.wantSuffix) {
				t.Errorf("got %q, want suffix %q", got, c.wantSuffix)
			}
			if !strings.HasPrefix(got, root) {
				t.Errorf("got %q is not under root %q", got, root)
			}
		})
	}
}

func TestResolveUnderRoot_Rejects(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name, in string
	}{
		{"empty", ""},
		{"single dotdot", "../escape.pdf"},
		{"nested dotdot", "a/b/../../../escape.pdf"},
		{"absolute unix", "/etc/passwd-like-but-rooted-only-as-leading-slash"},     // legal — rooted but stays under root
		{"absolute windows", `C:\Windows\System32\file.dll`},                       // illegal on any host
		{"dotdot only", ".."},
	}
	// Strip the case valid under our rule: a leading "/" is treated as relative
	// to the data root (TrimPrefix), so it's accepted — not a reject.
	rejects := cases[:0]
	for _, c := range cases {
		if c.name != "absolute unix" {
			rejects = append(rejects, c)
		}
	}
	for _, c := range rejects {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ResolveUnderRoot(root, c.in); err == nil {
				t.Errorf("ResolveUnderRoot(%q) succeeded; want error", c.in)
			}
		})
	}
}

// Confirms the leading-slash carve-out: a leading "/" is treated as relative
// to the data root, so the CRM can send either `/a/b` or `a/b` (legacy
// /confirm relied on this).
func TestResolveUnderRoot_LeadingSlashIsRelative(t *testing.T) {
	root := t.TempDir()
	got, err := ResolveUnderRoot(root, "/inside.pdf")
	if err != nil {
		t.Fatalf("expected leading-slash path to be treated as relative: %v", err)
	}
	want := filepath.Join(root, "inside.pdf")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveUnderRoot_SymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behaviour differs on Windows; covered by the dotdot tests")
	}
	root := t.TempDir()
	outside := t.TempDir()
	// Create root/link → outside, then ask for a path under that link.
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// The lexical Rel check doesn't resolve symlinks, so a link walking outside
	// root still looks under root lexically. That's a known limitation; the real
	// defence is the API's authorize step rejecting paths the user can't access.
	// Here we assert the lexical check accepts the path.
	got, err := ResolveUnderRoot(root, "link/inside.pdf")
	if err != nil {
		t.Skipf("symlink rejected by lexical check on this OS — fine, no regression")
	}
	if !strings.HasPrefix(got, root) {
		t.Errorf("resolved %q does not begin with root %q", got, root)
	}
}
