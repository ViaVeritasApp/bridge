//go:build !windows

package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"bridge/internal/telemetry"
)

// A program that says its version, as the bridge's `version` does.
func program(version string) []byte { return []byte("#!/bin/sh\necho " + version + "\n") }

type release struct {
	srv   *httptest.Server
	offer Offer
}

// publish serves a release of a program saying says, signed with signer;
// corrupt serves an archive that doesn't match its checksum.
func publish(t *testing.T, version, says string, signer ed25519.PrivateKey, corrupt bool) release {
	t.Helper()
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	body := program(says)
	_ = tw.WriteHeader(&tar.Header{Name: "bridge", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = gz.Close()

	name := fmt.Sprintf("bridge_%s_linux_amd64.tar.gz", version)
	sum := sha256.Sum256(archive.Bytes())
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(signer, sums)) + "\n"
	served := archive.Bytes()
	if corrupt {
		served = append([]byte{}, served...)
		served[len(served)/2] ^= 0xff
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/sums", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(sums) })
	mux.HandleFunc("/sig", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(sig)) })
	mux.HandleFunc("/archive", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(served) })
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return release{srv: srv, offer: Offer{
		Version: version, Asset: name, URL: srv.URL + "/archive", SumsURL: srv.URL + "/sums", SignatureURL: srv.URL + "/sig",
	}}
}

type fixture struct {
	dir       string
	key       ed25519.PrivateKey
	restarted atomic.Int32
	events    *telemetry.Store
}

func newFixture(t *testing.T, installed string) *fixture {
	t.Helper()
	f := &fixture{dir: t.TempDir(), events: &telemetry.Store{}}
	_, f.key, _ = ed25519.GenerateKey(rand.Reader)
	f.events.Setup(f.dir)
	if err := os.WriteFile(f.binary(), program(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) binary() string { return filepath.Join(f.dir, "bridge") }

// manager is the bridge as version runs it, from the installed program.
func (f *fixture) manager(version string, client *http.Client) *Manager {
	return &Manager{
		Dir: f.dir, Binary: f.binary(), Running: f.binary(), Version: version, GOOS: "linux", Arch: "amd64",
		Keys: []ed25519.PublicKey{f.key.Public().(ed25519.PublicKey)}, Client: client,
		Restart: func() { f.restarted.Add(1) }, ConfirmWithin: time.Second, Events: f.events,
	}
}

func (f *fixture) installed(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.binary())
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(strings.TrimPrefix(string(b), "#!/bin/sh\necho "))
}

func (f *fixture) kinds() []telemetry.Kind {
	var kinds []telemetry.Kind
	for _, ev := range f.events.Pending() {
		kinds = append(kinds, ev.Kind)
	}
	return kinds
}

func TestUpdatesAndConfirms(t *testing.T) {
	f := newFixture(t, "v0.1.0")
	rel := publish(t, "v0.2.0", "v0.2.0", f.key, false)
	m := f.manager("v0.1.0", rel.srv.Client())
	if err := m.Apply(context.Background(), rel.offer, ByPortal); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if f.installed(t) != "v0.2.0" || f.restarted.Load() != 1 || m.Status().Phase != PhaseRestarting {
		t.Fatalf("installed %s, restarted %d, phase %q", f.installed(t), f.restarted.Load(), m.Status().Phase)
	}
	if b, _ := os.ReadFile(previousPath(f.binary())); !strings.Contains(string(b), "v0.1.0") {
		t.Fatal("the version before wasn't kept")
	}

	// The service starts again as v0.2.0 and reaches ViaVeritas.
	next := f.manager("v0.2.0", rel.srv.Client())
	next.Resume(context.Background())
	next.Confirm()
	if s := next.Status(); s.Phase != PhaseIdle {
		t.Fatalf("after confirming: %+v", s)
	}
	if got := f.kinds(); len(got) != 2 || got[0] != telemetry.UpdateStarted || got[1] != telemetry.UpdateInstalled {
		t.Fatalf("events %v", got)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "updates")); !os.IsNotExist(err) {
		t.Fatal("the download stayed")
	}
}

func TestRefusesWhatDoesntCheckOut(t *testing.T) {
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	cases := map[string]struct {
		rel   func(t *testing.T, f *fixture) release
		stage string
	}{
		"signed by another key":  {func(t *testing.T, f *fixture) release { return publish(t, "v0.2.0", "v0.2.0", stranger, false) }, "verify"},
		"a corrupt archive":      {func(t *testing.T, f *fixture) release { return publish(t, "v0.2.0", "v0.2.0", f.key, true) }, "verify"},
		"another version inside": {func(t *testing.T, f *fixture) release { return publish(t, "v0.2.0", "v0.9.9", f.key, false) }, "check"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "v0.1.0")
			rel := c.rel(t, f)
			m := f.manager("v0.1.0", rel.srv.Client())
			if err := m.Apply(context.Background(), rel.offer, ByPortal); err == nil {
				t.Fatal("Apply took it")
			}
			if s := m.Status(); s.Phase != PhaseFailed || s.Stage != c.stage {
				t.Fatalf("status %+v, want failed at %s", s, c.stage)
			}
			if f.installed(t) != "v0.1.0" || f.restarted.Load() != 0 {
				t.Fatal("the running version was touched")
			}
			if got := f.kinds(); len(got) != 2 || got[1] != telemetry.UpdateFailed {
				t.Fatalf("events %v", got)
			}
		})
	}

	t.Run("an asset of another system", func(t *testing.T) {
		f := newFixture(t, "v0.1.0")
		rel := publish(t, "v0.2.0", "v0.2.0", f.key, false)
		m := f.manager("v0.1.0", rel.srv.Client())
		m.Arch = "arm64"
		if err := m.Apply(context.Background(), rel.offer, ByPortal); err == nil || m.Status().Stage != "verify" {
			t.Fatalf("Apply = %v, %+v", err, m.Status())
		}
	})
}

func TestRefusals(t *testing.T) {
	f := newFixture(t, "v0.1.0")
	rel := publish(t, "v0.2.0", "v0.2.0", f.key, false)
	code := func(err error) string {
		var r *Refusal
		if errors.As(err, &r) {
			return r.Code
		}
		return fmt.Sprint(err)
	}
	if got := code(f.manager("v0.2.0", rel.srv.Client()).Now(&rel.offer, ByPortal)); got != CodeNotNewer {
		t.Errorf("same version: %s", got)
	}
	if got := code(f.manager("v0.1.0-dev+abc", rel.srv.Client()).Now(&rel.offer, ByPortal)); got != CodeDevBuild {
		t.Errorf("a developer's build: %s", got)
	}
	unsigned := f.manager("v0.1.0", rel.srv.Client())
	unsigned.Keys = nil
	if got := code(unsigned.Now(&rel.offer, ByPortal)); got != CodeUnsigned {
		t.Errorf("no keys: %s", got)
	}
	elsewhere := f.manager("v0.1.0", rel.srv.Client())
	elsewhere.Running = filepath.Join(t.TempDir(), "bridge")
	if got := code(elsewhere.Now(&rel.offer, ByPortal)); got != CodeNotInstalled {
		t.Errorf("not the installed copy: %s", got)
	}
	if got := code(f.manager("v0.1.0", rel.srv.Client()).Now(nil, ByComputer)); got != CodeNoOffer {
		t.Errorf("no offer known: %s", got)
	}
}

func TestGoesBackWhenItDoesntReachViaVeritas(t *testing.T) {
	f := newFixture(t, "v0.1.0")
	rel := publish(t, "v0.2.0", "v0.2.0", f.key, false)
	if err := f.manager("v0.1.0", rel.srv.Client()).Apply(context.Background(), rel.offer, ByPortal); err != nil {
		t.Fatal(err)
	}

	next := f.manager("v0.2.0", rel.srv.Client())
	next.ConfirmWithin = 100 * time.Millisecond
	next.Resume(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for f.restarted.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if f.installed(t) != "v0.1.0" || f.restarted.Load() != 2 {
		t.Fatalf("installed %s, restarted %d", f.installed(t), f.restarted.Load())
	}
	if s := next.Status(); s.Phase != PhaseFailed || s.Stage != "start" {
		t.Fatalf("status %+v", s)
	}
	if got := f.kinds(); got[len(got)-1] != telemetry.UpdateRolledBack {
		t.Fatalf("events %v", got)
	}

	// The version before starts again: it keeps the failure, and leaves that release alone tonight.
	before := f.manager("v0.1.0", rel.srv.Client())
	before.Resume(context.Background())
	if s := before.Status(); s.Phase != PhaseFailed || s.To != "v0.2.0" {
		t.Fatalf("the version before reads %+v", s)
	}
}

func TestGoesBackAfterTwoStarts(t *testing.T) {
	f := newFixture(t, "v0.1.0")
	rel := publish(t, "v0.2.0", "v0.2.0", f.key, false)
	if err := f.manager("v0.1.0", rel.srv.Client()).Apply(context.Background(), rel.offer, ByPortal); err != nil {
		t.Fatal(err)
	}
	// Two starts that stopped before reaching ViaVeritas; the third goes back.
	for i := 0; i < maxStarts; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		f.manager("v0.2.0", rel.srv.Client()).Resume(ctx)
		cancel()
	}
	f.manager("v0.2.0", rel.srv.Client()).Resume(context.Background())
	if f.installed(t) != "v0.1.0" {
		t.Fatalf("installed %s after three starts", f.installed(t))
	}
}

func TestOnlyWhenAsked(t *testing.T) {
	f := newFixture(t, "v0.1.0")
	rel := publish(t, "v0.2.0", "v0.2.0", f.key, false)

	// Offered: kept and said, never installed by itself.
	m := f.manager("v0.1.0", rel.srv.Client())
	m.Offered(&rel.offer)
	time.Sleep(200 * time.Millisecond)
	if f.restarted.Load() != 0 || m.Status().Available != "v0.2.0" {
		t.Fatalf("restarted %d, status %+v", f.restarted.Load(), m.Status())
	}

	// Asked on the computer: the offer kept is installed.
	if err := m.Now(nil, ByComputer); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for f.restarted.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if f.installed(t) != "v0.2.0" {
		t.Fatalf("installed %s when asked", f.installed(t))
	}
}
