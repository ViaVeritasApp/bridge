package terminal

import (
	"os"

	pty "github.com/aymanbagabas/go-pty"
)

func prepareTTY(pty.Pty, *Account) {}

// stopProcess ends a process.
func stopProcess(p *os.Process) {
	if p != nil {
		_ = p.Kill()
	}
}
