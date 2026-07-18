package watch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bridge/internal/config"
)

// setupConfigDir points config.Dir() (and indexPath) at a temp dir so
// loadIndex/saveIndex don't touch the real install, reusing config.SetPath.
func setupConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	config.SetPath(filepath.Join(dir, "config.json"))
	t.Cleanup(func() { config.SetPath("") }) // reset so other tests see Defaults()
	return dir
}

func TestIndex_MigrateV2ToV3(t *testing.T) {
	dir := setupConfigDir(t)
	// Hand-write a v2 index: header + 5-column lines.
	v2 := "bridge index v2\n" +
		"1024,1700000000,1a2b:ff03,application/pdf,/patients/scan.pdf\n" +
		"512,1700000001,dead:beef,text/plain,/notes.txt\n"
	if err := os.WriteFile(filepath.Join(dir, "index.txt"), []byte(v2), 0o600); err != nil {
		t.Fatalf("write v2 index: %v", err)
	}

	idx, ok := loadIndex()
	if !ok {
		t.Fatalf("loadIndex returned ok=false on a valid v2 index")
	}
	if len(idx.Files) != 2 {
		t.Fatalf("got %d entries, want 2", len(idx.Files))
	}
	for path, e := range idx.Files {
		if e.APIFileID != "" {
			t.Errorf("v2 row %q has unexpected APIFileID=%q (want \"\")", path, e.APIFileID)
		}
	}

	// First saveIndex promotes to v3.
	if err := saveIndex(idx); err != nil {
		t.Fatalf("saveIndex: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "index.txt"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	if lines[0] != "bridge index v3" {
		t.Errorf("header after save = %q, want %q", lines[0], "bridge index v3")
	}
	for _, l := range lines[1:] {
		if l == "" {
			continue
		}
		// SplitN(.., 6) is what loadIndex uses; require exactly 6 parts.
		if got := strings.Count(l, ","); got != 5 { // 5 commas separate 6 cols
			t.Errorf("v3 line %q has %d commas, want 5 (6 columns)", l, got)
		}
	}
}

func TestIndex_AttributionRoundTrip(t *testing.T) {
	setupConfigDir(t)

	idx := index{Files: map[string]entry{
		"/a.pdf": {Size: 1, ModUnix: 100, FileID: "fid-a", MIME: "application/pdf", APIFileID: "api-a"},
		"/b.txt": {Size: 2, ModUnix: 200, FileID: "fid-b", MIME: "text/plain", APIFileID: ""},
	}}
	if err := saveIndex(idx); err != nil {
		t.Fatalf("saveIndex: %v", err)
	}
	got, ok := loadIndex()
	if !ok {
		t.Fatalf("loadIndex ok=false")
	}
	if got.Files["/a.pdf"].APIFileID != "api-a" {
		t.Errorf("apiFileId for /a.pdf round-tripped as %q, want %q", got.Files["/a.pdf"].APIFileID, "api-a")
	}
	if got.Files["/b.txt"].APIFileID != "" {
		t.Errorf("apiFileId for /b.txt round-tripped as %q, want \"\"", got.Files["/b.txt"].APIFileID)
	}
}

func TestApplyAttributions(t *testing.T) {
	snap := map[string]entry{
		"/x.pdf": {Size: 1, APIFileID: ""},
		"/y.pdf": {Size: 2, APIFileID: "stale"},
	}
	applyAttributions(snap, []SyncAttribution{
		{Path: "/x.pdf", FileID: "fresh-x"},
		{Path: "/y.pdf", FileID: "fresh-y"},
		{Path: "/not-in-snap.pdf", FileID: "ignored"},
		{Path: "/z.pdf", FileID: ""}, // empty fileId is skipped
	})
	if snap["/x.pdf"].APIFileID != "fresh-x" {
		t.Errorf("/x.pdf apiFileId = %q, want %q", snap["/x.pdf"].APIFileID, "fresh-x")
	}
	if snap["/y.pdf"].APIFileID != "fresh-y" {
		t.Errorf("/y.pdf apiFileId = %q, want %q", snap["/y.pdf"].APIFileID, "fresh-y")
	}
}
