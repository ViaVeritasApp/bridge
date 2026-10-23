package main

import (
	"os"
	"path/filepath"
	"testing"

	"bridge/internal/config"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.2.0", "v0.1.9", 1},
		{"v0.1.0", "0.1.0", 0},
		{"v0.9.12", "v0.10.0", -1},
		{"0.0.0-dev+abc1234", "v0.1.0", 0},
		{"v0.0.1-3-gabc123-dirty", "v9.0.0", 0},
	}
	for _, c := range cases {
		if got := config.CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// A bridge paired from Downloads keeps its pairing, index and certificate once installed.
func TestMigrateStateMovesWhatWasBesideTheProgram(t *testing.T) {
	from, install := t.TempDir(), t.TempDir()
	config.SetInstallDir(install)
	t.Cleanup(func() { config.SetInstallDir("") })
	for name, body := range map[string]string{"config.json": `{"bridge_id":"b1"}`, "index.txt": "bridge index v3\n", "tls/cert.pem": "cert"} {
		p := filepath.Join(from, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := migrateState(from); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.json", "index.txt", "tls/cert.pem"} {
		if _, err := os.Stat(filepath.Join(install, filepath.FromSlash(name))); err != nil {
			t.Errorf("%s not moved: %v", name, err)
		}
	}

	// The installation's own state wins over a copy's, later.
	if err := os.WriteFile(filepath.Join(from, "config.json"), []byte(`{"bridge_id":"other"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := migrateState(from); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(install, "config.json")); string(b) != `{"bridge_id":"b1"}` {
		t.Errorf("installed config overwritten: %s", b)
	}
}
