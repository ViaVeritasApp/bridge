package utils

import (
	"io/fs"
	"os"
	"path/filepath"
)

// AtomicWrite writes data to a temp file in path's directory, chmods it to mode,
// then atomically renames it over path — so a crash mid-write never leaves a
// half-written file. The caller must create the directory first.
func AtomicWrite(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
