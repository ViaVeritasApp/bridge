//go:build !windows

package platform

import "syscall"

// SetServiceUmask sets the service umask to 022 so uploaded files are 0644 and
// new dirs 0755 — still root-owned, but readable/manageable by the clinic
// rather than locked down by a restrictive inherited umask. The bridge's own
// secrets set their mode explicitly via utils.AtomicWrite, so this doesn't
// loosen them.
func (osImpl) SetServiceUmask() { syscall.Umask(0o022) }
