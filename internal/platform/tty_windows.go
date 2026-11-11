//go:build windows

package platform

import (
	"os"

	"golang.org/x/sys/windows"
)

// IsTerminal reports whether f is a console someone can type into.
func IsTerminal(f *os.File) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil
}
