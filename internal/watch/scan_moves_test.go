package watch

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"bridge/internal/config"
)

// A command renames on disk and leaves index.txt as it was: the next scan must
// report the move (same file identity, new path) and no deletion, so the sync
// moves the rows in Files rather than deleting them with their tags and notes.
func TestScanReportsACommandsRenameAsAMove(t *testing.T) {
	setupConfigDir(t)
	root := t.TempDir()
	for _, f := range []string{"Scans/a.pdf", "Scans/2026/b.pdf"} {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{Folders: []config.Folder{{ID: "f1", Name: "Data", Path: root}}}

	first, err := scan(context.Background(), cfg, nil)
	if err != nil || first.skip != "" {
		t.Fatalf("first scan: %v %q", err, first.skip)
	}
	persist(first.snapshot)

	if err := os.Rename(filepath.Join(root, "Scans"), filepath.Join(root, "Old")); err != nil {
		t.Fatal(err)
	}

	next, err := scan(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(next.added)
	if len(next.deleted) != 0 {
		t.Errorf("rename reported as deletions: %v", next.deleted)
	}
	if want := []string{"/Data/Old/2026/b.pdf", "/Data/Old/a.pdf"}; len(next.added) != 2 || next.added[0] != want[0] || next.added[1] != want[1] {
		t.Errorf("added = %v; want %v", next.added, want)
	}
	for _, rel := range next.added {
		if next.snapshot[rel].FileID != first.snapshot["/Data/Scans"+rel[len("/Data/Old"):]].FileID {
			t.Errorf("%s lost its file identity", rel)
		}
	}
}

func writeFiles(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, f := range names {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Each folder's files sit under its name, with its id in their identity; a
// folder that isn't there (an unplugged drive) keeps its files, and one renamed
// in the bridge moves them rather than replacing them.
func TestScanSeveralFolders(t *testing.T) {
	setupConfigDir(t)
	a, b := t.TempDir(), t.TempDir()
	writeFiles(t, a, "x.pdf")
	writeFiles(t, b, "Sub/y.pdf")
	cfg := config.Config{Folders: []config.Folder{{ID: "aa", Name: "Scans", Path: a}, {ID: "bb", Name: "Drive", Path: b}}}

	first, err := scan(context.Background(), cfg, nil)
	if err != nil || first.skip != "" {
		t.Fatalf("first scan: %v %q", err, first.skip)
	}
	x, y := first.snapshot["/Scans/x.pdf"], first.snapshot["/Drive/Sub/y.pdf"]
	if !strings.HasPrefix(x.FileID, "aa:") || !strings.HasPrefix(y.FileID, "bb:") {
		t.Fatalf("identities: %q %q (snapshot %v)", x.FileID, y.FileID, first.snapshot)
	}
	persist(first.snapshot)

	gone := cfg
	gone.Folders = []config.Folder{cfg.Folders[0], {ID: "bb", Name: "Drive", Path: filepath.Join(b, "unplugged")}}
	away, err := scan(context.Background(), gone, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(away.deleted) != 0 || away.snapshot["/Drive/Sub/y.pdf"].FileID != y.FileID || len(away.unavailable) != 1 {
		t.Fatalf("an unplugged folder lost its files: deleted %v, unavailable %v", away.deleted, away.unavailable)
	}

	renamed := cfg
	renamed.Folders = []config.Folder{{ID: "aa", Name: "Patients", Path: a}, cfg.Folders[1]}
	moved, err := scan(context.Background(), renamed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(moved.deleted) != 0 || moved.snapshot["/Patients/x.pdf"].FileID != x.FileID {
		t.Fatalf("a rename lost the files: deleted %v, snapshot %v", moved.deleted, moved.snapshot)
	}

	dropped := cfg
	dropped.Folders = cfg.Folders[:1]
	off, err := scan(context.Background(), dropped, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(off.deleted) != 1 || off.deleted[0] != y.FileID {
		t.Fatalf("a folder no longer synced kept its files: deleted %v", off.deleted)
	}
}
