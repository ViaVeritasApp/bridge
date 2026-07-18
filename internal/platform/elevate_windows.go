//go:build windows

package platform

import "golang.org/x/sys/windows"

// IsAdmin reports whether the process token is a member of
// BUILTIN\Administrators with admin rights granted.
func (osImpl) IsAdmin() bool {
	var sid *windows.SID
	if err := windows.AllocateAndInitializeSid(
		&windows.SECURITY_NT_AUTHORITY,
		2,
		windows.SECURITY_BUILTIN_DOMAIN_RID,
		windows.DOMAIN_ALIAS_RID_ADMINS,
		0, 0, 0, 0, 0, 0,
		&sid,
	); err != nil {
		return false
	}
	defer windows.FreeSid(sid)

	member, err := windows.Token(0).IsMember(sid)
	return err == nil && member
}

// ElevationHint tells the operator how to retry the command as Administrator.
func (osImpl) ElevationHint() string {
	return `this command must run as Administrator — right-click bridge.exe → "Run as administrator", or use an elevated PowerShell`
}
