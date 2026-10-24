//go:build windows

package platform

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// installDirSDDL: SYSTEM and Administrators have full control, inherited by
// everything inside, and nothing comes from the folder above ("P"). A folder
// made under C:\ otherwise lets every signed-in user change what is in it, and
// the service runs its program and reads its settings from there as SYSTEM.
const installDirSDDL = "D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

// HardenDir lets only SYSTEM and Administrators into dir and what it holds.
func (osImpl) HardenDir(dir string) error {
	sd, err := windows.SecurityDescriptorFromString(installDirSDDL)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("set the permissions of %s: %w", dir, err)
	}
	return nil
}
