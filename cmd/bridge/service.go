package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kardianos/service"

	"bridge/internal/apisocket"
	"bridge/internal/bridgesrv"
	"bridge/internal/config"
	"bridge/internal/control"
	"bridge/internal/heartbeat"
	"bridge/internal/httpx"
	"bridge/internal/logs"
	"bridge/internal/logx"
	"bridge/internal/platform"
	"bridge/internal/proxy"
	"bridge/internal/selfupdate"
	"bridge/internal/streams"
	"bridge/internal/telemetry"
	"bridge/internal/terminal"
	"bridge/internal/watch"
)

// exitRestart ends the service so its manager starts it again: a version just
// put in place, or the one before after an update that failed. Not a stop on
// purpose, which is what has Windows and launchd start it again too.
const exitRestart = 3

// systemdScript is kardianos's unit with RestartSec=5 in place of its fixed
// 120: an update ends the service on purpose to have it start again.
const systemdScript = `[Unit]
Description={{.Description}}
ConditionFileIsExecutable={{.Path|cmdEscape}}
{{range $i, $dep := .Dependencies}}
{{$dep}} {{end}}

[Service]
StartLimitInterval=5
StartLimitBurst=10
ExecStart={{.Path|cmdEscape}}{{range .Arguments}} {{.|cmd}}{{end}}
{{if .ChRoot}}RootDirectory={{.ChRoot|cmd}}{{end}}
{{if .WorkingDirectory}}WorkingDirectory={{.WorkingDirectory|cmdEscape}}{{end}}
{{if .UserName}}User={{.UserName}}{{end}}
{{if .ReloadSignal}}ExecReload=/bin/kill -{{.ReloadSignal}} "$MAINPID"{{end}}
{{if .PIDFile}}PIDFile={{.PIDFile|cmd}}{{end}}
{{if and .LogOutput .HasOutputFileSupport -}}
StandardOutput=file:{{.LogDirectory}}/{{.Name}}.out
StandardError=file:{{.LogDirectory}}/{{.Name}}.err
{{- end}}
{{if gt .LimitNOFILE -1 }}LimitNOFILE={{.LimitNOFILE}}{{end}}
{{if .Restart}}Restart={{.Restart}}{{end}}
{{if .SuccessExitStatus}}SuccessExitStatus={{.SuccessExitStatus}}{{end}}
RestartSec=5
EnvironmentFile=-/etc/sysconfig/{{.Name}}

{{range $k, $v := .EnvVars -}}
Environment={{$k}}={{$v}}
{{end -}}

[Install]
WantedBy=multi-user.target
`

var svcConfig = &service.Config{
	Name:        "bridge",
	DisplayName: "ViaVeritas Bridge",
	Description: "Connects this computer to ViaVeritas: syncs its folders to Files and does what the organization allows on it.",
	// The installed program, whichever copy registers the service.
	Executable: config.InstalledBinary(),
	// The bare binary opens the setup UI, so the service manager must invoke
	// an explicit subcommand. Embedded into the launchd plist / systemd
	// ExecStart / Windows SCM at install time.
	Arguments: []string{"service"},
	// Auto-restart if the process dies unexpectedly (killed from Task Manager /
	// Activity Monitor / `kill`); an intentional stop (uninstall, reinstall,
	// `sc stop`, Stop()) does NOT restart.
	//
	// Keys are bare strings, not service.* constants: those constants live in
	// kardianos's per-platform files and wouldn't compile on the other GOOS.
	// An unrecognized key is ignored by that platform's service manager.
	Option: service.KeyValue{
		// Windows SCM recovery action; without it a killed service stays down.
		"OnFailure":              "restart",
		"OnFailureDelayDuration": "5s",
		// launchd/systemd restart on exit by default; set explicitly so the
		// intent survives a library default change.
		"KeepAlive":     true,
		"Restart":       "always",
		"SystemdScript": systemdScript,
	},
}

type program struct {
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	updater *selfupdate.Manager
	// begun: it said it started (telemetry), so Stop says it stopped.
	begun atomic.Bool
}

func (p *program) Start(s service.Service) error {
	logx.Info("bridge", "service Start() invoked")
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	go p.runForever(ctx)
	return nil
}

func (p *program) Stop(s service.Service) error {
	logx.Info("bridge", "service Stop() invoked; cancelling context")
	if p.begun.Load() {
		telemetry.Default.End(config.Dir(), "stopped")
	}
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
	logx.Ok("bridge", "shutdown complete")
	return nil
}

// run starts fn in a goroutine Stop waits for.
func (p *program) run(fn func()) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		fn()
	}()
}

func (p *program) runForever(ctx context.Context) {
	logx.Info("bridge", "Bridge starting (pid=%d, goos=%s, goarch=%s)", os.Getpid(), runtime.GOOS, runtime.GOARCH)

	// 022 umask so uploads under the data root land 0644 / dirs 0755 —
	// root-owned but clinic-readable, not locked down by an inherited umask.
	platform.Current().SetServiceUmask()
	if config.Dir() == config.InstallDir() {
		if err := platform.Current().HardenDir(config.InstallDir()); err != nil {
			logx.Warn("bridge", "could not keep other users out of %s: %v", config.InstallDir(), err)
		}
	}

	cfg, ok := waitForPairing(ctx)
	if !ok {
		return
	}
	logx.Ok("config", "loaded from %s", config.Path())
	// A bridge paired before it had a port serves nothing on the network.
	if cfg.BridgePort == 0 {
		if err := ensurePort(&cfg); err == nil {
			if serr := config.Save(cfg); serr != nil {
				logx.Warn("config", "could not keep its port: %v", serr)
			}
		}
	}
	logx.Info("config", "bridgeID=%s api=%s bridgeAddr=%s folders=%d version=%s",
		cfg.BridgeID, cfg.APIBaseURL, cfg.BridgeAddr(), len(cfg.Folders), cfg.Version)
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		logx.Warn("bridge", "no client credentials in config; sync & heartbeat will be rejected (HTTP 401) — re-run `bridge pair <code>`")
	}

	// The config as it is on disk: switches turned here, in the dashboard or by
	// ViaVeritas apply within seconds, without a restart.
	live := config.NewLive(cfg)
	live.OnChange(func(old, next config.Config) {
		if !reflect.DeepEqual(old.Folders, next.Folders) || old.Effective(config.FeatureFiles) != next.Effective(config.FeatureFiles) {
			watch.Nudge()
		}
		if config.SwitchesChanged(old, next) || !reflect.DeepEqual(old.Folders, next.Folders) ||
			old.SessionsRunAs() != next.SessionsRunAs() || old.SessionNotices() != next.SessionNotices() ||
			!reflect.DeepEqual(old.LogSources, next.LogSources) {
			heartbeat.Nudge()
		}
	})
	p.run(func() { live.Run(ctx) })

	// What happens to the bridge itself (telemetry, always told), and its
	// updates: a start right after an update has to reach ViaVeritas, or the
	// version before goes back.
	telemetry.Default.Setup(config.Dir())
	telemetry.Default.Begin(config.Dir())
	p.begun.Store(true)
	p.updater = selfupdate.New(p.restartForUpdate)
	p.updater.OnChange = heartbeat.Nudge
	p.updater.Resume(ctx)
	heartbeat.SetUpdateStatus(p.updater.Status)
	control.OnUpdate(func() error { return p.updater.Now(nil, selfupdate.ByComputer) }, p.updater.Status)
	apisocket.OnConnect(func(context.Context) {
		p.updater.Confirm()
		telemetry.Default.Flush()
	})
	apisocket.Handle("update", p.handleUpdate(live))
	// Terminals and commands from ViaVeritas, as this computer allows them.
	terminal.Register(streams.Default, live.Get)
	// What ViaVeritas reaches through this computer, while connections are on.
	proxy.Register(streams.Default)
	// The logs this computer lets ViaVeritas read, live.
	logs.Register(streams.Default, live.Get)
	p.run(func() { telemetry.Default.Run(ctx, sendEvents) })

	// The dashboard and the CLI ask the running service how it stands.
	p.run(func() {
		if err := control.Serve(ctx); err != nil && ctx.Err() == nil {
			logx.Warn("control", "the dashboard can't reach the service: %v", err)
		}
	})

	// What it says of its computer: the inventory, metrics, live samples and speedtests.
	newMonitoring(live).run(ctx, p)

	logx.Info("bridge", "starting the sync loop (interval=%s) and the heartbeat (every %s)", cfg.ScanInterval(), heartbeat.Interval)
	p.run(func() { p.syncLoop(ctx, live) })
	p.run(func() { p.heartbeatLoop(ctx, live) })

	// The socket the API sends its commands and streams on, and its switches.
	if cfg.OrganizationID != "" {
		logx.Info("bridge", "starting the API socket")
		p.run(func() { apisocket.Run(ctx, live) })
	} else {
		logx.Warn("bridge", "API socket disabled: organization_id missing; re-run `bridge pair <code>` to populate it — changes made in the portal can't reach this bridge until then")
	}

	if cfg.BridgeAddr() != "" {
		logx.Info("bridge", "starting HTTP API supervisor on %s", cfg.BridgeAddr())
		p.run(func() {
			// Supervise relaunches Serve when TLS state flips (cert appears,
			// disappears, or rotates fingerprint). It's the only long-lived
			// listener and is wg-tracked so Stop() drains in-flight transfers
			// via http.Server.Shutdown.
			if serr := bridgesrv.Supervise(ctx, cfg); serr != nil && ctx.Err() == nil {
				logx.Error("bridgesrv", "supervisor exited unexpectedly: %v", serr)
			}
		})
	} else {
		logx.Warn("bridge", "no bridge port configured; HTTP API disabled (run `bridge configure --bridge-port <n>`)")
	}

	// Block until shutdown. Everything owns its lifecycle via the shared ctx;
	// Stop() cancels them, then wg.Wait() blocks for clean exit.
	<-ctx.Done()
}

// waitForPairing is the config once the bridge is paired, read again every
// few seconds until then: a service installed before pairing starts working
// the moment it is paired. false when ctx ends first.
func waitForPairing(ctx context.Context) (config.Config, bool) {
	warned := false
	for {
		cfg, err := config.Load()
		if err == nil && cfg.BridgeID != "" {
			return cfg, true
		}
		if !warned {
			if err != nil {
				logx.Info("config", "no settings yet (%v); waiting for the bridge to be paired", err)
			} else {
				logx.Info("config", "not paired yet; waiting for `bridge pair <code>` or the dashboard")
			}
			warned = true
		}
		select {
		case <-ctx.Done():
			return cfg, false
		case <-time.After(5 * time.Second):
		}
	}
}

// syncLoop scans the folders and syncs the difference every ScanInterval, or
// at once when nudged (a command changed something, a folder was added). With
// no folder, or Files switched off, it waits.
func (p *program) syncLoop(ctx context.Context, live *config.Live) {
	syncURL := httpx.NormalizeURL(live.Get().APIBaseURL, "/bridges/sync")
	client := httpx.Client(30 * time.Second)
	logx.Ok("bridge", "sync loop started (%s)", syncURL)

	off := false
	for {
		cfg := live.Get()
		switch {
		case len(cfg.Folders) == 0:
			heartbeat.SetSyncResult(nil)
		case !cfg.Effective(config.FeatureFiles):
			if !off {
				logx.Info("watch", "files are switched off for this bridge: not syncing its folders")
				off = true
			}
			heartbeat.SetSyncResult(nil)
		default:
			off = false
			files, skipped, _ := watch.LastScan()
			err := watch.ScanOnce(ctx, cfg, client, syncURL)
			nowFiles, nowSkipped, _ := watch.LastScan()
			if heartbeat.SetSyncResult(err) || files != nowFiles || skipped != nowSkipped {
				heartbeat.Nudge()
			}
		}

		select {
		case <-ctx.Done():
			logx.Info("bridge", "sync loop stopping (ctx cancelled)")
			return
		case <-time.After(cfg.ScanInterval()):
		case <-watch.Nudged():
		}
	}
}

// heartbeatLoop tells ViaVeritas how the bridge stands every heartbeat.Interval,
// or at once when nudged (a switch changed, a sync ended differently).
func (p *program) heartbeatLoop(ctx context.Context, live *config.Live) {
	url := httpx.NormalizeURL(live.Get().APIBaseURL, "/bridges/heartbeat")
	client := httpx.Client(10 * time.Second)
	for {
		cfg := live.Get()
		if answer := heartbeat.Send(ctx, cfg, client, url, heartbeat.SyncResult()); answer.OK && p.updater != nil {
			p.updater.Confirm()
			p.updater.Offered(answer.Update)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(heartbeat.Interval):
		case <-heartbeat.Nudged():
		}
	}
}

// handleUpdate is someone in ViaVeritas asking for the update now: it answers
// once the update starts or why it can't, and goes on by itself.
func (p *program) handleUpdate(live *config.Live) apisocket.Handler {
	return func(_ context.Context, data json.RawMessage) any {
		var offer selfupdate.Offer
		if err := json.Unmarshal(data, &offer); err != nil {
			return map[string]any{"ok": false, "code": selfupdate.CodeInvalid, "message": "the offer couldn't be read"}
		}
		if err := p.updater.Now(&offer, selfupdate.ByPortal); err != nil {
			code := "error"
			var refusal *selfupdate.Refusal
			if errors.As(err, &refusal) {
				code = refusal.Code
			}
			return map[string]any{"ok": false, "code": code, "message": err.Error()}
		}
		return map[string]any{"ok": true, "version": offer.Version}
	}
}

// restartForUpdate ends the service for its manager to start it again (exitRestart),
// after its loops end, so the version now in place runs.
func (p *program) restartForUpdate() {
	telemetry.Default.End(config.Dir(), "update")
	if p.cancel != nil {
		p.cancel()
	}
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
	}
	logx.Info("bridge", "ending so the service starts again with the version in place")
	os.Exit(exitRestart)
}

// sendEvents hands telemetry events to ViaVeritas, which answers how many it kept.
func sendEvents(ctx context.Context, events []telemetry.Event) error {
	answer, err := apisocket.EmitAck(ctx, "events", map[string]any{"items": events}, 15*time.Second)
	if err != nil {
		return err
	}
	var kept struct {
		Stored *int `json:"stored"`
	}
	if json.Unmarshal(answer, &kept) != nil || kept.Stored == nil {
		return fmt.Errorf("ViaVeritas didn't keep the events: %s", answer)
	}
	return nil
}

// runServiceProcess is the service-manager entrypoint: runs the kardianos
// service loop (program.Start/Stop). Invoked by the hidden `service`
// subcommand, and by the no-arg fallback for older installs.
func runServiceProcess() error {
	// Service stdout is discarded (macOS) or uncaptured (Windows), so tee logs
	// to a rotating file the `logs` command can tail. Best effort: a
	// permissions failure must not stop the service. Load honors a configured
	// log-dir (Defaults on error).
	svcCfg, _ := config.Load()
	if err := logx.EnableFileLogging(svcCfg.LogFile()); err != nil {
		logx.Warn("bridge", "file logging disabled (%v); logs go to stdout only", err)
	}
	s, err := service.New(&program{}, svcConfig)
	if err != nil {
		return err
	}
	return s.Run()
}
