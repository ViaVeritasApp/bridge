//go:build windows

package platform

import "golang.org/x/sys/windows"

// DiskFreeBytes returns the bytes available to the calling user on the volume
// backing path, or 0 if it cannot be determined.
func (osImpl) DiskFreeBytes(path string) int64 {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	var freeToCaller, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeToCaller, &total, &totalFree); err != nil {
		return 0
	}
	if freeToCaller > 1<<62 {
		return 0
	}
	return int64(freeToCaller)
}
