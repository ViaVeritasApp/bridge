package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"bridge/internal/config"
	"bridge/internal/logx"
	"bridge/internal/platform"
)

func logSourcesCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "log-sources",
		Short: "List the logs ViaVeritas may read on this computer, beside the bridge's own",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List the logs ViaVeritas may read, with their ids",
		Args:  cobra.NoArgs,
		RunE:  runLogSourcesList,
	})
	add := &cobra.Command{
		Use:   "add <file|journal|macos|eventlog> [target]",
		Short: "Let ViaVeritas read a log: a file (its path or a pattern), the journal (a unit or all), the unified log (a predicate or all), or an event log (its name)",
		Args:  cobra.RangeArgs(1, 2),
		RunE:  runLogSourcesAdd,
	}
	add.Flags().String("name", "", "what it is, as ViaVeritas shows it")
	cmd.AddCommand(add)
	cmd.AddCommand(&cobra.Command{
		Use:   "remove <id>",
		Short: "Stop ViaVeritas reading it: a view of it open now ends",
		Args:  cobra.ExactArgs(1),
		RunE:  runLogSourcesRemove,
	})
	return cmd
}

func runLogSourcesList(_ *cobra.Command, _ []string) error {
	if err := platform.Require("reading which logs ViaVeritas may read"); err != nil {
		return err
	}
	cfg := config.LoadOrDefaults()
	fmt.Printf("%-18s %-10s %-28s %s\n", "ID", "KIND", "NAME", "READS")
	for _, s := range cfg.ListedLogSources() {
		reads := s.Target
		switch {
		case s.Kind == config.LogBridge:
			reads = cfg.LogFile()
		case reads == "":
			reads = "everything"
		}
		fmt.Printf("%-18s %-10s %-28s %s\n", s.ID, s.Kind, s.Name, reads)
	}
	fmt.Printf("\nLogs: %s.\n", featureState(cfg, config.FeatureLogs))
	return nil
}

func runLogSourcesAdd(cmd *cobra.Command, args []string) error {
	if err := platform.Require("changing which logs ViaVeritas may read"); err != nil {
		return err
	}
	name, _ := cmd.Flags().GetString("name")
	src := config.LogSource{Kind: config.LogSourceKind(strings.ToLower(args[0])), Name: name}
	if len(args) > 1 {
		src.Target = args[1]
	}
	cfg, kept, err := config.LoadOrDefaults().WithLogSource(src)
	if err != nil {
		return err
	}
	if err := config.Save(cfg); err != nil {
		return err
	}
	logx.Ok("bridge", "ViaVeritas may read %s (%s)", kept.Name, kept.ID)
	if !cfg.Allows(config.FeatureLogs) {
		logx.Info("bridge", "logs are off on this computer: `bridge features enable logs` lets ViaVeritas read them")
	}
	return nil
}

func runLogSourcesRemove(_ *cobra.Command, args []string) error {
	if err := platform.Require("changing which logs ViaVeritas may read"); err != nil {
		return err
	}
	if args[0] == config.BridgeLogID {
		return fmt.Errorf("the bridge's own log is always listed: switching logs off stops ViaVeritas reading it")
	}
	cfg, found := config.LoadOrDefaults().WithoutLogSource(args[0])
	if !found {
		return fmt.Errorf("no log is listed as %q: `bridge log-sources list` shows their ids", args[0])
	}
	if err := config.Save(cfg); err != nil {
		return err
	}
	logx.Ok("bridge", "ViaVeritas may no longer read %s", args[0])
	return nil
}
