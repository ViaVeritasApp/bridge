//go:build windows

package platform

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// ComputeFileID derives the NTFS file identity: volume serial + 64-bit file
// index from GetFileInformationByHandle, stable across a rename/move on the
// same volume.
//
// The handle is opened FILE_READ_ATTRIBUTES only (not GENERIC_READ as os.Open
// would): a metadata-only open gives the antivirus minifilter no content to
// scan — GENERIC_READ made a first scan take minutes. FILE_FLAG_BACKUP_SEMANTICS
// also handles directory paths and restrictive ACLs (the service runs as
// SYSTEM); FILE_FLAG_OPEN_REPARSE_POINT avoids traversing a reparse point.
//
// id is the compact "<vol>:<index>" hex stored in index.txt, compared for move
// detection, and sent verbatim as device_file_ino.
func (osImpl) ComputeFileID(path string, _ os.FileInfo) (id string, ok bool) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", false
	}
	h, err := windows.CreateFile(
		p,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(h)

	var bhfi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &bhfi); err != nil {
		return "", false
	}
	vol := uint64(bhfi.VolumeSerialNumber)
	idx := uint64(bhfi.FileIndexHigh)<<32 | uint64(bhfi.FileIndexLow)
	return fmt.Sprintf("%x:%x", vol, idx), true
}

// DeviceOf isn't needed on Windows: a walk doesn't follow the mount points
// (reparse points) that put another volume inside a folder.
func DeviceOf(os.FileInfo) (uint64, bool) { return 0, false }
