package control

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"bridge/internal/config"
)

func TestAskAndEnd(t *testing.T) {
	dir := t.TempDir()
	config.SetPath(filepath.Join(dir, "config.json"))
	t.Cleanup(func() { config.SetPath("") })

	if _, err := Ask(); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("with no service: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("control.json never appeared")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if info, _ := os.Stat(path()); info.Mode().Perm() != 0o600 {
		t.Fatalf("control.json is %v, want 0600", info.Mode().Perm())
	}

	st, err := Ask()
	if err != nil || st.PID != os.Getpid() || st.Version != config.DefaultVersion {
		t.Fatalf("Ask = %+v, %v", st, err)
	}
	if err := End("nope"); err == nil {
		t.Fatal("ended a stream that isn't open")
	}

	// Without the token, nothing answers.
	f, _ := dial()
	res, err := http.Get("http://127.0.0.1:" + strconv.Itoa(f.Port) + "/state")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %s", res.Status)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path()); !os.IsNotExist(err) {
		t.Fatal("control.json outlived the service")
	}
}
