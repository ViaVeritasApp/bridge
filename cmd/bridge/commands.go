package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"

	"bridge/internal/config"
	"bridge/internal/control"
	"bridge/internal/heartbeat"
	"bridge/internal/logx"
	"bridge/internal/pair"
	"bridge/internal/platform"
	"bridge/internal/setupui"
	"bridge/internal/watch"
)

func runInstall(cmd *cobra.Command, _ []string) error {
	if err := platform.Require("installing the bridge service"); err != nil {
		return err
	}
	force, _ := cmd.Flags().GetBool("force")
	if err := installProgram(force); err != nil {
		return err
	}
	if cfg, err := config.Load(); err == nil && cfg.BridgePort == 0 {
		if err := ensurePort(&cfg); err != nil {
			return err
		}
		if err := config.Save(cfg); err != nil {
			return err
		}
	}
	s, err := service.New(&program{}, svcConfig)
	if err != nil {
		return err
	}
	if err := s.Install(); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return err
		}
		logx.Warn("bridge", "service already installed (%v); replacing it", err)
		if rerr := reinstallService(s); rerr != nil {
			return fmt.Errorf("replace existing service: %w", rerr)
		}
		logx.Ok("bridge", "replaced existing service installation")
	}

	// Open the Windows Firewall for inbound LAN connections. Best-effort: a
	// failure must not fail the install (operator can add the rule by hand).
	// No-op on macOS/Linux. Runs before Start() so the listener is reachable
	// as soon as it comes up.
	if ferr := platform.Current().EnsureFirewallRule(); ferr != nil {
		logx.Warn("bridge", "could not add the Windows Firewall rule (%v); the "+
			"bridge may be unreachable from the LAN — add an inbound allow rule "+
			"for %q manually, or re-run `bridge install`", ferr, platform.Current().FirewallRuleName())
	} else {
		logx.Ok("bridge", "firewall rule %q is in place", platform.Current().FirewallRuleName())
	}

	if serr := s.Start(); serr != nil {
		logx.Warn("bridge", "service registered but did not start now (it will start on boot, and `pair` will start it): %v", serr)
	} else {
		logx.Ok("bridge", "service started")
	}
	return nil
}

func runConfigure(cmd *cobra.Command, _ []string) error {
	if err := platform.Require("saving the bridge configuration"); err != nil {
		return err
	}

	cfg, _ := config.Load() // Defaults on miss; that's fine for the configure path.
	// Pristine snapshot for change detection. Config has a slice field
	// (AllowedOrigin), so compare with reflect.DeepEqual, not ==.
	before := cfg

	// Bridge HTTP API port: explicit flag wins, else the existing config's
	// port, else a random free port. The only port the bridge binds.
	bridgePort, err := selectPort(cmd, "bridge-port", cfg.BridgePort, 6500, 7500, "0.0.0.0", "selected random free bridge port")
	if err != nil {
		return err
	}
	cfg.BridgePort = bridgePort

	if cmd.Flags().Changed("scan-interval") {
		v, _ := cmd.Flags().GetInt("scan-interval")
		if v < 1 {
			return fmt.Errorf("--scan-interval must be >= 1 second")
		}
		cfg.ScanIntervalSeconds = v
	}
	if cmd.Flags().Changed("include-hidden") {
		v, _ := cmd.Flags().GetBool("include-hidden")
		cfg.IncludeHidden = v
	}
	if cmd.Flags().Changed("log-dir") {
		v, _ := cmd.Flags().GetString("log-dir")
		if v = strings.TrimSpace(v); v != "" {
			abs, aerr := filepath.Abs(v)
			if aerr != nil {
				return fmt.Errorf("resolve --log-dir %q: %w", v, aerr)
			}
			// Its log would sync with the folder, and change at every scan.
			if f, ok := cfg.FolderHolding(abs); ok {
				return fmt.Errorf("--log-dir %s is in %q, which is synced: choose a folder apart from it", abs, f.Name)
			}
			cfg.LogDirectory = abs
		} else {
			cfg.LogDirectory = "" // reset to default (<exe-dir>/logs)
		}
	}

	if err := config.Save(cfg); err != nil {
		return err
	}
	logx.Ok("bridge", "configured port=%d", cfg.BridgePort)

	if !reflect.DeepEqual(cfg, before) {
		restartOrStartService("service restarted to apply new config")
	} else {
		logx.Ok("bridge", "no configuration changes; service left running")
	}
	return nil
}

func runUninstall(_ *cobra.Command, _ []string) error {
	if err := platform.Require("uninstalling the bridge service"); err != nil {
		return err
	}
	s, err := service.New(&program{}, svcConfig)
	if err != nil {
		return err
	}
	if serr := platform.Current().StopService(s, svcConfig.Name); serr != nil {
		// Not running / not loaded is fine — we're tearing it down anyway.
		logx.Info("bridge", "service not running or already stopped (%v); proceeding to uninstall", serr)
	} else {
		logx.Ok("bridge", "service stopped")
	}
	if uerr := s.Uninstall(); uerr != nil {
		return uerr
	}
	logx.Ok("bridge", "service uninstalled")

	// Best-effort firewall-rule cleanup. A leftover rule is harmless (nothing
	// listens once the service is gone), so a failure is logged, not fatal.
	// No-op on macOS/Linux.
	if ferr := platform.Current().RemoveFirewallRule(); ferr != nil {
		logx.Warn("bridge", "could not remove the Windows Firewall rule %q (%v); "+
			"you may delete it manually", platform.Current().FirewallRuleName(), ferr)
	} else {
		logx.Ok("bridge", "firewall rule %q removed", platform.Current().FirewallRuleName())
	}
	return nil
}

func runPair(cmd *cobra.Command, args []string) error {
	apiURL, _ := cmd.Flags().GetString("api-url")
	if strings.TrimSpace(apiURL) == "" {
		apiURL = config.Defaults().APIBaseURL
	}
	if err := platform.Require("saving the bridge configuration"); err != nil {
		return err
	}

	cfg := config.LoadOrDefaults()
	// Its address on the network goes with the claim, so it needs a port first.
	if err := ensurePort(&cfg); err != nil {
		return err
	}

	urls := pair.GuessLANURLs(cfg)

	claimCfg, err := pair.Claim(apiURL, args[0], urls.BridgeBaseURL)
	if err != nil {
		return err
	}
	cfg.APIBaseURL = claimCfg.APIBaseURL
	cfg.BridgeID = claimCfg.BridgeID
	cfg.OrganizationID = claimCfg.OrganizationID
	cfg.ClientID = claimCfg.ClientID
	cfg.ClientSecret = claimCfg.ClientSecret
	cfg.SigningKey = claimCfg.SigningKey
	cfg.AllowedOrigin = claimCfg.AllowedOrigin
	// TLS provisioning info — empty until ACME succeeds; the cert arrives
	// later via heartbeat.
	cfg.TLSDomain = claimCfg.TLSDomain
	cfg.Subdomain = claimCfg.Subdomain
	if err := config.Save(cfg); err != nil {
		return err
	}

	// Heartbeat immediately so the API records this bridge and provisions/pushes
	// TLS now, not after the restarted service's first loop tick.
	heartbeat.SendNow(cfg)

	restartOrStartService("service restarted to apply new config")

	fmt.Printf("Paired. Bridge ID: %s\n", cfg.BridgeID)
	if urls.BridgeBaseURL != "" {
		fmt.Printf("Bridge API at %s\n", urls.BridgeBaseURL)
	}
	if urls.BridgeMdnsBaseURL != "" {
		fmt.Printf("Also reachable via %s (mDNS)\n", urls.BridgeMdnsBaseURL)
	}
	if urls.BridgeTLSBaseURL != "" {
		fmt.Printf("HTTPS URL: %s\n", urls.BridgeTLSBaseURL)
	}
	return nil
}

func runSetup(_ *cobra.Command, _ []string) error { return launchSetupUI() }

// launchSetupUI runs the operator-facing web wizard — behavior of both the
// `setup` subcommand and the bare (no-argument) binary.
func launchSetupUI() error {
	if err := platform.Require("running the setup wizard"); err != nil {
		return err
	}
	// Ctrl+C cleanly stops the local server.
	ctx, stop := signalContext()
	defer stop()
	return setupui.Serve(ctx, setupui.Hooks{ServiceState: serviceState})
}

// serviceState is the service manager's word on the bridge's service.
func serviceState() string {
	s, err := service.New(&program{}, svcConfig)
	if err != nil {
		return "unknown"
	}
	st, err := s.Status()
	if errors.Is(err, service.ErrNotInstalled) {
		return "not_installed"
	}
	if err != nil {
		return "unknown"
	}
	switch st {
	case service.StatusRunning:
		return "running"
	case service.StatusStopped:
		return "stopped"
	}
	return "unknown"
}

// runRestart stops the service and starts it again: it picks up its settings
// and scans its folder at once.
func runRestart(_ *cobra.Command, _ []string) error {
	if err := platform.Require("restarting the bridge service"); err != nil {
		return err
	}
	s, err := service.New(&program{}, svcConfig)
	if err != nil {
		return err
	}
	if serr := platform.Current().StopService(s, svcConfig.Name); serr != nil {
		logx.Info("bridge", "service wasn't running (%v); starting it", serr)
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("start the service: %w", err)
	}
	logx.Ok("bridge", "service restarted")
	return nil
}

func runStatus(_ *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	kv := func(k, v string, quoted bool) {
		key := logx.Key(fmt.Sprintf("%-10s", k))
		var val string
		if quoted {
			val = logx.Str(strconv.Quote(v))
		} else {
			val = logx.Scalar(v)
		}
		fmt.Printf("%s = %s\n", key, val)
	}
	section := func(name string) {
		fmt.Println()
		fmt.Println(logx.Heading("[" + name + "]"))
	}

	linked := "false"
	if cfg.BridgeID != "" {
		linked = "true"
	}
	credentials := "absent"
	if cfg.ClientSecret != "" {
		credentials = "present"
	}
	signingKey := "absent (download links fail until the next heartbeat brings it)"
	if cfg.SigningKey != "" {
		signingKey = "present"
	}
	urls := pair.GuessLANURLs(cfg)

	fmt.Println(logx.Comment("# bridge status"))
	section("pairing")
	kv("linked", linked, false)
	kv("bridge_id", cfg.BridgeID, true)
	kv("credentials", credentials, true)
	kv("signing_key", signingKey, true)
	kv("organization", cfg.OrganizationID, true)
	kv("api", cfg.APIBaseURL, true)
	section("serving")
	kv("bind", cfg.BridgeAddr(), true)
	for _, f := range cfg.Folders {
		kv("folder", f.Name+" = "+f.Path, true)
	}
	kv("lan_url", urls.BridgeBaseURL, true)
	kv("mdns_url", urls.BridgeMdnsBaseURL, true)
	kv("tls_url", urls.BridgeTLSBaseURL, true)
	section("heartbeat")
	kv("last_beat", cfg.LastHeartbeatAt, true)
	section("watch")
	kv("scan_every", cfg.ScanInterval().String(), false)
	kv("hidden", strconv.FormatBool(cfg.IncludeHidden), false)
	section("config")
	kv("path", config.Path(), true)
	kv("logs", cfg.LogFile(), true)
	section("features")
	for _, f := range config.Built {
		kv(string(f), featureState(cfg, f), false)
	}
	if cfg.RemotePaused {
		kv("remote", "paused", false)
	}
	section("service")
	if st, err := control.Ask(); err == nil {
		socket := "not connected"
		if st.Socket.Connected {
			socket = "connected since " + st.Socket.Since
		} else if st.Socket.Error != "" {
			socket = "not connected: " + st.Socket.Error
		}
		kv("socket", socket, true)
		kv("streams", strconv.Itoa(len(st.Streams)), false)
	} else {
		kv("running", "false", false)
	}
	return nil
}

func runPreview(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	plan, err := watch.Preview(cmd.Context(), cfg)
	if err != nil {
		return err
	}

	kv := func(k, v string) {
		fmt.Printf("%s = %s\n", logx.Key(fmt.Sprintf("%-10s", k)), logx.Scalar(v))
	}

	fmt.Println(logx.Comment("# bridge sync preview"))
	fmt.Println()
	fmt.Println(logx.Heading("[preview]"))
	kv("mode", plan.Mode)
	if plan.Mode == "skip" {
		kv("reason", plan.Reason)
		return nil
	}
	index := "present"
	if !plan.IndexFound {
		index = "missing"
	}
	kv("folders", strings.Join(plan.Folders, ", "))
	if len(plan.Unavailable) > 0 {
		kv("unreadable", strings.Join(plan.Unavailable, ", ")+" (their files stay as they were)")
	}
	kv("index", index)
	kv("index_path", watch.IndexPath())
	kv("indexed", strconv.Itoa(plan.Indexed))
	kv("skipped", strconv.Itoa(plan.Skipped))
	kv("created", strconv.Itoa(plan.Created))
	kv("modified", strconv.Itoa(plan.Modified))
	kv("deleted", strconv.Itoa(plan.Deleted))
	kv("total", strconv.Itoa(plan.Total))
	kv("scan_took", plan.ScanTook.String())
	if plan.Mode == "none" && plan.Indexed > 0 {
		fmt.Println()
		fmt.Println(logx.Comment(fmt.Sprintf("# nothing to upload: index.txt already lists all %d file(s).", plan.Indexed)))
		fmt.Println(logx.Comment("# if these files are missing in API, run: bridge resync"))
	}
	if plan.Skipped > 0 {
		fmt.Println()
		fmt.Println(logx.Comment(fmt.Sprintf("# WARNING: %d entr%s under the data root were unreadable and skipped — check folder permissions/ownership.", plan.Skipped, map[bool]string{true: "y", false: "ies"}[plan.Skipped == 1])))
	}
	return nil
}

func runResync(_ *cobra.Command, _ []string) error {
	if err := platform.Require("resetting the sync index"); err != nil {
		return err
	}
	if err := watch.ResetIndex(); err != nil {
		return fmt.Errorf("could not reset the sync index: %w", err)
	}
	logx.Ok("watch", "sync index cleared (%s) — the running service re-sends the full inventory on its next scan", watch.IndexPath())
	fmt.Println("Sync index cleared. The bridge will re-send every file on its next scan (within one scan interval).")
	return nil
}
