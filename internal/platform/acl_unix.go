//go:build !windows

package platform

import (
	"fmt"
	"os"
)

// HardenDir keeps dir writable by its owner (root) alone. What holds secrets
// in it is written 0600 (utils.AtomicWrite), so others read only the program
// and the logs.
func (osImpl) HardenDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o022 != 0 {
		if err := os.Chmod(dir, 0o755); err != nil {
			return fmt.Errorf("set the permissions of %s: %w", dir, err)
		}
	}
	return nil
}
