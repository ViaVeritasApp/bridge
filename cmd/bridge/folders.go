package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"bridge/internal/config"
	"bridge/internal/logx"
	"bridge/internal/netx"
	"bridge/internal/platform"
)

// The service reads the folders at every scan, so a change needs no restart.

// ensurePort gives a bridge with no port yet a free one between 6500 and 7500:
// without it, nothing serves its files on the network.
func ensurePort(cfg *config.Config) error {
	if cfg.BridgePort > 0 {
		return nil
	}
	port, err := netx.RandomFreePort("0.0.0.0", 6500, 7500)
	if err != nil {
		return fmt.Errorf("pick a port for its files: %w", err)
	}
	cfg.BridgePort = port
	logx.Ok("bridge", "it serves its files on port %d", port)
	return nil
}

func runFolderList(_ *cobra.Command, _ []string) error {
	cfg := config.LoadOrDefaults()
	if len(cfg.Folders) == 0 {
		fmt.Println("no folder is synced yet: bridge folder add <path>")
		return nil
	}
	for _, f := range cfg.Folders {
		state := ""
		if info, err := os.Stat(f.Path); err != nil || !info.IsDir() {
			state = "  (isn't there: its files stay as they were)"
		}
		fmt.Printf("%s\t%s%s\n", f.Name, f.Path, state)
	}
	return nil
}

func runFolderAdd(cmd *cobra.Command, args []string) error {
	if err := platform.Require("changing the synced folders"); err != nil {
		return err
	}
	cfg := config.LoadOrDefaults()
	name, _ := cmd.Flags().GetString("name")
	next, f, err := cfg.AddFolder(args[0], name)
	if err != nil {
		return err
	}
	if err := ensurePort(&next); err != nil {
		return err
	}
	if err := config.Save(next); err != nil {
		return err
	}
	logx.Ok("bridge", "now syncing %s as %q; its files reach Files at the next scan", f.Path, f.Name)
	if platform.Protected(f.Path) {
		logx.Warn("bridge", "macOS keeps the service out of %s until %s has Full Disk Access (System Settings › Privacy & Security › Full Disk Access)", f.Path, config.InstalledBinary())
	}
	return nil
}

func runFolderRename(_ *cobra.Command, args []string) error {
	if err := platform.Require("changing the synced folders"); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	next, f, err := cfg.RenameFolder(args[0], args[1])
	if err != nil {
		return err
	}
	if err := config.Save(next); err != nil {
		return err
	}
	logx.Ok("bridge", "%s is now %q in Files; its files move there at the next scan", f.Path, f.Name)
	return nil
}

func runFolderRemove(_ *cobra.Command, args []string) error {
	if err := platform.Require("changing the synced folders"); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	next, f, err := cfg.RemoveFolder(args[0])
	if err != nil {
		return err
	}
	if err := config.Save(next); err != nil {
		return err
	}
	logx.Ok("bridge", "stopped syncing %q (%s): its files leave Files at the next scan, and stay on this computer", f.Name, f.Path)
	return nil
}
