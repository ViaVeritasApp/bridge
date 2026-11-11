//go:build linux

package platform

import (
	"errors"
	"os"
	"os/exec"
	"strings"
)

// terminals are the terminal apps tried, in order, and how each is told what
// to run: the Debian/Ubuntu default first, then the desktops' own, then the
// rest. A {} argument is replaced by the program and its arguments; {sh} by
// the same as one shell-quoted string, for those that take a command line.
var terminals = [][]string{
	{"x-terminal-emulator", "-e", "{}"},
	{"gnome-terminal", "--", "{}"},
	{"ptyxis", "--", "{}"},
	{"kgx", "--", "{}"},
	{"konsole", "-e", "{}"},
	{"xfce4-terminal", "-x", "{}"},
	{"mate-terminal", "-x", "{}"},
	{"tilix", "-e", "{sh}"},
	{"terminator", "-x", "{}"},
	{"lxterminal", "-e", "{sh}"},
	{"qterminal", "-e", "{sh}"},
	{"alacritty", "-e", "{}"},
	{"kitty", "{}"},
	{"foot", "{}"},
	{"wezterm", "start", "--", "{}"},
	{"xterm", "-e", "{}"},
}

// openTerminal runs exe in a new window of the first terminal app found, as a
// file manager starts a double-clicked program without one. Needs a display.
func openTerminal(exe string) error {
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return errors.New("no display")
	}
	run := []string{"/usr/bin/env", envInTerminal + "=1", exe}
	quoted := make([]string, len(run))
	for i, a := range run {
		quoted[i] = shellQuote(a)
	}
	for _, t := range terminals {
		path, err := exec.LookPath(t[0])
		if err != nil {
			continue
		}
		var args []string
		for _, a := range t[1:] {
			switch a {
			case "{}":
				args = append(args, run...)
			case "{sh}":
				args = append(args, strings.Join(quoted, " "))
			default:
				args = append(args, a)
			}
		}
		cmd := exec.Command(path, args...)
		if err := cmd.Start(); err != nil {
			continue
		}
		_ = cmd.Process.Release()
		return nil
	}
	return errors.New("no terminal app found")
}
