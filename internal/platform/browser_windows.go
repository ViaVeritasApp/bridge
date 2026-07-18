//go:build windows

package platform

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// OpenBrowser opens url in the default browser (best-effort; the wizard's
// printed URL is the fallback).
func (osImpl) OpenBrowser(url string) {
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// FilesystemRoot is "" on Windows: the folder browser presents the drive list
// (see DriveLetters) as the root rather than a single "/" path.
func (osImpl) FilesystemRoot() string { return "" }

// DriveLetters lists mounted drives C:\..Z:\ for the folder-browser root.
func (osImpl) DriveLetters() []DirEntry {
	var d []DirEntry
	for c := 'C'; c <= 'Z'; c++ {
		p := string(c) + ":\\"
		if _, err := os.Stat(p); err == nil {
			d = append(d, DirEntry{Name: string(c) + ":", Path: p})
		}
	}
	return d
}

// Breadcrumbs splits absPath into folder-browser breadcrumb segments, led by
// "This PC" and the drive-volume crumb.
func (osImpl) Breadcrumbs(absPath string) []Crumb {
	vol := filepath.VolumeName(absPath) // "C:" on windows
	rest := strings.Trim(strings.TrimPrefix(absPath, vol), string(os.PathSeparator))
	out := []Crumb{
		{Label: "This PC", Path: ""},
		{Label: vol + "\\", Path: vol + "\\"},
	}
	if rest == "" {
		return out
	}
	cur := vol + string(os.PathSeparator)
	for _, seg := range strings.Split(rest, string(os.PathSeparator)) {
		cur = filepath.Join(cur, seg)
		out = append(out, Crumb{Label: seg, Path: cur})
	}
	return out
}
