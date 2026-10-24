package terminal

import (
	"strings"
)

// consoleUser is the person signed in at the computer itself: logind's active
// local session of a user (one over SSH doesn't count), else the first person
// `who` lists on a console or a display.
func consoleUser() (string, error) {
	if out, err := run("loginctl", "list-sessions", "--no-legend"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			props, err := run("loginctl", "show-session", fields[0], "--property=Name", "--property=Active", "--property=Remote", "--property=Class")
			if err != nil {
				continue
			}
			p := map[string]string{}
			for _, kv := range strings.Split(props, "\n") {
				if k, v, ok := strings.Cut(kv, "="); ok {
					p[k] = v
				}
			}
			if p["Active"] == "yes" && p["Remote"] == "no" && p["Class"] == "user" && p["Name"] != "" && p["Name"] != "root" {
				return p["Name"], nil
			}
		}
	}
	if out, err := run("who"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] != "root" && (strings.HasPrefix(fields[1], "tty") || strings.HasPrefix(fields[1], ":") || strings.HasPrefix(fields[1], "seat")) {
				return fields[0], nil
			}
		}
	}
	return "", ErrNobody
}

func recordedShell(name string) string { return passwdShell(name) }
