// Package terminal runs what ViaVeritas opens on this computer from its
// portal: a terminal (a shell in a pseudo-terminal: PTY on Unix, ConPTY on
// Windows) and a command, its output streamed back with its exit code. Both
// run as the account this computer chose (config.RunAs): the person signed in
// at it, or its administrator. The bridge never keeps a password: it takes the
// signed-in person's identity from the system, so with nobody signed in a
// session that should run as them is refused.
package terminal

import "errors"

// ErrNobody: the sessions run as the person signed in, and nobody is.
var ErrNobody = errors.New("nobody is signed in at this computer: someone has to be, or its sessions must run as its administrator")

// Account is who a terminal or a command runs as.
type Account struct {
	Name  string
	Home  string
	Shell string
	// System: the computer's administrator (root, or SYSTEM).
	System bool

	// Unix: the identity to switch to (left alone when the bridge isn't root).
	UID, GID uint32
	Groups   []uint32
	switchTo bool

	// Windows: the signed-in person's primary token; 0 for SYSTEM.
	token uintptr
}

// Label names the account to ViaVeritas and in the log.
func (a *Account) Label() string { return a.Name }
