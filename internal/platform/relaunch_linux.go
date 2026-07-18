//go:build linux

package platform

import (
	"os"
	"os/exec"
)

// GUIElevationAvailable reports whether a PolicyKit GUI password prompt can be
// shown: pkexec must exist and there must be a display (X11 or Wayland).
func (osImpl) GUIElevationAvailable() bool {
	if _, err := exec.LookPath("pkexec"); err != nil {
		return false
	}
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

// Relaunch re-executes the binary via pkexec (PolicyKit GUI prompt). pkexec
// sanitizes the environment, stripping DISPLAY/XAUTHORITY/WAYLAND_DISPLAY/
// XDG_RUNTIME_DIR that the elevated wizard's xdg-open needs, so we re-inject
// them via an `env` wrapper. The loop guard is IsAdmin() (geteuid == 0), not
// the stripped env var.
func (osImpl) Relaunch(exe string) error {
	args := []string{"env"}
	for _, k := range []string{"DISPLAY", "XAUTHORITY", "WAYLAND_DISPLAY", "XDG_RUNTIME_DIR"} {
		if v := os.Getenv(k); v != "" {
			args = append(args, k+"="+v)
		}
	}
	args = append(args, envElevated+"=1", exe, "setup")

	cmd := exec.Command("pkexec", args...)
	if err := cmd.Run(); err != nil {
		// pkexec: 126 = not authorized / dismissed, 127 = auth agent
		// unavailable. Both are user-side cancellation.
		if ee, ok := err.(*exec.ExitError); ok {
			if code := ee.ExitCode(); code == 126 || code == 127 {
				return ErrElevationCancelled
			}
		}
		return err
	}
	return nil
}
