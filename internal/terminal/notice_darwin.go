package terminal

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// showNotice shows a notification to the person signed in at the Mac, as them.
func showNotice(title, text string) error {
	uid := consoleUID()
	name, err := consoleUser()
	if uid == "" || err != nil {
		return errors.New("nobody is signed in")
	}
	quote := func(s string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"` }
	script := "display notification " + quote(text) + " with title " + quote(title)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "launchctl", "asuser", uid, "sudo", "-u", name, "osascript", "-e", script).Run()
}
