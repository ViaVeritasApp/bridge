//go:build !windows

package platform

import (
	"fmt"
	"os"
	"syscall"
)

// ComputeFileID derives the OS file identity from info: device ID + inode,
// stable across a rename/move on the same filesystem (path unused on Unix). id
// is the compact "<dev>:<ino>" hex stored in index.txt, compared for move
// detection, and sent verbatim as device_file_ino.
func (osImpl) ComputeFileID(_ string, info os.FileInfo) (id string, ok bool) {
	st, isStat := info.Sys().(*syscall.Stat_t)
	if !isStat {
		return "", false
	}
	// Dev is int32 on darwin and uint64 on linux; Ino is uint64 on both.
	dev := uint64(st.Dev)
	ino := uint64(st.Ino)
	return fmt.Sprintf("%x:%x", dev, ino), true
}

// DeviceOf is the filesystem a file is on, so a folder's scan stays on its own.
func DeviceOf(info os.FileInfo) (uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true
}
