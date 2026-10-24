package main

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"bridge/internal/apisocket"
	"bridge/internal/config"
	"bridge/internal/inventory"
	"bridge/internal/logx"
	"bridge/internal/metrics"
	"bridge/internal/speedtest"
)

// How often the inventory is collected again when nothing says it changed.
const inventoryEvery = time.Hour

// The longest one `live` request keeps live samples coming: ViaVeritas asks again while someone watches.
const liveAtMost = 2 * time.Minute

// monitoring is what the bridge says of its computer: the inventory when it
// changes, a sample every metrics.Interval and every metrics.LiveInterval while
// someone watches (each sent at once, kept while the socket is down; ViaVeritas
// keeps them all), and a speedtest when asked. Each runs only while its feature does.
type monitoring struct {
	live   *config.Live
	buffer metrics.Buffer

	invMu    sync.Mutex
	inv      *inventory.Inventory
	invHash  string
	sentHash string
	recheck  chan struct{}

	liveMu    sync.Mutex
	liveUntil time.Time
	// wake has the sampling loop start on the live pace at once.
	wake chan struct{}

	testing sync.Mutex
}

func newMonitoring(live *config.Live) *monitoring {
	m := &monitoring{live: live, recheck: make(chan struct{}, 1), wake: make(chan struct{}, 1)}
	live.OnChange(func(old, next config.Config) {
		for _, f := range []config.Feature{config.FeatureInventory, config.FeatureInventoryPersonal} {
			if old.Effective(f) != next.Effective(f) {
				select {
				case m.recheck <- struct{}{}:
				default:
				}
			}
		}
	})
	apisocket.OnConnect(m.connected)
	apisocket.Handle("live", m.handleLive)
	apisocket.Handle("speedtest", m.handleSpeedtest)
	return m
}

func (m *monitoring) run(ctx context.Context, p *program) {
	p.run(func() { m.inventoryLoop(ctx) })
	p.run(func() { m.metricsLoop(ctx) })
}

// connected sends what ViaVeritas may have missed: the inventory, and the samples that waited.
func (m *monitoring) connected(ctx context.Context) {
	m.invMu.Lock()
	m.sentHash = ""
	m.invMu.Unlock()
	m.sendInventory()
	m.flush()
}

func (m *monitoring) inventoryLoop(ctx context.Context) {
	for {
		cfg := m.live.Get()
		if cfg.Effective(config.FeatureInventory) {
			inv := inventory.Collect(ctx, cfg.Effective(config.FeatureInventoryPersonal), inventory.BridgeFacts{
				Version: config.DefaultVersion, InstallDir: config.InstallDir(), PID: os.Getpid(),
			})
			m.invMu.Lock()
			m.inv, m.invHash = &inv, inv.Hash()
			m.invMu.Unlock()
			m.sendInventory()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(inventoryEvery):
		case <-m.recheck:
		}
	}
}

// sendInventory sends the inventory when ViaVeritas doesn't have this one yet.
func (m *monitoring) sendInventory() {
	m.invMu.Lock()
	inv, hash, sent := m.inv, m.invHash, m.sentHash
	m.invMu.Unlock()
	if inv == nil || hash == sent || !m.live.Get().Effective(config.FeatureInventory) {
		return
	}
	if err := apisocket.Emit("inventory", map[string]any{"hash": hash, "inventory": inv}); err != nil {
		return
	}
	m.invMu.Lock()
	m.sentHash = hash
	m.invMu.Unlock()
	logx.Debug("monitoring", "sent the inventory")
}

// metricsLoop samples the computer and sends each sample at once: every
// metrics.Interval, and every metrics.LiveInterval while someone watches.
func (m *monitoring) metricsLoop(ctx context.Context) {
	var sampler metrics.Sampler
	sampler.Take(ctx) // rates need one before
	for {
		wait := metrics.Interval
		if m.watched() {
			wait = metrics.LiveInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
			continue
		case <-time.After(wait):
		}
		if m.live.Get().Effective(config.FeatureMetrics) {
			m.buffer.Add(sampler.Take(ctx))
			m.flush()
		}
	}
}

// flush sends the waiting samples, a batch at a time, while the socket takes them.
func (m *monitoring) flush() {
	for m.buffer.Len() > 0 {
		batch := m.buffer.Take()
		if err := apisocket.Emit("metrics", map[string]any{"samples": batch}); err != nil {
			m.buffer.Return(batch)
			return
		}
	}
}

// watched reports whether someone watches the computer in ViaVeritas now.
func (m *monitoring) watched() bool {
	m.liveMu.Lock()
	defer m.liveMu.Unlock()
	return time.Now().Before(m.liveUntil)
}

// handleLive: someone watches the computer in ViaVeritas, so samples come every
// metrics.LiveInterval for as long as asked (two minutes at most; it asks again).
func (m *monitoring) handleLive(_ context.Context, data json.RawMessage) any {
	var req struct {
		Seconds int `json:"seconds"`
	}
	_ = json.Unmarshal(data, &req)
	if !m.live.Get().Effective(config.FeatureMetrics) {
		return map[string]any{"ok": false, "code": "feature_off"}
	}
	until := time.Now().Add(min(time.Duration(max(req.Seconds, 1))*time.Second, liveAtMost))
	m.liveMu.Lock()
	start := !time.Now().Before(m.liveUntil)
	if until.After(m.liveUntil) {
		m.liveUntil = until
	}
	m.liveMu.Unlock()
	if start {
		select {
		case m.wake <- struct{}{}:
		default:
		}
	}
	return map[string]any{"ok": true}
}

// handleSpeedtest runs a speedtest against the API, one at a time.
func (m *monitoring) handleSpeedtest(ctx context.Context, data json.RawMessage) any {
	var req speedtest.Request
	if err := json.Unmarshal(data, &req); err != nil || req.Token == "" {
		return speedtest.Result{Code: "invalid", Message: "unreadable request"}
	}
	cfg := m.live.Get()
	if !cfg.Effective(config.FeatureSpeedtest) {
		return speedtest.Result{Code: "feature_off", Message: "speedtests are switched off for this bridge"}
	}
	if !m.testing.TryLock() {
		return speedtest.Result{Code: "busy", Message: "a speedtest is running already"}
	}
	defer m.testing.Unlock()
	logx.Info("monitoring", "running a speedtest for ViaVeritas")
	res := speedtest.Run(ctx, cfg.APIBaseURL, req)
	if res.OK {
		logx.Ok("monitoring", "speedtest: %.1f ms, %.1f Mbit/s down, %.1f Mbit/s up", res.LatencyMS.Avg, res.DownloadBPS/1e6, res.UploadBPS/1e6)
	} else {
		logx.Warn("monitoring", "speedtest failed: %s", res.Message)
	}
	return res
}
