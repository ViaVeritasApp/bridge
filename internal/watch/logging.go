package watch

import (
	"sync"
	"time"

	"bridge/internal/logx"
)

// syncLog rate-limits steady-state lines so `bridge logs` (no BRIDGE_DEBUG)
// shows scanner activity without one line every 15s forever.
var syncLog struct {
	mu        sync.Mutex
	lastSkip  string
	caughtN   int
	caughtAt  time.Time
	caughtAny bool
}

// logCaughtUp emits the "nothing to sync" line at Ok on first run, on
// file-count change, or at most every 5 min — Debug in between. A non-zero
// skipped count is always surfaced at Warn.
func logCaughtUp(n, skipped int, took time.Duration) {
	syncLog.mu.Lock()
	loud := !syncLog.caughtAny || n != syncLog.caughtN ||
		time.Since(syncLog.caughtAt) > 5*time.Minute
	if loud {
		syncLog.caughtAny = true
		syncLog.caughtN = n
		syncLog.caughtAt = time.Now()
	}
	syncLog.mu.Unlock()

	switch {
	case skipped > 0:
		logx.Warn("watch", "caught up: %d file(s) indexed but %d unreadable & skipped — check folder permissions (scan %s)", n, skipped, took)
	case loud:
		logx.Ok("watch", "caught up: %d file(s) indexed, nothing to sync (scan %s)", n, took)
	default:
		logx.Debug("watch", "no changes (%d file(s), scan %s)", n, took)
	}
}

// resetCaughtUp forces the next caught-up state to log loudly again (called
// after any real sync so steady state is re-announced).
func resetCaughtUp() {
	syncLog.mu.Lock()
	syncLog.caughtAny = false
	syncLog.mu.Unlock()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
