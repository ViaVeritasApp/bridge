//go:build windows

package platform

import "errors"

// StartSetup is the program run with no arguments (see start_unix.go). On
// Windows a double-clicked bridge.exe gets its console window from Explorer,
// and its manifest has Windows ask for an administrator (UAC) before it
// starts, so it is elevated already; a build without the manifest asks for
// it here and starts again in a new, elevated window.
func StartSetup() (done bool, code int) {
	handled, err := RelaunchElevated()
	if !handled {
		return false, 0
	}
	if errors.Is(err, ErrElevationCancelled) {
		return true, 130
	}
	return true, 0
}
