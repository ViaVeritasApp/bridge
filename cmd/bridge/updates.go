package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"bridge/internal/config"
	"bridge/internal/control"
	"bridge/internal/logx"
	"bridge/internal/platform"
	"bridge/internal/selfupdate"
)

func runUpdatesStatus(_ *cobra.Command, _ []string) error {
	// Its settings and the running service are an administrator's to read.
	if err := platform.Require("seeing how the bridge updates"); err != nil {
		return err
	}
	fmt.Printf("Version:  %s\n", config.DefaultVersion)
	fmt.Println("Updates:  only when someone asks: here (`bridge updates now`, the dashboard) or in ViaVeritas")
	st, err := control.Ask()
	if err != nil {
		if errors.Is(err, control.ErrNotRunning) {
			fmt.Println("The service isn't running, so it can't say whether a newer version is out.")
			return nil
		}
		return err
	}
	if st.Update == nil {
		return nil
	}
	u := *st.Update
	if u.Available != "" {
		fmt.Printf("Newer:    %s is out (`bridge updates now` installs it)\n", u.Available)
	}
	switch u.Phase {
	case selfupdate.PhaseDownloading, selfupdate.PhaseInstalling:
		fmt.Printf("Now:      updating to %s (%s)\n", u.To, u.Phase)
	case selfupdate.PhaseRestarting:
		fmt.Printf("Now:      %s was just put in place and is starting\n", u.To)
	case selfupdate.PhaseFailed:
		fmt.Printf("Last:     the update to %s failed (%s): %s\n", u.To, u.Stage, u.Error)
	}
	return nil
}

func runUpdatesNow(_ *cobra.Command, _ []string) error {
	if err := platform.Require("updating the bridge"); err != nil {
		return err
	}
	if err := control.UpdateNow(); err != nil {
		if errors.Is(err, control.ErrNotRunning) {
			return errors.New("the service isn't running: start it, or install the new version by hand")
		}
		return err
	}
	logx.Ok("bridge", "updating: the service restarts with the new version in a minute or two (`bridge updates` says how it goes)")
	return nil
}

func updatesCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "updates",
		Short: "Whether a newer version is out, and update to it now",
		Args:  cobra.NoArgs,
		RunE:  runUpdatesStatus,
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "now",
		Short: "Install the newer version ViaVeritas offers, now",
		Args:  cobra.NoArgs,
		RunE:  runUpdatesNow,
	})
	return cmd
}
