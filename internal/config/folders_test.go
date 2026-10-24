package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func folderSetup(t *testing.T) (Config, string) {
	t.Helper()
	dir := t.TempDir()
	SetPath(filepath.Join(dir, "state", "config.json"))
	SetInstallDir(filepath.Join(dir, "state"))
	t.Cleanup(func() { SetPath(""); SetInstallDir("") })
	base := filepath.Join(dir, "disk")
	for _, d := range []string{"Scans/2026", "Invoices", "Other/Scans", "state"} {
		if err := os.MkdirAll(filepath.Join(dir, map[bool]string{true: d, false: "disk/" + d}[d == "state"]), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return Config{}, base
}

// Folders never overlap: the same file must not be reachable from two of them.
func TestFoldersDontOverlap(t *testing.T) {
	cfg, base := folderSetup(t)
	cfg, scans, err := cfg.AddFolder(filepath.Join(base, "Scans"), "")
	if err != nil || scans.Name != "Scans" {
		t.Fatalf("add Scans: %+v %v", scans, err)
	}
	for _, p := range []string{filepath.Join(base, "Scans"), filepath.Join(base, "Scans", "2026"), base} {
		if _, _, err := cfg.AddFolder(p, ""); err == nil {
			t.Errorf("%s overlaps Scans but was added", p)
		}
	}
	cfg, other, err := cfg.AddFolder(filepath.Join(base, "Other", "Scans"), "")
	if err != nil || other.Name != "Scans 2" {
		t.Fatalf("a second folder of the same name: %+v %v", other, err)
	}
	if _, _, err := cfg.AddFolder(filepath.Join(base, "Invoices"), "scans 2"); err == nil {
		t.Error("a name taken in another case was given")
	}
	if _, _, err := cfg.AddFolder(filepath.Join(base, "missing"), ""); err == nil {
		t.Error("a folder that isn't there was added")
	}
	if scans.ID == other.ID || scans.ID != folderID(filepath.Join(base, "Scans")) {
		t.Errorf("ids: %q %q", scans.ID, other.ID)
	}
}

// Its settings, secrets included, never sync.
func TestFoldersStayApartFromTheBridgesOwnFiles(t *testing.T) {
	cfg, _ := folderSetup(t)
	if _, _, err := cfg.AddFolder(InstallDir(), ""); err == nil || !strings.Contains(err.Error(), "own files") {
		t.Errorf("the install folder was added: %v", err)
	}
	if _, _, err := cfg.AddFolder(filepath.Dir(InstallDir()), ""); err == nil {
		t.Error("a folder holding the install folder was added")
	}
}

func TestFolderNamesAndPaths(t *testing.T) {
	cfg, base := folderSetup(t)
	cfg, _, _ = cfg.AddFolder(filepath.Join(base, "Scans"), "")
	if CheckFolderName(" pad") == nil {
		t.Error("a name with a space around it passed (RenameFolder trims what is typed)")
	}
	for _, bad := range []string{"", ".hidden", "a/b", `a\b`, "..", "a\x01"} {
		if _, _, err := cfg.RenameFolder("Scans", bad); err == nil {
			t.Errorf("rename to %q accepted", bad)
		}
	}
	cfg, f, err := cfg.RenameFolder("scans", "Patients")
	if err != nil || f.Name != "Patients" || f.ID != folderID(filepath.Join(base, "Scans")) {
		t.Fatalf("rename: %+v %v", f, err)
	}

	got, inner, err := cfg.SplitPath("/Patients/2026/a.pdf")
	if err != nil || got.ID != f.ID || inner != "/2026/a.pdf" {
		t.Errorf("split: %+v %q %v", got, inner, err)
	}
	if _, inner, err := cfg.SplitPath("/Patients"); err != nil || inner != "/" {
		t.Errorf("split the folder itself: %q %v", inner, err)
	}
	if _, _, err := cfg.SplitPath("/Nowhere/a.pdf"); err == nil {
		t.Error("an unknown folder resolved")
	}

	cfg, _, err = cfg.RemoveFolder("Patients")
	if err != nil || len(cfg.Folders) != 0 {
		t.Errorf("remove: %+v %v", cfg.Folders, err)
	}
}
