package terminal

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"

	"bridge/internal/config"
)

// Resolve is the account sessions run as now: SYSTEM (what the service runs
// as), or the person signed in at the console, whose identity the service
// takes from Windows (WTSQueryUserToken) without a password.
func Resolve(runAs config.RunAs) (*Account, error) {
	shell := powershell()
	if runAs == config.RunAsSystem {
		return &Account{Name: "SYSTEM", System: true, Home: os.Getenv("SystemRoot"), Shell: shell}, nil
	}
	session := windows.WTSGetActiveConsoleSessionId()
	if session == 0xFFFFFFFF {
		return nil, ErrNobody
	}
	var token windows.Token
	if err := windows.WTSQueryUserToken(session, &token); err != nil {
		if err == windows.ERROR_NO_TOKEN {
			return nil, ErrNobody
		}
		return nil, fmt.Errorf("the signed-in person's identity couldn't be taken: %w", err)
	}
	a := &Account{Shell: shell, token: uintptr(token)}
	if tu, err := token.GetTokenUser(); err == nil {
		if name, domain, _, err := tu.User.Sid.LookupAccount(""); err == nil {
			a.Name = name
			if domain != "" {
				a.Name = domain + `\` + name
			}
		}
	}
	if home, err := token.GetUserProfileDirectory(); err == nil {
		a.Home = home
	}
	if a.Name == "" {
		a.Name = "the signed-in person"
	}
	return a, nil
}

// Release closes the token taken from Windows.
func (a *Account) Release() {
	if a.token != 0 {
		_ = windows.Token(a.token).Close()
		a.token = 0
	}
}

// env: nil lets Windows build the account's own (CreateEnvironmentBlock, for a token).
func (a *Account) env() []string { return nil }

// sysProcAttr runs as the account's token, without a window of its own.
func (a *Account) sysProcAttr(_ bool) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Token: syscall.Token(a.token), HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

func (a *Account) shellArgs() []string { return []string{"-NoLogo"} }

func (a *Account) commandArgs(command string) (string, []string) {
	return a.Shell, []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", command}
}

func powershell() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, `System32\WindowsPowerShell\v1.0\powershell.exe`)
}
