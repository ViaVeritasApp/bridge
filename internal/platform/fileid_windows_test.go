//go:build windows

package platform

import (
	"os"
	"path/filepath"
	"testing"
)

// TestComputeFileID checks the Windows file-identity resolver: non-empty ids,
// distinct files differ, and a repeat call is stable.
func TestComputeFileID(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte(p), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	impl := osImpl{}
	ida, ok := impl.ComputeFileID(a, nil)
	if !ok || ida == "" {
		t.Fatalf("ComputeFileID(a) = %q, %v; want a non-empty id", ida, ok)
	}
	idb, ok := impl.ComputeFileID(b, nil)
	if !ok || idb == "" {
		t.Fatalf("ComputeFileID(b) = %q, %v; want a non-empty id", idb, ok)
	}
	if ida == idb {
		t.Errorf("distinct files share a file ID: %q", ida)
	}
	if again, _ := impl.ComputeFileID(a, nil); again != ida {
		t.Errorf("ComputeFileID(a) not stable: %q != %q", again, ida)
	}
}
