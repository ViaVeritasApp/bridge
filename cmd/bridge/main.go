// Command bridge serves a clinic's local files to the CRM over the LAN.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"

	"bridge/internal/config"
	"bridge/internal/logx"
	"bridge/internal/platform"
)

func main() {
	root := &cobra.Command{Use: "bridge"}
	root.SilenceErrors = true
	root.SilenceUsage = true

	// Global --config overrides the config path for this invocation. Resolved
	// to absolute so a relative value is taken from the CWD, not the state dir.
	root.PersistentFlags().String("config", "", "path to config.json (default: <exe-dir>/config.json)")
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		v, _ := cmd.Flags().GetString("config")
		if v = strings.TrimSpace(v); v != "" {
			abs, err := filepath.Abs(v)
			if err != nil {
				return fmt.Errorf("resolve --config %q: %w", v, err)
			}
			config.SetPath(abs)
		}
		return nil
	}

	root.AddCommand(&cobra.Command{
		Use:   "install",
		Short: "Register the bridge as a system service",
		RunE:  runInstall,
	})

	configureCmd := &cobra.Command{
		Use:   "configure",
		Short: "Set the bridge port and data root",
		RunE:  runConfigure,
	}
	configureCmd.Flags().Int("bridge-port", 0, "port the bridge HTTP API binds (random free 6500-7500 if unset)")
	configureCmd.Flags().String("data-root", "", "directory the bridge serves (required the first time; kept if omitted later)")
	configureCmd.Flags().Int("scan-interval", 0, "file-watch stat-scan interval in seconds (default 15; kept if omitted)")
	configureCmd.Flags().Bool("include-hidden", false, "include dotfiles/hidden dirs in file-watch (default false)")
	configureCmd.Flags().String("log-dir", "", "directory for service log files (empty resets to <exe-dir>/logs)")
	configureCmd.Flags().Bool("socket-logs", true, "stream the service log to API over a socket (default true)")
	root.AddCommand(configureCmd)

	root.AddCommand(&cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the system service",
		RunE:  runUninstall,
	})

	pairCmd := &cobra.Command{
		Use:   "pair <code>",
		Short: "Claim a pairing code and persist config",
		Args:  cobra.ExactArgs(1),
		RunE:  runPair,
	}
	pairCmd.Flags().String("api-url", "", "API base URL (required)")
	root.AddCommand(pairCmd)

	root.AddCommand(&cobra.Command{
		Use:   "setup",
		Short: "Open a guided web wizard for non-technical operators (configure → install → pair)",
		RunE:  runSetup,
	})

	// Internal entrypoint the OS service manager invokes (see
	// svcConfig.Arguments). Hidden: operators never type this.
	root.AddCommand(&cobra.Command{
		Use:    "service",
		Short:  "Run the bridge as the system service (internal; used by the service manager)",
		Hidden: true,
		RunE:   func(_ *cobra.Command, _ []string) error { return runServiceProcess() },
	})

	root.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Print current config and connectivity",
		RunE:  runStatus,
	})

	root.AddCommand(&cobra.Command{
		Use:   "preview",
		Short: "Show what the next sync would upload (no changes made)",
		RunE:  runPreview,
	})

	root.AddCommand(&cobra.Command{
		Use:   "resync",
		Short: "Clear the sync index so every file is re-sent on the next scan",
		RunE:  runResync,
	})

	logsCmd := &cobra.Command{
		Use:   "logs",
		Short: "Stream the service log in real time (Ctrl+C to stop)",
		RunE:  runLogs,
	}
	logsCmd.Flags().IntP("lines", "n", 50, "lines of existing log to show before following")
	logsCmd.Flags().Bool("follow", true, "keep streaming new log lines (Ctrl+C to stop)")
	root.AddCommand(logsCmd)

	// No subcommand. New installs invoke `service` explicitly, so this path
	// means either (a) a human ran the bare binary → open the setup wizard, or
	// (b) an OLD install whose unit predates the explicit arg → run the service.
	// service.Interactive() reports false only under a service manager
	// (independent of argv), so it distinguishes the two and keeps old
	// deployments working after an in-place binary upgrade.
	if len(os.Args) < 2 {
		if _, err := service.New(&program{}, svcConfig); err == nil && !service.Interactive() {
			if rerr := runServiceProcess(); rerr != nil {
				fmt.Fprintln(os.Stderr, rerr)
				os.Exit(1)
			}
			return
		}
		// Operator double-clicked the binary. Try a native GUI elevation
		// prompt and re-exec elevated; handled=false means headless/SSH/
		// already-root → fall through to the CLI path below.
		if handled, _ := platform.RelaunchElevated(); handled {
			// Elevated child finished (or the user dismissed the prompt) — exit quietly.
			return
		}
		if err := launchSetupUI(); err != nil {
			if !errors.Is(err, platform.ErrNotElevated) {
				logx.Error("bridge", "%v", err)
			}
			os.Exit(1)
		}
		return
	}

	if err := root.Execute(); err != nil {
		if !errors.Is(err, platform.ErrNotElevated) {
			logx.Error("bridge", "%v", err)
		}
		os.Exit(1)
	}
}
