package platform

import (
	"os"

	"bridge/internal/logx"
)

// envElevated is set on the relaunched child so it never re-elevates. On
// Windows/Linux IsAdmin() (token / geteuid) is the real loop guard and this is
// belt-and-suspenders; on macOS `do shell script` doesn't inherit osascript's
// environment, so the darwin path sets it *inside* the privileged shell command.
const envElevated = "BRIDGE_ELEVATED"

// Require returns nil if elevated, else logs what's needed and returns
// ErrNotElevated (it does not re-run elevated). action is a short phrase for
// the log line, e.g. "installing the service".
func Require(action string) error {
	if current.IsAdmin() {
		return nil
	}
	logx.Error("bridge", "%s requires elevated privileges — %s", action, current.ElevationHint())
	return ErrNotElevated
}

// RelaunchElevated re-execs this executable elevated via a native OS GUI
// prompt, then runs the `setup` wizard. Return values:
//
//	handled=false           → not applicable (already elevated, we are the
//	                          child, or no GUI / headless / SSH); caller falls
//	                          back to its existing launchSetupUI() path.
//	handled=true, err=nil   → child ran the wizard to completion; caller exit(0).
//	handled=true, err!=nil  → user dismissed the prompt (ErrElevationCancelled);
//	                          caller exits quietly without the "needs root" log.
func RelaunchElevated() (handled bool, err error) {
	if current.IsAdmin() {
		return false, nil
	}
	if os.Getenv(envElevated) == "1" {
		return false, nil
	}
	if !current.GUIElevationAvailable() {
		return false, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return false, nil
	}
	if err := current.Relaunch(exe); err != nil {
		if err == ErrElevationCancelled {
			return true, err
		}
		// Spawn/exec failure: fall back so the operator still gets the hint.
		return false, nil
	}
	return true, nil
}
