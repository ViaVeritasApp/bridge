package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"

	"bridge/internal/logx"
	"bridge/internal/netx"
	"bridge/internal/platform"
)

// selectPort resolves the bridge port: an explicit flag wins (validated to
// 1-65535), else a valid existing value, else a random free port in [lo,hi]
// announced with randomMsg. Kept general in case configure grows other ports.
func selectPort(cmd *cobra.Command, flag string, existing, lo, hi int, host, randomMsg string) (int, error) {
	if cmd.Flags().Changed(flag) {
		port, _ := cmd.Flags().GetInt(flag)
		if port < 1 || port > 65535 {
			return 0, fmt.Errorf("--%s %d out of range (1-65535)", flag, port)
		}
		return port, nil
	}
	if existing >= 1 && existing <= 65535 {
		return existing, nil
	}
	p, err := netx.RandomFreePort(host, lo, hi)
	if err != nil {
		return 0, err
	}
	logx.Ok("bridge", "%s %d", randomMsg, p)
	return p, nil
}

// restartOrStartService applies a freshly saved config: restart the service,
// falling back to a plain start (it may be stopped, or not installed yet —
// `install` starts it later, not a failure). restartedMsg is logged verbatim
// on a successful restart.
func restartOrStartService(restartedMsg string) {
	if serviceState() == "not_installed" {
		logx.Info("bridge", "saved; it applies once the bridge is installed")
		return
	}
	s, serr := service.New(&program{}, svcConfig)
	if serr != nil {
		return
	}
	if rerr := s.Restart(); rerr != nil {
		if aerr := s.Start(); aerr != nil {
			logx.Info("bridge", "config saved; service not running yet (run `bridge install`): %v", aerr)
		} else {
			logx.Ok("bridge", "service started with new config")
		}
	} else {
		logx.Ok("bridge", "%s", restartedMsg)
	}
}

// reinstallService replaces an existing service registration. On Windows a
// plain Uninstall+Install races: the SCM only *marks* a service for deletion
// (removed once stopped and all handles close), so the next Install fails with
// "already exists". Stop first so the delete completes, then retry Install
// until the SCM drops the old registration. macOS/Linux uninstall is
// synchronous, so the first retry succeeds.
func reinstallService(s service.Service) error {
	// Stopping lets the SCM actually remove a Windows service (marked-for-delete
	// otherwise). platform.StopService also force-terminates a process the
	// graceful stop left alive, so the reinstall doesn't race a lingering
	// listener. A stop error just means it wasn't running — fine.
	if serr := platform.Current().StopService(s, svcConfig.Name); serr != nil {
		logx.Info("bridge", "service not running or already stopped (%v); proceeding to replace", serr)
	}
	if uerr := s.Uninstall(); uerr != nil {
		return fmt.Errorf("uninstall failed: %w", uerr)
	}
	// Windows removes the service asynchronously; poll Install until the old
	// registration is gone (~10s budget; deletion is near-instant once stopped).
	var ierr error
	for i := 0; i < 20; i++ {
		if ierr = s.Install(); ierr == nil {
			return nil
		}
		m := ierr.Error()
		if !strings.Contains(m, "already exists") && !strings.Contains(m, "marked for deletion") {
			return fmt.Errorf("reinstall failed: %w", ierr)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("reinstall failed: %w", ierr)
}

// signalContext returns a context cancelled on Ctrl+C / SIGTERM, used by the
// foreground commands (setup wizard, log tail) for a clean exit.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
