package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"bridge/internal/config"
	"bridge/internal/control"
	"bridge/internal/logx"
	"bridge/internal/platform"
)

// The service reads its switches again within seconds of a change, so none of
// these needs a restart.

// featureState says whether f runs, and if not, which side holds it.
func featureState(cfg config.Config, f config.Feature) string {
	switch {
	case !cfg.Allows(f):
		return "off on this computer"
	case cfg.Paused(f):
		return "paused on this computer"
	case !cfg.PortalAllows(f):
		return "off in ViaVeritas"
	}
	return "on"
}

func parseFeature(s string) (config.Feature, error) {
	f := config.Feature(strings.ToLower(strings.TrimSpace(s)))
	if f.Known() {
		return f, nil
	}
	names := make([]string, 0, len(config.Features))
	for _, it := range config.Features {
		names = append(names, string(it))
	}
	return "", fmt.Errorf("no feature is called %q: choose one of %s", s, strings.Join(names, ", "))
}

func runFeatureList(_ *cobra.Command, _ []string) error {
	cfg := config.LoadOrDefaults()
	fmt.Printf("%-20s %-6s %-11s %s\n", "FEATURE", "HERE", "VIAVERITAS", "NOW")
	onOff := map[bool]string{true: "on", false: "off"}
	for _, f := range config.Built {
		fmt.Printf("%-20s %-6s %-11s %s\n", f, onOff[cfg.Allows(f)], onOff[cfg.PortalAllows(f)], featureState(cfg, f))
	}
	if cfg.RemotePaused {
		fmt.Println("\nRemote access is paused: `bridge resume` lets it run again.")
	}
	return nil
}

func runFeatureSet(on bool) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, args []string) error {
		if err := platform.Require("changing what the bridge may do"); err != nil {
			return err
		}
		f, err := parseFeature(args[0])
		if err != nil {
			return err
		}
		cfg := config.LoadOrDefaults()
		if cfg.Allows(f) == on {
			logx.Ok("bridge", "%s is %s on this computer already", f, map[bool]string{true: "on", false: "off"}[on])
			return nil
		}
		if err := config.Save(cfg.WithFeature(f, on)); err != nil {
			return err
		}
		if !on {
			logx.Ok("bridge", "%s is off on this computer: ViaVeritas can't turn it on", f)
			return nil
		}
		logx.Ok("bridge", "%s is on on this computer; ViaVeritas can still switch it off", f)
		if !f.IsBuilt() {
			logx.Info("bridge", "this version of the bridge can't do %s yet: the switch applies once a version can", f)
		}
		return nil
	}
}

func runPause(paused bool) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, _ []string) error {
		if err := platform.Require("pausing remote access"); err != nil {
			return err
		}
		cfg := config.LoadOrDefaults()
		if cfg.RemotePaused == paused {
			logx.Ok("bridge", "remote access is %s already", map[bool]string{true: "paused", false: "on"}[paused])
			return nil
		}
		cfg.RemotePaused = paused
		if err := config.Save(cfg); err != nil {
			return err
		}
		if paused {
			logx.Ok("bridge", "remote access paused: what is open ends, and nothing opens until `bridge resume`")
		} else {
			logx.Ok("bridge", "remote access resumed, as each feature's switches allow")
		}
		return nil
	}
}

func runSessionsList(_ *cobra.Command, _ []string) error {
	if err := platform.Require("listing what ViaVeritas has open"); err != nil {
		return err
	}
	st, err := control.Ask()
	if err != nil {
		if errors.Is(err, control.ErrNotRunning) {
			fmt.Println("The service isn't running, so nothing is open.")
			return nil
		}
		return err
	}
	if len(st.Streams) == 0 {
		fmt.Println("Nothing is open on this computer from ViaVeritas.")
		return nil
	}
	fmt.Printf("%-38s %-10s %-30s %s\n", "ID", "KIND", "OPENED BY", "SINCE")
	for _, s := range st.Streams {
		by := s.User.Email
		if by == "" {
			by = s.User.Name
		}
		if by == "" {
			by = "ViaVeritas"
		}
		fmt.Printf("%-38s %-10s %-30s %s\n", s.ID, s.Kind, by, s.OpenedAt.Local().Format(time.DateTime))
	}
	return nil
}

func runSessionsRunAs(_ *cobra.Command, args []string) error {
	if err := platform.Require("choosing who terminals and commands run as"); err != nil {
		return err
	}
	runAs := config.RunAs(strings.ToLower(strings.TrimSpace(args[0])))
	if !runAs.Known() {
		return fmt.Errorf("choose %s (the person signed in at this computer) or %s (its administrator)", config.RunAsUser, config.RunAsSystem)
	}
	cfg := config.LoadOrDefaults()
	cfg.RunAs = runAs
	if err := config.Save(cfg); err != nil {
		return err
	}
	if runAs == config.RunAsSystem {
		logx.Ok("bridge", "terminals and commands from ViaVeritas run as this computer's administrator")
	} else {
		logx.Ok("bridge", "terminals and commands from ViaVeritas run as the person signed in at this computer, and wait for someone to be")
	}
	return nil
}

func runSessionsNotices(_ *cobra.Command, args []string) error {
	if err := platform.Require("choosing whether the screen says when ViaVeritas opens something"); err != nil {
		return err
	}
	on := strings.ToLower(strings.TrimSpace(args[0]))
	if on != "on" && on != "off" {
		return errors.New("choose on or off")
	}
	cfg := config.LoadOrDefaults()
	cfg.HideSessionNotices = on == "off"
	if err := config.Save(cfg); err != nil {
		return err
	}
	logx.Ok("bridge", "the screen %s when ViaVeritas opens a terminal or runs a command; the log always does", map[bool]string{true: "says", false: "doesn't say"}[on == "on"])
	return nil
}

func runSessionsEnd(_ *cobra.Command, args []string) error {
	if err := platform.Require("ending what ViaVeritas has open"); err != nil {
		return err
	}
	if err := control.End(args[0]); err != nil {
		return err
	}
	logx.Ok("bridge", "ended %s", args[0])
	return nil
}
