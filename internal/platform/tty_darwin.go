//go:build darwin

package platform

import "golang.org/x/sys/unix"

const ioctlReadTermios = unix.TIOCGETA
