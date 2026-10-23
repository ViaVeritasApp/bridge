package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bridge/internal/config"
	"bridge/internal/watch"
)

// setup gives a data root and a config dir (where index.txt lives) of their own.
func setup(t *testing.T, files ...string) (config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	config.SetPath(filepath.Join(dir, "config.json"))
	t.Cleanup(func() { config.SetPath("") })

	root := filepath.Join(dir, "data")
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(f, "/")))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return config.Config{Folders: []config.Folder{{ID: "f1", Name: "Data", Path: root}}}, root
}

// writeIndex stands in for a scan that has indexed these files.
func writeIndex(t *testing.T, paths ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("bridge index v3\n")
	for i, p := range paths {
		b.WriteString("1,1700000000,id")
		b.WriteString(string(rune('a' + i)))
		b.WriteString(",text/plain,api")
		b.WriteString(string(rune('a' + i)))
		b.WriteString(",")
		b.WriteString(p)
		b.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(config.Dir(), "index.txt"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(root, rel string) bool {
	_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(rel, "/"))))
	return err == nil
}

// The index stays as it was: the scan that follows finds the move and syncs it,
// whether or not the API got the answer.
func TestMoveFileRenamesAndLeavesTheIndexToTheScan(t *testing.T) {
	cfg, root := setup(t, "/Scans/a.pdf")
	writeIndex(t, "/Data/Scans/a.pdf")

	r := Run(cfg, Command{ID: "1", Type: "move", Moves: []Move{{From: "/Data/Scans/a.pdf", To: "/Data/Archive/b.pdf"}}})
	if len(r.Items) != 1 || !r.Items[0].OK {
		t.Fatalf("move failed: %+v", r.Items)
	}
	if exists(root, "/Scans/a.pdf") || !exists(root, "/Archive/b.pdf") {
		t.Fatal("file not moved on disk")
	}
	if _, _, _, _, _, ok := watch.LookupIndex("/Data/Scans/a.pdf"); !ok {
		t.Error("index changed: the next scan wouldn't sync the move")
	}
	select {
	case <-watch.Nudged():
	default:
		t.Error("no scan asked for after the move")
	}
}

func TestMoveDirectoryMovesEverythingUnderIt(t *testing.T) {
	cfg, root := setup(t, "/Scans/a.pdf", "/Scans/2026/b.pdf", "/Scans/.hidden")

	r := Run(cfg, Command{ID: "2", Type: "move", Moves: []Move{{From: "/Data/Scans", To: "/Data/Old/Scans", Dir: true}}})
	if !r.Items[0].OK {
		t.Fatalf("move failed: %+v", r.Items)
	}
	if !exists(root, "/Old/Scans/2026/b.pdf") || !exists(root, "/Old/Scans/.hidden") || exists(root, "/Scans") {
		t.Fatal("directory not moved whole")
	}
}

func TestMoveIntoHiddenNameRefused(t *testing.T) {
	cfg, root := setup(t, "/Scans/a.pdf")
	r := Run(cfg, Command{ID: "12", Type: "move", Moves: []Move{{From: "/Data/Scans/a.pdf", To: "/Data/Scans/.a.pdf"}, {From: "/Data/Scans", To: "/Data/.archive/Scans", Dir: true}}})
	if r.Items[0].Error != errInvalid || r.Items[1].Error != errInvalid || !exists(root, "/Scans/a.pdf") {
		t.Fatalf("a move under a hidden name went through: %+v", r.Items)
	}
	cfg.IncludeHidden = true
	if r := Run(cfg, Command{ID: "13", Type: "move", Moves: []Move{{From: "/Data/Scans/a.pdf", To: "/Data/Scans/.a.pdf"}}}); !r.Items[0].OK {
		t.Errorf("refused although hidden files are synced: %+v", r.Items[0])
	}
}

func TestMoveNeverOverwrites(t *testing.T) {
	cfg, root := setup(t, "/a.pdf", "/b.pdf")
	r := Run(cfg, Command{ID: "3", Type: "move", Moves: []Move{{From: "/Data/a.pdf", To: "/Data/b.pdf"}}})
	if r.Items[0].OK || r.Items[0].Error != errExists {
		t.Fatalf("want exists, got %+v", r.Items[0])
	}
	if b, _ := os.ReadFile(filepath.Join(root, "b.pdf")); string(b) != "/b.pdf" {
		t.Error("destination was overwritten")
	}
}

func TestMoveMissingAndEscaping(t *testing.T) {
	cfg, _ := setup(t, "/a.pdf")
	r := Run(cfg, Command{ID: "4", Type: "move", Moves: []Move{
		{From: "/Data/gone.pdf", To: "/Data/x.pdf"},
		{From: "/Data/a.pdf", To: "../outside.pdf"},
		{From: "/Data/a.pdf", To: "/Data/"},
	}})
	if r.Items[0].Error != errNotFound {
		t.Errorf("missing source: %+v", r.Items[0])
	}
	if r.Items[1].Error != errInvalid || r.Items[2].Error != errInvalid {
		t.Errorf("escape or root accepted: %+v %+v", r.Items[1], r.Items[2])
	}
}

func TestMoveFolderIntoItselfRefused(t *testing.T) {
	cfg, _ := setup(t, "/Scans/a.pdf")
	r := Run(cfg, Command{ID: "5", Type: "move", Moves: []Move{{From: "/Data/Scans", To: "/Data/Scans/Inner", Dir: true}}})
	if r.Items[0].Error != errInvalid {
		t.Errorf("want invalid, got %+v", r.Items[0])
	}
}

func TestDeleteRemovesFilesAndPrunesEmptyFolders(t *testing.T) {
	cfg, root := setup(t, "/Scans/2026/a.pdf", "/Scans/b.pdf", "/Keep/.hidden", "/Keep/c.pdf")
	writeIndex(t, "/Data/Scans/2026/a.pdf", "/Data/Scans/b.pdf", "/Data/Keep/c.pdf")

	r := Run(cfg, Command{ID: "6", Type: "delete", Paths: []string{"/Data/Scans/2026/a.pdf", "/Data/Scans/b.pdf", "/Data/nothing.pdf"}, Prune: "/Data/Scans"})
	if !r.Items[0].OK || !r.Items[1].OK || r.Items[2].Error != errNotFound {
		t.Fatalf("unexpected results: %+v", r.Items)
	}
	if exists(root, "/Scans") {
		t.Error("emptied folder not pruned")
	}

	r = Run(cfg, Command{ID: "7", Type: "delete", Paths: []string{"/Data/Keep/c.pdf"}, Prune: "/Data/Keep"})
	if !r.Items[0].OK || !exists(root, "/Keep/.hidden") {
		t.Error("a folder still holding a hidden file was pruned")
	}
}

func TestDeleteRefusesFolders(t *testing.T) {
	cfg, root := setup(t, "/Scans/a.pdf")
	r := Run(cfg, Command{ID: "8", Type: "delete", Paths: []string{"/Data/Scans"}})
	if r.Items[0].Error != errInvalid || !exists(root, "/Scans/a.pdf") {
		t.Errorf("a folder was deleted: %+v", r.Items[0])
	}
}

func TestPruneNeverRemovesTheDataRoot(t *testing.T) {
	cfg, root := setup(t)
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	Run(cfg, Command{ID: "9", Type: "delete", Paths: []string{}, Prune: "/Data/"})
	if !exists(root, "/empty") {
		t.Error("pruning the root removed folders elsewhere")
	}
	if _, err := os.Stat(root); err != nil {
		t.Error("data root removed")
	}
}

func TestResentCommandRunsOnce(t *testing.T) {
	cfg, root := setup(t, "/a.pdf")
	first := Run(cfg, Command{ID: "10", Type: "move", Moves: []Move{{From: "/Data/a.pdf", To: "/Data/b.pdf"}}})
	again := Run(cfg, Command{ID: "10", Type: "move", Moves: []Move{{From: "/Data/a.pdf", To: "/Data/b.pdf"}}})
	if !first.Items[0].OK || !again.Items[0].OK {
		t.Fatalf("resent command answered differently: %+v / %+v", first.Items, again.Items)
	}
	if !exists(root, "/b.pdf") {
		t.Error("file not moved")
	}
}

// Each synced folder may be on a disk of its own: a file stays in its folder,
// and the folders themselves are changed in the bridge alone.
func TestSyncedFoldersStayApart(t *testing.T) {
	cfg, root := setup(t, "/a.pdf")
	other := t.TempDir()
	cfg.Folders = append(cfg.Folders, config.Folder{ID: "f2", Name: "Other", Path: other})

	r := Run(cfg, Command{ID: "20", Type: "move", Moves: []Move{
		{From: "/Data/a.pdf", To: "/Other/a.pdf"},
		{From: "/Data", To: "/Archive", Dir: true},
		{From: "/Nowhere/x.pdf", To: "/Data/x.pdf"},
		{From: "/Data/a.pdf", To: "/b.pdf"},
	}})
	if r.Items[0].Error != errInvalid || r.Items[1].Error != errInvalid || r.Items[2].Error != errNotFound || r.Items[3].Error != errInvalid {
		t.Fatalf("unexpected results: %+v", r.Items)
	}
	if !exists(root, "/a.pdf") || exists(other, "/a.pdf") {
		t.Error("a file crossed into another synced folder")
	}

	del := Run(cfg, Command{ID: "21", Type: "delete", Paths: []string{"/Data", "/Nowhere/x.pdf"}})
	if del.Items[0].Error != errInvalid || del.Items[1].Error != errNotFound {
		t.Errorf("unexpected delete results: %+v", del.Items)
	}
}
