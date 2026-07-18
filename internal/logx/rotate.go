package logx

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// rotatingFile is a minimal, dependency-free size-based rotating log sink. When
// the active file would exceed maxBytes it rotates to "<path>.1", older backups
// shift up, and the oldest is discarded — bounding disk use at
// ~(maxBackups+1)*maxBytes. Created world-readable (0644 in a 0755 dir) so
// `bridge logs` works without sudo even though the service writes it as root.
type rotatingFile struct {
	path       string
	maxBytes   int64
	maxBackups int

	mu   sync.Mutex
	f    *os.File
	size int64
}

func newRotatingFile(path string, maxBytes int64, maxBackups int) *rotatingFile {
	return &rotatingFile{path: path, maxBytes: maxBytes, maxBackups: maxBackups}
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.f == nil {
		if err := r.openLocked(); err != nil {
			return 0, err
		}
	}
	if r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// openLocked (re)opens the active file for appending, seeding size from its
// length so a restart continues the same file until it fills.
func (r *rotatingFile) openLocked() error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	// Explicit Chmod: O_CREATE perms are masked by umask, but the log must be
	// world-readable for the unprivileged `logs` command.
	_ = f.Chmod(0o644)
	r.f = f
	r.size = 0
	if fi, serr := f.Stat(); serr == nil {
		r.size = fi.Size()
	}
	return nil
}

func (r *rotatingFile) backup(i int) string {
	return r.path + "." + strconv.Itoa(i)
}

// rotateLocked closes the active file, shifts backups up, drops the oldest, and
// reopens a fresh base file.
func (r *rotatingFile) rotateLocked() error {
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}
	_ = os.Remove(r.backup(r.maxBackups))
	for i := r.maxBackups - 1; i >= 1; i-- {
		_ = os.Rename(r.backup(i), r.backup(i+1))
	}
	_ = os.Rename(r.path, r.backup(1))
	return r.openLocked()
}
