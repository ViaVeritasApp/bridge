//go:build darwin || linux

package terminal

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"bridge/internal/config"
)

// Resolve is the account sessions run as now. A bridge that isn't root (a
// developer's run) can't switch: it runs them as itself.
func Resolve(runAs config.RunAs) (*Account, error) {
	if os.Geteuid() != 0 {
		u, err := user.Current()
		if err != nil {
			return nil, err
		}
		return lookup(u.Username, false)
	}
	if runAs == config.RunAsSystem {
		return lookup("root", true)
	}
	name, err := consoleUser()
	if err != nil {
		return nil, err
	}
	return lookup(name, false)
}

// Release lets go of what the account holds: nothing on Unix.
func (a *Account) Release() {}

func lookup(name string, system bool) (*Account, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("the account %q: %w", name, err)
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	a := &Account{Name: u.Username, Home: u.HomeDir, Shell: loginShell(u.Username), System: system || uid == 0, UID: uint32(uid), GID: uint32(gid)}
	if ids, err := u.GroupIds(); err == nil {
		for _, id := range ids {
			if g, err := strconv.ParseUint(id, 10, 32); err == nil {
				a.Groups = append(a.Groups, uint32(g))
			}
		}
	}
	a.switchTo = os.Geteuid() == 0 && uint32(os.Geteuid()) != a.UID
	if a.Home == "" {
		a.Home = "/"
	}
	return a, nil
}

// env is a login environment for the account: its own home, name and shell,
// the usual PATH, and a terminal that knows colours.
func (a *Account) env() []string {
	path := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	if _, err := os.Stat("/opt/homebrew/bin"); err == nil {
		path = "/opt/homebrew/bin:/opt/homebrew/sbin:" + path
	}
	lang := os.Getenv("LANG")
	if lang == "" {
		lang = "en_US.UTF-8"
	}
	return []string{
		"HOME=" + a.Home, "USER=" + a.Name, "LOGNAME=" + a.Name, "SHELL=" + a.Shell,
		"PATH=" + path, "TERM=xterm-256color", "COLORTERM=truecolor", "LANG=" + lang,
	}
}

// sysProcAttr switches to the account when the bridge may; group: the command
// leads a process group of its own, so ending it ends what it started.
func (a *Account) sysProcAttr(group bool) *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setpgid: group}
	if a.switchTo {
		attr.Credential = &syscall.Credential{Uid: a.UID, Gid: a.GID, Groups: a.Groups}
	}
	return attr
}

// shellArgs starts the account's shell as a login shell.
func (a *Account) shellArgs() []string { return []string{"-l"} }

// commandArgs runs one command line in the account's login shell, so its PATH is the one it knows.
func (a *Account) commandArgs(command string) (string, []string) {
	return a.Shell, []string{"-lc", command}
}

// loginShell is the account's shell, from the system's records, else the first usual one there is.
func loginShell(name string) string {
	if shell := recordedShell(name); shell != "" {
		if _, err := os.Stat(shell); err == nil {
			return shell
		}
	}
	for _, shell := range []string{"/bin/zsh", "/bin/bash", "/bin/sh"} {
		if _, err := os.Stat(shell); err == nil {
			return shell
		}
	}
	return "/bin/sh"
}

// passwdShell is the shell of a passwd entry: getent's (local files, LDAP…), else /etc/passwd's.
func passwdShell(name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "getent", "passwd", name).Output(); err == nil {
		if fields := strings.Split(strings.TrimSpace(string(out)), ":"); len(fields) >= 7 {
			return fields[6]
		}
	}
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) >= 7 && fields[0] == name {
			return fields[6]
		}
	}
	return ""
}

// run asks the system something, briefly.
func run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return strings.TrimSpace(string(out)), err
}
