//go:build !windows

package platform

import "syscall"

// DiskFreeBytes returns the bytes available to an unprivileged caller on the
// filesystem backing path, or 0 if it cannot be determined.
func (osImpl) DiskFreeBytes(path string) int64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0
	}
	return int64(stat.Bavail) * int64(stat.Bsize)
}
