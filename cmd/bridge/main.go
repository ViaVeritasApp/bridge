// Command bridge keeps a folder in sync with ViaVeritas Files and serves it on the LAN.
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

	installCmd := &cobra.Command{
		Use:   "install",
		Short: "Install the bridge in " + config.InstallDir() + " and run it as a system service",
		RunE:  runInstall,
	}
	installCmd.Flags().Bool("force", false, "replace an installed bridge even when it is newer")
	root.AddCommand(installCmd)

	root.AddCommand(&cobra.Command{
		Use:   "restart",
		Short: "Stop the bridge service and start it again",
		RunE:  runRestart,
	})

	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print this program's version",
		Run:   func(*cobra.Command, []string) { fmt.Println(config.DefaultVersion) },
	})

	configureCmd := &cobra.Command{
		Use:   "configure",
		Short: "Set the bridge's port, scan interval, hidden files and log folder",
		RunE:  runConfigure,
	}
	configureCmd.Flags().Int("bridge-port", 0, "port the bridge HTTP API binds (random free 6500-7500 if unset)")
	configureCmd.Flags().Int("scan-interval", 0, "file-watch stat-scan interval in seconds (default 15; kept if omitted)")
	configureCmd.Flags().Bool("include-hidden", false, "include dotfiles/hidden dirs in file-watch (default false)")
	configureCmd.Flags().String("log-dir", "", "directory for service log files (empty resets to <exe-dir>/logs)")
	root.AddCommand(configureCmd)

	featuresCmd := &cobra.Command{
		Use:   "features",
		Short: "List what this bridge does, and switch each on or off on this computer",
	}
	featuresCmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List its features: on here, in ViaVeritas, and so whether they run",
		Args:  cobra.NoArgs,
		RunE:  runFeatureList,
	})
	featuresCmd.AddCommand(&cobra.Command{
		Use:   "enable <feature>",
		Short: "Let a feature run on this computer (ViaVeritas can still switch it off)",
		Args:  cobra.ExactArgs(1),
		RunE:  runFeatureSet(true),
	})
	featuresCmd.AddCommand(&cobra.Command{
		Use:   "disable <feature>",
		Short: "Stop a feature on this computer, whatever ViaVeritas says",
		Args:  cobra.ExactArgs(1),
		RunE:  runFeatureSet(false),
	})
	root.AddCommand(featuresCmd)
	root.AddCommand(updatesCommand())
	root.AddCommand(logSourcesCommand())

	root.AddCommand(&cobra.Command{
		Use:   "pause",
		Short: "Pause all remote access (terminal, commands, connections into the network, logs) until resumed",
		Args:  cobra.NoArgs,
		RunE:  runPause(true),
	})
	root.AddCommand(&cobra.Command{
		Use:   "resume",
		Short: "Resume remote access, as each feature's switches allow",
		Args:  cobra.NoArgs,
		RunE:  runPause(false),
	})

	sessionsCmd := &cobra.Command{
		Use:   "sessions",
		Short: "List or end what ViaVeritas has open on this computer",
	}
	sessionsCmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List the streams open now, and who opened them",
		Args:  cobra.NoArgs,
		RunE:  runSessionsList,
	})
	sessionsCmd.AddCommand(&cobra.Command{
		Use:   "end <id>",
		Short: "End one of them",
		Args:  cobra.ExactArgs(1),
		RunE:  runSessionsEnd,
	})
	sessionsCmd.AddCommand(&cobra.Command{
		Use:   "run-as <user|system>",
		Short: "Run terminals and commands from ViaVeritas as the person signed in here (user) or the administrator (system)",
		Args:  cobra.ExactArgs(1),
		RunE:  runSessionsRunAs,
	})
	sessionsCmd.AddCommand(&cobra.Command{
		Use:   "notices <on|off>",
		Short: "Say on this computer's screen when ViaVeritas opens a terminal or runs a command",
		Args:  cobra.ExactArgs(1),
		RunE:  runSessionsNotices,
	})
	root.AddCommand(sessionsCmd)

	folderCmd := &cobra.Command{
		Use:   "folder",
		Short: "List, add, rename or remove the folders it syncs",
	}
	folderCmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List the folders it syncs",
		Args:  cobra.NoArgs,
		RunE:  runFolderList,
	})
	folderAdd := &cobra.Command{
		Use:   "add <path>",
		Short: "Sync one more folder, apart from those synced already",
		Args:  cobra.ExactArgs(1),
		RunE:  runFolderAdd,
	}
	folderAdd.Flags().String("name", "", "its name in Files (default: the folder's own)")
	folderCmd.AddCommand(folderAdd)
	folderCmd.AddCommand(&cobra.Command{
		Use:   "rename <name|path> <new name>",
		Short: "Change a folder's name in Files (its files move there, nothing on disk changes)",
		Args:  cobra.ExactArgs(2),
		RunE:  runFolderRename,
	})
	folderCmd.AddCommand(&cobra.Command{
		Use:   "remove <name|path>",
		Short: "Stop syncing a folder (its files leave Files, nothing on disk changes)",
		Args:  cobra.ExactArgs(1),
		RunE:  runFolderRemove,
	})
	root.AddCommand(folderCmd)

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
	pairCmd.Flags().String("api-url", "", "API base URL (default "+config.Defaults().APIBaseURL+")")
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
