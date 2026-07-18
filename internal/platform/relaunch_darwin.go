//go:build darwin

package platform

import (
	"os"
	"os/exec"
	"strings"
)

// GUIElevationAvailable reports whether a native macOS password dialog can be
// shown. An SSH session has no WindowServer, so osascript elevation would fail.
func (osImpl) GUIElevationAvailable() bool {
	return os.Getenv("SSH_CONNECTION") == "" && os.Getenv("SSH_TTY") == ""
}

// shellQuote wraps s in single quotes for /bin/sh, escaping embedded quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// appleEscape escapes a Go string for use inside an AppleScript double-quoted
// string literal.
func appleEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// Relaunch re-executes the binary elevated via a native admin password dialog.
// The exe path crosses two layers (AppleScript literal, then the /bin/sh that
// `do shell script` spawns): shell-quote for sh, prepend the loop-guard env
// assignment *inside* the command (`do shell script` doesn't inherit
// osascript's environment), then AppleScript-escape the whole command.
func (osImpl) Relaunch(exe string) error {
	inner := envElevated + "=1 " + shellQuote(exe) + " setup"
	script := `do shell script "` + appleEscape(inner) + `" with administrator privileges`

	cmd := exec.Command("osascript", "-e", script)
	out, err := cmd.CombinedOutput()
	if err == nil {
		// Privileged child ran the wizard to completion (do shell script blocks
		// until it exits; the wizard self-stops via the browser-close watchdog).
		return nil
	}
	// osascript surfaces a cancelled dialog as error -128 / "User canceled".
	o := string(out)
	if strings.Contains(o, "User canceled") || strings.Contains(o, "-128") {
		return ErrElevationCancelled
	}
	return err
}
