//go:build windows

package platform

// SetServiceUmask is a no-op on Windows; file access is governed by ACLs.
func (osImpl) SetServiceUmask() {}
