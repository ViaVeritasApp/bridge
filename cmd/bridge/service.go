package main

import (
	"context"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/kardianos/service"

	"bridge/internal/apisocket"
	"bridge/internal/bridgesrv"
	"bridge/internal/config"
	"bridge/internal/heartbeat"
	"bridge/internal/httpx"
	"bridge/internal/logx"
	"bridge/internal/platform"
	"bridge/internal/watch"
)

var svcConfig = &service.Config{
	Name:        "bridge",
	DisplayName: "ViaVeritas Bridge",
	Description: "Keeps a folder in sync with ViaVeritas Files and serves it on the local network.",
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
		// intent survives a library default change. (Linux still waits
		// kardianos's hardcoded RestartSec=120 — accepted.)
		"KeepAlive": true,
		"Restart":   "always",
	},
}

type program struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
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
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
	logx.Ok("bridge", "shutdown complete")
	return nil
}

func (p *program) runForever(ctx context.Context) {
	logx.Info("bridge", "Bridge starting (pid=%d, goos=%s, goarch=%s)", os.Getpid(), runtime.GOOS, runtime.GOARCH)

	// 022 umask so uploads under the data root land 0644 / dirs 0755 —
	// root-owned but clinic-readable, not locked down by an inherited umask.
	platform.Current().SetServiceUmask()

	cfg, err := config.Load()
	if err != nil {
		logx.Error("config", "load failed: %v (path=%s)", err, config.Path())
		return
	}
	logx.Ok("config", "loaded from %s", config.Path())

	bridgeID := cfg.BridgeID
	if bridgeID == "" {
		bridgeID = "<unpaired>"
	}
	logx.Info("config", "bridgeID=%s api=%s bridgeAddr=%s folders=%d version=%s",
		bridgeID, cfg.APIBaseURL, cfg.BridgeAddr(), len(cfg.Folders), cfg.Version)

	if cfg.BridgeID != "" {
		if cfg.ClientID == "" || cfg.ClientSecret == "" {
			logx.Warn("bridge", "no client credentials in config; sync & heartbeat will be rejected (HTTP 401) — re-run `bridge pair <code>`")
		}
		logx.Info("bridge", "starting sync+heartbeat loop (interval=%s, includeHidden=%v)", cfg.ScanInterval(), cfg.IncludeHidden)
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.syncLoop(ctx, cfg)
		}()
	} else {
		logx.Warn("bridge", "BridgeID is empty; skipping sync & heartbeat loop (run `bridge pair <code>` to pair)")
	}

	// The socket the API sends its commands on (moves, renames, deletes made in
	// the portal), and the log stream when SocketLogs is on. Gated on pairing:
	// auth uses the bridge token + organization_id.
	if cfg.BridgeID != "" && cfg.OrganizationID != "" {
		logx.Info("bridge", "starting the API socket (logs streamed: %v)", cfg.SocketLogs)
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			apisocket.Run(ctx, cfg)
		}()
	} else if cfg.BridgeID != "" {
		logx.Warn("bridge", "API socket disabled: organization_id missing; re-run `bridge pair <code>` to populate it — changes made in the portal can't reach this bridge until then")
	}

	if cfg.BridgeAddr() != "" {
		logx.Info("bridge", "starting HTTP API supervisor on %s", cfg.BridgeAddr())
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			// Supervise relaunches Serve when TLS state flips (cert appears,
			// disappears, or rotates fingerprint). It's the only long-lived
			// listener and is wg-tracked so Stop() drains in-flight transfers
			// via http.Server.Shutdown.
			if serr := bridgesrv.Supervise(ctx, cfg); serr != nil && ctx.Err() == nil {
				logx.Error("bridgesrv", "supervisor exited unexpectedly: %v", serr)
			}
		}()
	} else {
		logx.Warn("bridge", "no bridge port configured; HTTP API disabled (run `bridge configure --bridge-port <n>`)")
	}

	// Block until shutdown. syncLoop + the bridgesrv supervisor own their
	// lifecycles via the shared ctx; Stop() cancels them, then wg.Wait() blocks
	// for clean exit.
	<-ctx.Done()
}

// syncLoop is the single service loop: every cfg.ScanInterval() it scans the
// data root, syncs the diff, then heartbeats that sync's outcome. Config is
// reloaded each tick so `configure` edits apply without a restart.
func (p *program) syncLoop(ctx context.Context, cfg config.Config) {
	syncURL := httpx.NormalizeURL(cfg.APIBaseURL, "/bridges/sync")
	hbURL := httpx.NormalizeURL(cfg.APIBaseURL, "/bridges/heartbeat")
	syncClient := httpx.Client(30 * time.Second)
	hbClient := httpx.Client(10 * time.Second)
	logx.Ok("bridge", "loop started (sync=%s heartbeat=%s interval=%s)", syncURL, hbURL, cfg.ScanInterval())

	for {
		live := config.LoadOr(cfg)
		syncErr := watch.ScanOnce(ctx, live, syncClient, syncURL)
		heartbeat.Send(ctx, live, hbClient, hbURL, syncErr)

		select {
		case <-ctx.Done():
			logx.Info("bridge", "loop stopping (ctx cancelled)")
			return
		case <-time.After(live.ScanInterval()):
		case <-watch.Nudged():
		}
	}
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
