// Package uploadgate is the bridge's sync lock and prepared-upload registry:
// the sync loop calls Locked() each tick and skips scanning while an upload is
// pending. Entries auto-expire so a never-confirmed upload can't wedge the loop.
package uploadgate

import (
	"sync"
	"time"

	"bridge/internal/watch"
)

// Prepared is one declared-but-not-yet-confirmed upload.
type Prepared struct {
	Name           string
	Path           string // key; root-relative path under the data root
	Description    string
	OrganizationID string               // forwarded as ?organizationId to the sync-files call on /confirm
	Resources      []watch.FileRelation // resource attribution as supplied by the CRM, forwarded to /sync
	Size           int64
	Expiry         time.Time
}

var (
	mu      sync.Mutex
	pending = map[string]*Prepared{}
)

// Add registers (or replaces) a prepared upload, keyed by its Path.
func Add(p *Prepared) {
	mu.Lock()
	defer mu.Unlock()
	pending[p.Path] = p
}

// Take removes and returns the prepared upload for path. ok is false if no
// (live) entry exists; expired entries are pruned and treated as absent.
func Take(path string) (*Prepared, bool) {
	mu.Lock()
	defer mu.Unlock()
	pruneLocked()
	p, ok := pending[path]
	if !ok {
		return nil, false
	}
	delete(pending, path)
	return p, true
}

// Locked reports whether the sync should pause: true while any non-expired
// prepared upload is pending. Prunes first, so a never-confirmed upload
// releases the lock at its Expiry.
func Locked() bool {
	mu.Lock()
	defer mu.Unlock()
	pruneLocked()
	return len(pending) > 0
}

// pruneLocked drops expired entries. Caller must hold mu.
func pruneLocked() {
	now := time.Now()
	for k, p := range pending {
		if now.After(p.Expiry) {
			delete(pending, k)
		}
	}
}

const (
	lockBytesPerSec = 512 * 1024 // 0.5 MB/s assumed upload throughput
	lockMin         = 1 * time.Minute
	lockMax         = 15 * time.Minute
)

// LockDuration maps an upload size to how long the sync stays locked: linear at
// an assumed 0.5 MB/s, clamped to [1min, 15min].
func LockDuration(sizeBytes int64) time.Duration {
	if sizeBytes <= 0 {
		return lockMin
	}
	d := time.Duration(sizeBytes/lockBytesPerSec) * time.Second
	if d < lockMin {
		return lockMin
	}
	if d > lockMax {
		return lockMax
	}
	return d
}
