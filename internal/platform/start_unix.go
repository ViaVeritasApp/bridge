//go:build !windows

package platform

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
)

// envInTerminal marks the copy of the program a terminal window opened by
// openTerminal runs, so one without a terminal never opens another.
const envInTerminal = "BRIDGE_IN_TERMINAL"

// StartSetup is the program run with no arguments (a double-click, or `bridge`
// typed in a terminal): the setup wizard, as root, in a terminal window.
//
//	done=false → this process is root already: the caller runs the wizard here.
//	done=true  → it ran, or was started, elsewhere: the caller exits with code.
//	             130 means the person pressed Ctrl+C before it started.
//
// Without a terminal (a Linux file manager starts programs without one) it
// opens one running this program again. In one, sudo asks for the password
// there and runs `<exe> setup` in it, so its output and Ctrl+C stay in sight.
// With no terminal to open and no sudo, the native password dialog is the
// last resort.
func StartSetup() (done bool, code int) {
	if current.IsAdmin() {
		return false, 0
	}
	exe, err := os.Executable()
	if err != nil {
		return false, 0
	}
	if !IsTerminal(os.Stdin) {
		if os.Getenv(envInTerminal) == "" && openTerminal(exe) == nil {
			return true, 0
		}
		return guiFallback()
	}
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return guiFallback()
	}
	return true, sudoSetup(sudo, exe)
}

// guiFallback asks for the password in the native dialog (osascript /
// pkexec), as double-clicks did before they opened a terminal.
func guiFallback() (bool, int) {
	handled, err := RelaunchElevated()
	if !handled {
		return false, 0
	}
	if errors.Is(err, ErrElevationCancelled) {
		return true, 130
	}
	return true, 0
}

// sudoSetup runs `sudo <exe> setup` in this terminal and returns its exit code.
func sudoSetup(sudo, exe string) int {
	fmt.Println()
	fmt.Println("  ViaVeritas Bridge needs administrator rights to set up this computer.")
	if exec.Command(sudo, "-n", "true").Run() != nil {
		fmt.Println("  Type the password you sign in to this computer with, then press Enter.")
		fmt.Println("  Nothing shows on screen while you type it.")
	}
	fmt.Println()

	args := []string{"-p", "  Password for %u: ", "--"}
	if env := displayEnv(); len(env) > 0 {
		// sudo resets the environment; the wizard needs these to open the
		// browser on the person's screen.
		args = append(args, append([]string{"/usr/bin/env"}, env...)...)
	}
	args = append(args, exe, "setup")
	cmd := exec.Command(sudo, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	// Ctrl+C reaches sudo and the wizard as well (one process group); the
	// wizard stops cleanly on it, so this process only waits for it to. Notify
	// rather than Ignore: an ignored signal would stay ignored in the child.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	err := cmd.Run()
	interrupted := len(sig) > 0
	switch {
	case err == nil:
		return 0
	case interrupted:
		return 130
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	fmt.Fprintln(os.Stderr, "  couldn't run sudo:", err)
	return 1
}

// displayEnv is the graphical session's environment the browser needs, as
// KEY=value pairs (Linux; macOS needs none).
func displayEnv() []string {
	if runtime.GOOS != "linux" {
		return nil
	}
	var env []string
	for _, k := range []string{"DISPLAY", "XAUTHORITY", "WAYLAND_DISPLAY", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// shellQuote wraps s in single quotes for /bin/sh, escaping embedded quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
