package terminal

import (
	"context"
	"errors"
	"os/exec"
	"os/user"
	"time"
)

// showNotice shows a desktop notification to the person signed in at the
// computer (notify-send on their session bus); a server without one shows nothing.
func showNotice(title, text string) error {
	name, err := consoleUser()
	if err != nil {
		return err
	}
	u, err := user.Lookup(name)
	if err != nil {
		return err
	}
	if _, err := exec.LookPath("notify-send"); err != nil {
		return errors.New("notify-send isn't installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "runuser", "-u", name, "--", "env",
		"DISPLAY=:0", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/"+u.Uid+"/bus",
		"notify-send", "--app-name=ViaVeritas", title, text).Run()
}
