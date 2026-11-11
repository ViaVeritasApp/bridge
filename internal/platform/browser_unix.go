//go:build !windows

package platform

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"bridge/internal/logx"
)

// OpenBrowser opens url in the default browser (best-effort; the wizard's
// printed URL is the fallback). macOS uses `open`, other Unixes `xdg-open`.
// Run as root through sudo or pkexec, it opens it as the person who ran that:
// a browser started as root uses root's profile, and Chrome refuses to.
func (osImpl) OpenBrowser(url string) {
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("/usr/bin/open", url)
	} else {
		cmd = exec.Command("xdg-open", url)
	}
	asInvoker(cmd)
	if err := cmd.Start(); err != nil {
		logx.Warn("setup", "couldn't open the browser: %v", err)
		return
	}
	go func() { _ = cmd.Wait() }()
}

// maxGroups is how many supplementary groups a process may have: macOS allows
// 16 (NGROUPS_MAX) and refuses more with EINVAL, though an account can be in
// more; Linux allows 65536.
var maxGroups = map[string]int{"darwin": 16}

// asInvoker has cmd run as the person who ran this root process through sudo
// (SUDO_UID) or pkexec (PKEXEC_UID), with their home; unchanged otherwise.
func asInvoker(cmd *exec.Cmd) {
	if os.Geteuid() != 0 {
		return
	}
	id := os.Getenv("SUDO_UID")
	if id == "" {
		id = os.Getenv("PKEXEC_UID")
	}
	if id == "" || id == "0" {
		return
	}
	u, err := user.LookupId(id)
	if err != nil {
		return
	}
	uid, err1 := strconv.ParseUint(u.Uid, 10, 32)
	gid, err2 := strconv.ParseUint(u.Gid, 10, 32)
	if err1 != nil || err2 != nil {
		return
	}
	var groups []uint32
	if gids, err := u.GroupIds(); err == nil {
		for _, g := range gids {
			if n, err := strconv.ParseUint(g, 10, 32); err == nil {
				groups = append(groups, uint32(n))
			}
		}
	}
	if m := maxGroups[runtime.GOOS]; m > 0 && len(groups) > m {
		groups = groups[:m]
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups}}
	cmd.Env = append(os.Environ(), "HOME="+u.HomeDir, "USER="+u.Username, "LOGNAME="+u.Username)
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
