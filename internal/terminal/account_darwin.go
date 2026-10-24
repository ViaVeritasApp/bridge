package terminal

import "strings"

// consoleUser is the person signed in at the Mac: the owner of /dev/console,
// root while it shows the login window.
func consoleUser() (string, error) {
	name, err := run("stat", "-f", "%Su", "/dev/console")
	if err != nil || name == "" || name == "root" || name == "_mbsetupuser" {
		return "", ErrNobody
	}
	return name, nil
}

// consoleUID is that person's uid, for showing them a notice ("" when nobody).
func consoleUID() string {
	uid, err := run("stat", "-f", "%u", "/dev/console")
	if err != nil || uid == "0" {
		return ""
	}
	return uid
}

func recordedShell(name string) string {
	out, err := run("dscl", ".", "-read", "/Users/"+name, "UserShell")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(out, "UserShell:"))
}
