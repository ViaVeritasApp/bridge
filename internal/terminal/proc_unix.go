//go:build darwin || linux

package terminal

import (
	"os"
	"syscall"
	"time"

	pty "github.com/aymanbagabas/go-pty"
)

// prepareTTY gives the account the terminal it will run in, as a login would.
func prepareTTY(pt pty.Pty, a *Account) {
	if a.switchTo {
		_ = os.Chown(pt.Name(), int(a.UID), int(a.GID))
	}
}

// stopProcess ends a process and what it started (its process group): a
// hangup first, as closing a terminal does, then a kill.
func stopProcess(p *os.Process) {
	if p == nil {
		return
	}
	_ = syscall.Kill(-p.Pid, syscall.SIGHUP)
	go func() {
		time.Sleep(2 * time.Second)
		_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
	}()
}
