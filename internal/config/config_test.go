package config

import (
	"os"
	"path/filepath"
	"testing"
)

// An upgrade replaces the binary and keeps config.json: the version is the build's.
func TestLoadReportsTheRunningBuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	SetPath(path)
	t.Cleanup(func() { SetPath("") })
	if err := os.WriteFile(path, []byte(`{"version":"v0.0.1","data_root":"/srv/files"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	before := DefaultVersion
	DefaultVersion = "v0.2.0"
	t.Cleanup(func() { DefaultVersion = before })

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != "v0.2.0" {
		t.Errorf("version %q, want the running build's v0.2.0", cfg.Version)
	}
	// A bridge from before folders: its one folder becomes the first.
	if len(cfg.Folders) != 1 || cfg.Folders[0].Path != "/srv/files" || cfg.Folders[0].Name != "files" {
		t.Errorf("data root not carried into folders: %+v", cfg.Folders)
	}
}

// Whichever copy of the program runs, the installation's state is the bridge's;
// beside the program only until an install moves it.
func TestStateLivesInTheInstallation(t *testing.T) {
	install := t.TempDir()
	SetInstallDir(install)
	t.Cleanup(func() { SetInstallDir("") })

	if Dir() != install {
		t.Fatalf("fresh: state in %q, want the installation %q", Dir(), install)
	}

	beside := filepath.Join(ExeDir(), "config.json")
	if err := os.WriteFile(beside, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(beside) })
	if Dir() != ExeDir() {
		t.Errorf("set up beside the program: state in %q, want %q", Dir(), ExeDir())
	}

	if err := os.WriteFile(filepath.Join(install, "config.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if Dir() != install {
		t.Errorf("installed: state in %q, want the installation %q", Dir(), install)
	}
	if got := (Config{}).LogDir(); got != filepath.Join(install, "logs") {
		t.Errorf("logs in %q", got)
	}
}
