//go:build !windows

package platform

import "os"

// IsAdmin reports whether the current process has root privileges.
func (osImpl) IsAdmin() bool { return os.Geteuid() == 0 }

// ElevationHint tells the operator how to retry the command as root.
func (osImpl) ElevationHint() string { return "this command must run as root" }
