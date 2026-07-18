//go:build windows

package platform

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// GUIElevationAvailable is always true on Windows: the "runas" verb surfaces a
// UAC consent prompt even on Server Core. The primary elevation path is the
// embedded requireAdministrator manifest; this runtime path is the fallback for
// a binary built without it.
func (osImpl) GUIElevationAvailable() bool { return true }

// Relaunch re-executes the binary via ShellExecute with the "runas" verb,
// triggering a UAC consent prompt. ShellExecute has no env parameter, so the
// loop guard is IsAdmin() itself.
func (osImpl) Relaunch(exe string) error {
	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return err
	}
	file, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	args, err := windows.UTF16PtrFromString("setup")
	if err != nil {
		return err
	}

	err = windows.ShellExecute(0, verb, file, args, nil, windows.SW_SHOWNORMAL)
	if err == nil {
		return nil
	}
	// ERROR_CANCELLED (1223): the user dismissed the UAC prompt.
	if errors.Is(err, syscall.Errno(windows.ERROR_CANCELLED)) {
		return ErrElevationCancelled
	}
	return err
}
