//go:build darwin

package platform

import "os/exec"

// openTerminal runs exe in a new Terminal window. Finder does this itself for
// a double-clicked program, so this is only for one started some other way.
func openTerminal(exe string) error {
	return exec.Command("/usr/bin/open", "-a", "Terminal", exe).Run()
}
