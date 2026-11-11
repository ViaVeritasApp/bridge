//go:build !windows

package platform

import (
	"os"

	"golang.org/x/sys/unix"
)

// IsTerminal reports whether f is a terminal someone can type into. A program
// a Linux file manager starts has /dev/null for stdin: a character device too,
// but not a terminal, so this asks for its terminal settings instead.
func IsTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), ioctlReadTermios)
	return err == nil
}
