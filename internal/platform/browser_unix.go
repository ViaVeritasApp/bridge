//go:build !windows

package platform

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// OpenBrowser opens url in the default browser (best-effort; the wizard's
// printed URL is the fallback). macOS uses `open`, other Unixes `xdg-open`.
func (osImpl) OpenBrowser(url string) {
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("open", url)
	} else {
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// FilesystemRoot is "/" on macOS/Linux — the single folder-browser root.
func (osImpl) FilesystemRoot() string { return "/" }

// DriveLetters returns nil: Unix has one "/" root, not drives.
func (osImpl) DriveLetters() []DirEntry { return nil }

// Breadcrumbs splits absPath into folder-browser breadcrumb segments from "/".
func (osImpl) Breadcrumbs(absPath string) []Crumb {
	rest := strings.Trim(absPath, string(os.PathSeparator))
	out := []Crumb{{Label: "/", Path: "/"}}
	if rest == "" {
		return out
	}
	cur := string(os.PathSeparator)
	for _, seg := range strings.Split(rest, string(os.PathSeparator)) {
		cur = filepath.Join(cur, seg)
		out = append(out, Crumb{Label: seg, Path: cur})
	}
	return out
}
