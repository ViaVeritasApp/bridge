// Package selfupdate installs new versions of the bridge when someone asks:
// on this computer (the dashboard, `bridge updates now`) or in ViaVeritas (an
// `update` event). ViaVeritas offers the latest release that suits this
// computer (the heartbeat's answer, or the event itself); the bridge takes only a
// release signed with a key compiled into it (keys.go), checks the archive
// against the signed SHA256SUMS.txt, runs the new program once to hear its
// version, then puts it in place of the installed one, keeping the version
// before beside it, and ends so the service manager starts the new one.
//
// The new version has two minutes to reach ViaVeritas, and two starts: past
// either, the version before goes back in place and starts again. What happens
// is kept as telemetry events and said in every heartbeat (Status).
//
// Nothing updates by itself. A developer's build never updates itself.
package selfupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"bridge/internal/config"
	"bridge/internal/logx"
	"bridge/internal/telemetry"
	"bridge/internal/utils"
)

// Offer is the latest release ViaVeritas says suits this computer.
type Offer struct {
	Version      string `json:"version"`
	Asset        string `json:"asset"`
	URL          string `json:"url"`
	SumsURL      string `json:"sums_url"`
	SignatureURL string `json:"signature_url"`
	Size         int64  `json:"size,omitempty"`
	// AskedBy names who asked for it now in ViaVeritas.
	AskedBy string `json:"asked_by,omitempty"`
}

// Who started an update.
const (
	ByPortal   = "portal"
	ByComputer = "computer"
)

// Phases of an update.
const (
	PhaseIdle        = ""
	PhaseDownloading = "downloading"
	PhaseInstalling  = "installing"
	// PhaseRestarting: the new version is in place and must reach ViaVeritas.
	PhaseRestarting = "restarting"
	PhaseFailed     = "failed"
)

// Why an update wasn't started, as the socket's answer and ViaVeritas name it.
const (
	CodeBusy         = "busy"
	CodeNotNewer     = "not_newer"
	CodeDevBuild     = "dev_build"
	CodeUnsigned     = "unsigned_build"
	CodeNotInstalled = "not_installed"
	CodeNoOffer      = "no_offer"
	CodeInvalid      = "invalid"
)

// Refusal is an update not started, and why.
type Refusal struct{ Code, Message string }

func (r *Refusal) Error() string { return r.Message }

// Status is how updating stands: for the heartbeat, the dashboard and the CLI.
type Status struct {
	Phase string `json:"phase"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
	By    string `json:"by,omitempty"`
	// Stage is where it failed: download, verify, check, install or start.
	Stage string `json:"stage,omitempty"`
	Error string `json:"error,omitempty"`
	At    string `json:"at,omitempty"`
	// Available is the newer version ViaVeritas offers, when there is one.
	Available string `json:"available,omitempty"`
}

// state is update-state.json: what outlives a restart.
type state struct {
	Status
	Starts int    `json:"starts,omitempty"`
	Offer  *Offer `json:"offer,omitempty"`
}

const (
	// maxStarts is how often the new version may start without reaching ViaVeritas.
	maxStarts  = 2
	maxSums    = 64 << 10
	maxSig     = 8 << 10
	maxArchive = 256 << 20
	maxBinary  = 256 << 20
	// maxReason is the longest a failure's reason is kept and said.
	maxReason = 400
)

// Manager updates the bridge. New fills it from the config; tests set it by hand.
type Manager struct {
	// Dir holds update-state.json and the updates being downloaded, beside Binary.
	Dir string
	// Binary is the installed program the service runs; Running is this process's.
	Binary, Running string
	Version         string
	GOOS, Arch      string
	Keys            []ed25519.PublicKey
	Client          *http.Client
	// Restart ends the process so the service manager starts it again.
	Restart func()
	// ConfirmWithin is how long the new version has to reach ViaVeritas.
	ConfirmWithin time.Duration
	Events        *telemetry.Store
	// OnChange runs (in a goroutine) when the phase changes: the heartbeat says it at once.
	OnChange func()

	mu        sync.Mutex
	st        state
	busy      bool
	base      context.Context
	confirmed chan struct{}
	once      sync.Once
	saved     string
}

// New is the service's updater: restart ends the process to start the new version.
func New(restart func()) *Manager {
	running, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(running); err == nil {
		running = resolved
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &Manager{
		Dir:     config.InstallDir(),
		Binary:  config.InstalledBinary(),
		Running: running,
		Version: config.DefaultVersion,
		GOOS:    runtime.GOOS,
		Arch:    config.ReleaseArch(),
		Keys:    TrustedKeys(),
		Client: &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" || len(via) > 5 {
				return errors.New("a release is fetched over https only")
			}
			return nil
		}},
		Restart:       restart,
		ConfirmWithin: 2 * time.Minute,
		Events:        telemetry.Default,
	}
}

func (m *Manager) statePath() string { return filepath.Join(m.Dir, "update-state.json") }

// Resume is the service starting: a version just put in place counts this start
// and must reach ViaVeritas (Confirm), or the one before goes back.
func (m *Manager) Resume(ctx context.Context) {
	m.mu.Lock()
	m.base = ctx
	m.confirmed = make(chan struct{})
	m.load()
	_ = os.Remove(failedPath(m.Binary))
	if m.st.Phase != PhaseRestarting {
		m.mu.Unlock()
		return
	}
	switch m.st.To {
	case m.Version:
		m.st.Starts++
		starts := m.st.Starts
		m.save()
		m.mu.Unlock()
		if starts > maxStarts {
			m.rollback("it stopped twice before it reached ViaVeritas")
			return
		}
		logx.Info("selfupdate", "running %s, just put in place: waiting to reach ViaVeritas", m.Version)
		go m.watch(ctx)
	default:
		from, to := m.st.From, m.st.To
		m.mu.Unlock()
		if m.Version == from {
			m.fail("start", "the version before started again in its place")
			return
		}
		// Another version was put in place by hand meanwhile: nothing to finish.
		logx.Info("selfupdate", "%s runs, not %s: forgetting that update", m.Version, to)
		m.setStatus(Status{})
	}
}

// watch takes the new version back when it doesn't reach ViaVeritas in time.
func (m *Manager) watch(ctx context.Context) {
	timer := time.NewTimer(m.ConfirmWithin)
	defer timer.Stop()
	select {
	case <-m.confirmed:
	case <-ctx.Done():
	case <-timer.C:
		m.rollback("it didn't reach ViaVeritas within two minutes")
	}
}

// Confirm says the bridge reached ViaVeritas (a heartbeat went through, or the
// socket connected): a version just put in place is installed for good.
func (m *Manager) Confirm() {
	m.mu.Lock()
	if m.st.Phase != PhaseRestarting || m.st.To != m.Version {
		m.mu.Unlock()
		return
	}
	from, to, by := m.st.From, m.st.To, m.st.By
	m.st.Status, m.st.Starts = Status{}, 0
	m.save()
	if m.confirmed != nil {
		m.once.Do(func() { close(m.confirmed) })
	}
	m.mu.Unlock()
	_ = os.RemoveAll(filepath.Join(m.Dir, "updates"))
	m.Events.Record(telemetry.UpdateInstalled, map[string]any{"from": from, "to": to, "by": by})
	logx.Ok("selfupdate", "updated from %s to %s", from, to)
}

// Offered keeps ViaVeritas's offer, from a heartbeat's answer (nil: nothing
// newer), for the dashboard, `bridge updates` and an update asked for here. It
// never installs it: someone asks, here or in ViaVeritas.
func (m *Manager) Offered(offer *Offer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !reflect.DeepEqual(m.st.Offer, offer) {
		m.st.Offer = offer
		m.save()
	}
}

// Now starts an update someone asked for: in ViaVeritas (offer is theirs) or on
// this computer (offer nil: the last one ViaVeritas made). It answers at once;
// what follows is in Status and the events.
func (m *Manager) Now(offer *Offer, by string) error {
	if offer == nil {
		m.mu.Lock()
		if m.st.Offer != nil {
			o := *m.st.Offer
			offer = &o
		}
		m.mu.Unlock()
	}
	if offer == nil {
		return &Refusal{CodeNoOffer, "no newer version is known yet: ViaVeritas says when there is one"}
	}
	m.mu.Lock()
	err := m.refusal(*offer)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	go func() { _ = m.Apply(m.context(), *offer, by) }()
	return nil
}

// refusal is why offer can't be installed now, or nil. Holds m.mu.
func (m *Manager) refusal(offer Offer) error {
	switch {
	case m.busy || m.st.Phase == PhaseRestarting:
		return &Refusal{CodeBusy, "it is updating already"}
	case len(m.Keys) == 0:
		return &Refusal{CodeUnsigned, "this build can't check a release's signature, so it doesn't update itself: install the new version by hand"}
	case !config.IsRelease(m.Version):
		return &Refusal{CodeDevBuild, "a developer's build doesn't update itself"}
	case !sameFile(m.Running, m.Binary):
		return &Refusal{CodeNotInstalled, "only the bridge installed as a service updates itself"}
	case !config.IsRelease(offer.Version) || offer.Asset == "" || !isHTTPS(offer.URL) || !isHTTPS(offer.SumsURL) || !isHTTPS(offer.SignatureURL):
		return &Refusal{CodeInvalid, "the offer doesn't name a release that can be fetched"}
	case config.CompareVersions(offer.Version, m.Version) <= 0:
		return &Refusal{CodeNotNewer, fmt.Sprintf("it runs %s already", m.Version)}
	}
	return nil
}

// Apply installs offer and restarts into it; on a failure the running version
// stays, and the failure is kept.
func (m *Manager) Apply(ctx context.Context, offer Offer, by string) error {
	m.mu.Lock()
	if err := m.refusal(offer); err != nil {
		m.mu.Unlock()
		return err
	}
	m.busy = true
	from := m.Version
	m.st.Status = Status{Phase: PhaseDownloading, From: from, To: offer.Version, By: by, At: now()}
	m.save()
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.busy = false
		m.mu.Unlock()
	}()

	detail := map[string]any{"from": from, "to": offer.Version, "by": by}
	if offer.AskedBy != "" {
		detail["asked_by"] = offer.AskedBy
	}
	m.Events.Record(telemetry.UpdateStarted, detail)
	logx.Info("selfupdate", "updating from %s to %s (%s)", from, offer.Version, by)

	if stage, err := m.install(ctx, offer); err != nil {
		_ = os.RemoveAll(filepath.Join(m.Dir, "updates"))
		m.fail(stage, err.Error())
		return err
	}
	m.mu.Lock()
	m.st.Status, m.st.Starts = Status{Phase: PhaseRestarting, From: from, To: offer.Version, By: by, At: now()}, 0
	m.save()
	m.mu.Unlock()
	logx.Ok("selfupdate", "%s is in place: starting it", offer.Version)
	if m.Restart != nil {
		m.Restart()
	}
	return nil
}

// install fetches, checks and puts the release in place: on a failure, the
// stage it failed at.
func (m *Manager) install(ctx context.Context, offer Offer) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()

	dir := filepath.Join(m.Dir, "updates", offer.Version)
	_ = os.RemoveAll(filepath.Join(m.Dir, "updates"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "download", err
	}
	sums, err := m.fetch(ctx, offer.SumsURL, maxSums)
	if err != nil {
		return "download", fmt.Errorf("the release's checksums: %w", err)
	}
	signature, err := m.fetch(ctx, offer.SignatureURL, maxSig)
	if err != nil {
		return "download", fmt.Errorf("the release's signature: %w", err)
	}
	if err := VerifySums(sums, signature, m.Keys); err != nil {
		return "verify", err
	}
	if err := checkAsset(offer.Asset, offer.Version, m.GOOS, m.Arch); err != nil {
		return "verify", err
	}
	want := ParseSums(sums)[offer.Asset]
	if want == "" {
		return "verify", fmt.Errorf("the signed checksums don't name %s", offer.Asset)
	}
	archive := filepath.Join(dir, offer.Asset)
	if stage, err := m.download(ctx, offer.URL, archive, want); err != nil {
		return stage, err
	}
	bin := filepath.Join(dir, binName(m.GOOS))
	if err := extract(archive, binName(m.GOOS), bin, maxBinary); err != nil {
		return "verify", err
	}
	if err := check(ctx, bin, offer.Version); err != nil {
		return "check", err
	}
	m.setPhase(PhaseInstalling)
	if err := m.swap(bin); err != nil {
		return "install", err
	}
	return "", nil
}

// swap puts bin in place of the installed program, the one before kept beside
// it. A running program can be renamed on every system (not replaced on Windows).
func (m *Manager) swap(bin string) error {
	prev := previousPath(m.Binary)
	if err := os.Remove(prev); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the version kept from before: %w", err)
	}
	if err := renameRetry(m.Binary, prev); err != nil {
		return fmt.Errorf("set the running version aside: %w", err)
	}
	if err := renameRetry(bin, m.Binary); err != nil {
		_ = renameRetry(prev, m.Binary)
		return fmt.Errorf("put the new version in place: %w", err)
	}
	return nil
}

// rollback puts the version before back in place and starts it again.
func (m *Manager) rollback(reason string) {
	prev, failed := previousPath(m.Binary), failedPath(m.Binary)
	if _, err := os.Stat(prev); err != nil {
		m.fail("start", reason+"; the version before wasn't kept to go back to")
		return
	}
	_ = os.Remove(failed)
	if err := renameRetry(m.Binary, failed); err != nil {
		m.fail("start", fmt.Sprintf("%s; it couldn't be set aside: %v", reason, err))
		return
	}
	if err := renameRetry(prev, m.Binary); err != nil {
		_ = renameRetry(failed, m.Binary)
		m.fail("start", fmt.Sprintf("%s; the version before couldn't go back: %v", reason, err))
		return
	}
	m.mu.Lock()
	from, to, by := m.st.From, m.st.To, m.st.By
	m.st.Status, m.st.Starts = Status{Phase: PhaseFailed, From: from, To: to, By: by, Stage: "start", Error: reason, At: now()}, 0
	m.save()
	m.mu.Unlock()
	m.Events.Record(telemetry.UpdateRolledBack, map[string]any{"from": from, "to": to, "by": by, "reason": reason})
	logx.Warn("selfupdate", "%s went back to %s: %s", to, from, reason)
	if m.Restart != nil {
		m.Restart()
	}
}

// fail keeps a failed update: the running version stays.
func (m *Manager) fail(stage, reason string) {
	if r := []rune(reason); len(r) > maxReason {
		reason = string(r[:maxReason]) + "…"
	}
	m.mu.Lock()
	from, to, by := m.st.From, m.st.To, m.st.By
	m.st.Status, m.st.Starts = Status{Phase: PhaseFailed, From: from, To: to, By: by, Stage: stage, Error: reason, At: now()}, 0
	m.save()
	m.mu.Unlock()
	m.Events.Record(telemetry.UpdateFailed, map[string]any{"from": from, "to": to, "by": by, "stage": stage, "reason": reason})
	logx.Warn("selfupdate", "the update to %s failed (%s): %s", to, stage, reason)
}

// Status is how updating stands now.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.st.Status
	if o := m.st.Offer; o != nil && config.CompareVersions(o.Version, m.Version) > 0 {
		s.Available = o.Version
	}
	return s
}

func (m *Manager) setStatus(s Status) {
	m.mu.Lock()
	m.st.Status = s
	m.save()
	m.mu.Unlock()
}

func (m *Manager) setPhase(phase string) {
	m.mu.Lock()
	m.st.Phase = phase
	m.save()
	m.mu.Unlock()
}

func (m *Manager) context() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.base != nil {
		return m.base
	}
	return context.Background()
}

// load reads update-state.json. Holds m.mu.
func (m *Manager) load() {
	m.st = state{}
	if b, err := os.ReadFile(m.statePath()); err == nil {
		_ = json.Unmarshal(b, &m.st)
	}
	m.saved = m.st.Phase
	// Downloading or installing don't outlive the process that did them.
	if m.st.Phase == PhaseDownloading || m.st.Phase == PhaseInstalling {
		m.st.Status = Status{Phase: PhaseFailed, From: m.st.From, To: m.st.To, By: m.st.By, Stage: "install", Error: "the bridge stopped before the update finished", At: m.st.At}
	}
}

// save writes update-state.json, and says when the phase changed. Holds m.mu.
func (m *Manager) save() {
	b, err := json.MarshalIndent(m.st, "", "  ")
	if err == nil {
		err = utils.AtomicWrite(m.statePath(), b, 0o600)
	}
	if err != nil {
		logx.Warn("selfupdate", "could not keep how the update stands: %v", err)
	}
	if m.st.Phase != m.saved {
		m.saved = m.st.Phase
		if m.OnChange != nil {
			go m.OnChange()
		}
	}
}

// fetch reads a small file of the release.
func (m *Manager) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	res, err := m.get(ctx, url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("larger than expected")
	}
	return b, nil
}

// download saves the archive to dst, which must hash to want.
func (m *Manager) download(ctx context.Context, url, dst, want string) (string, error) {
	res, err := m.get(ctx, url)
	if err != nil {
		return "download", fmt.Errorf("the archive: %w", err)
	}
	defer res.Body.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "download", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(res.Body, maxArchive+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "download", fmt.Errorf("the archive: %w", err)
	}
	if n > maxArchive {
		return "download", errors.New("the archive is larger than a release can be")
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "verify", errors.New("the archive doesn't match its signed checksum")
	}
	return "", nil
}

func (m *Manager) get(ctx context.Context, url string) (*http.Response, error) {
	if !isHTTPS(url) {
		return nil, errors.New("a release is fetched over https only")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ViaVeritas-Bridge/"+m.Version)
	res, err := m.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("answered %s", res.Status)
	}
	return res, nil
}

// check runs the new program once: it must start here and say its version.
func check(ctx context.Context, bin, version string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "version").Output()
	if err != nil {
		return fmt.Errorf("the new version doesn't run on this computer: %w", err)
	}
	if got := strings.TrimSpace(string(out)); got != version {
		return fmt.Errorf("the new program says it is %q, not %s", got, version)
	}
	return nil
}

func binName(goos string) string {
	if goos == "windows" {
		return "bridge.exe"
	}
	return "bridge"
}

// previousPath is where the version before is kept: bridge.previous(.exe).
func previousPath(binary string) string { return besides(binary, "previous") }

// failedPath is where a version taken back waits to be removed.
func failedPath(binary string) string { return besides(binary, "failed") }

func besides(binary, word string) string {
	ext := filepath.Ext(binary)
	return strings.TrimSuffix(binary, ext) + "." + word + ext
}

// renameRetry renames, trying again for a while: on Windows a scanner may hold
// a program it just saw written.
func renameRetry(from, to string) error {
	var err error
	for i := 0; i < 8; i++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return err
}

func sameFile(a, b string) bool {
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(sa, sb)
}

func isHTTPS(url string) bool { return strings.HasPrefix(url, "https://") }

func now() string { return time.Now().UTC().Format(time.RFC3339) }
