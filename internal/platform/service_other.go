//go:build !windows

package platform

import "github.com/kardianos/service"

// StopService stops the installed service. On macOS/Linux the platform stop
// (launchctl unload / systemctl stop, as root) reliably kills the process (with
// a SIGKILL fallback after the manager's timeout), so no extra force-kill is
// needed. name is unused here; the Windows build uses it to pin the process.
func (osImpl) StopService(s service.Service, _ string) error {
	return s.Stop()
}
