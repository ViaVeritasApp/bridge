package config

import (
	"context"
	"os"
	"sync"
	"time"
)

// Live is the config as it is on disk, for the parts of the service that run
// all along: it reads the file again when it changes (checked every 2 s, or at
// once with Reload) and tells its listeners. A file it can't read keeps the
// config it had.
type Live struct {
	mu        sync.Mutex
	cur       Config
	mod       time.Time
	size      int64
	listeners []func(old, next Config)
}

// LiveInterval is how often Live looks at the file.
const LiveInterval = 2 * time.Second

// NewLive starts from cfg, as it was just read.
func NewLive(cfg Config) *Live {
	l := &Live{cur: cfg}
	if info, err := os.Stat(Path()); err == nil {
		l.mod, l.size = info.ModTime(), info.Size()
	}
	return l
}

// Get is the config as it was last read.
func (l *Live) Get() Config {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cur
}

// OnChange calls fn with the config before and after each change it reads,
// in the goroutine that read it.
func (l *Live) OnChange(fn func(old, next Config)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listeners = append(l.listeners, fn)
}

// Reload reads the file now if it changed since the last read.
func (l *Live) Reload() {
	info, err := os.Stat(Path())
	if err != nil {
		return
	}
	l.mu.Lock()
	if info.ModTime().Equal(l.mod) && info.Size() == l.size {
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()

	next, err := Load()
	if err != nil {
		return
	}
	l.mu.Lock()
	old := l.cur
	l.cur, l.mod, l.size = next, info.ModTime(), info.Size()
	listeners := append([]func(old, next Config){}, l.listeners...)
	l.mu.Unlock()
	for _, fn := range listeners {
		fn(old, next)
	}
}

// Run reads the file again whenever it changes, until ctx ends.
func (l *Live) Run(ctx context.Context) {
	t := time.NewTicker(LiveInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.Reload()
		}
	}
}

// SwitchesChanged reports whether anything that decides what runs differs
// between a and b: a feature's switch here or in ViaVeritas, or the pause.
func SwitchesChanged(a, b Config) bool {
	if a.RemotePaused != b.RemotePaused {
		return true
	}
	for _, f := range Features {
		if a.Allows(f) != b.Allows(f) || a.PortalAllows(f) != b.PortalAllows(f) {
			return true
		}
	}
	return false
}
