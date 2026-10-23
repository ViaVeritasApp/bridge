package platform

import (
	"path/filepath"
	"regexp"
	"runtime"
)

// macProtected: the folders macOS keeps background services out of until they
// have Full Disk Access (Desktop, Documents, Downloads, iCloud Drive, cloud
// storage, other disks).
var macProtected = regexp.MustCompile(`^/Users/[^/]+/(Desktop|Documents|Downloads|Library/Mobile Documents|Library/CloudStorage)(/|$)|^/Volumes/`)

// Protected reports a folder the service can't read until the bridge has Full
// Disk Access. Only macOS has them.
func Protected(path string) bool {
	return runtime.GOOS == "darwin" && path != "" && macProtected.MatchString(filepath.ToSlash(filepath.Clean(path)))
}
