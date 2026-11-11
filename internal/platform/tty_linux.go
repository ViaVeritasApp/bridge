//go:build linux

package platform

import "golang.org/x/sys/unix"

const ioctlReadTermios = unix.TCGETS
