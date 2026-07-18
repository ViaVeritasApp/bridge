//go:build windows

package platform

import (
	"github.com/kardianos/service"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"

	"bridge/internal/logx"
)

// StopService stops the installed service and guarantees its process is gone.
// The Windows SCM "stop" can return success (or a benign error like
// ERROR_SERVICE_NOT_ACTIVE) while the process keeps running and holding the
// bridge's LAN listener. So we pin the process by handle (taken before the stop
// via the SCM-reported PID, so the PID can't be recycled) and terminate it if a
// graceful stop leaves it alive. Returns the underlying Stop error to log; name
// locates the service via the SCM.
func (osImpl) StopService(s service.Service, name string) error {
	h := serviceProcessHandle(name) // 0 if not installed / not running
	stopErr := s.Stop()
	if h == 0 {
		return stopErr
	}
	defer windows.CloseHandle(h)

	// Give a graceful exit a few seconds; the handle pins the process identity,
	// so this never races onto an unrelated PID.
	if ev, _ := windows.WaitForSingleObject(h, 5000); ev == windows.WAIT_OBJECT_0 {
		return stopErr // exited on its own
	}
	if terr := windows.TerminateProcess(h, 1); terr != nil {
		logx.Warn("bridge", "service process still running and could not be terminated: %v", terr)
	} else {
		_, _ = windows.WaitForSingleObject(h, 5000)
		logx.Warn("bridge", "service process did not stop on request; terminated it")
	}
	return stopErr
}

// serviceProcessHandle returns a handle (PROCESS_TERMINATE|SYNCHRONIZE) to the
// named service's process, or 0 if not installed / not running. The handle pins
// the PID so it can't be recycled before StopService waits on / terminates it.
func serviceProcessHandle(name string) windows.Handle {
	enableSeDebugPrivilege() // so OpenProcess can open the LocalSystem service

	m, err := mgr.Connect()
	if err != nil {
		return 0
	}
	defer m.Disconnect()

	wsvc, err := m.OpenService(name)
	if err != nil {
		return 0 // not installed — nothing to force-stop
	}
	defer wsvc.Close()

	st, err := wsvc.Query()
	if err != nil || st.ProcessId == 0 {
		return 0 // not running — nothing to force-stop
	}

	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, st.ProcessId)
	if err != nil {
		logx.Warn("bridge", "could not open service process %d: %v", st.ProcessId, err)
		return 0
	}
	return h
}

// enableSeDebugPrivilege best-effort enables SeDebugPrivilege in the current
// (elevated) token, so OpenProcess can open the LocalSystem service process
// regardless of its DACL.
func enableSeDebugPrivilege() {
	cur, err := windows.GetCurrentProcess()
	if err != nil {
		return
	}
	var tok windows.Token
	if err := windows.OpenProcessToken(cur, windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &tok); err != nil {
		return
	}
	defer tok.Close()

	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr("SeDebugPrivilege"), &luid); err != nil {
		return
	}
	tp := windows.Tokenprivileges{PrivilegeCount: 1}
	tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
	_ = windows.AdjustTokenPrivileges(tok, false, &tp, 0, nil, nil)
}
