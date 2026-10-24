// Package metrics samples how the computer is doing: processor, load,
// memory, disks and network, every Interval while the metrics feature runs,
// and every LiveInterval while someone in ViaVeritas watches. Each sample is
// sent at once and kept there with its time; samples wait here while the
// socket is down (24 hours of them at most, the oldest dropped first), so a
// socket that drops loses nothing it gets back to.
package metrics

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	gnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/sensors"

	"bridge/internal/inventory"
)

const (
	// Interval is how often the computer is sampled.
	Interval = 30 * time.Second
	// LiveInterval is how often while someone watches.
	LiveInterval = 2 * time.Second
	// Keep is how many samples wait at most: 24 hours of them.
	Keep = int(24 * time.Hour / Interval)
	// Batch is the most samples one message carries, well under the socket's 1 MB.
	Batch = 240
)

// Value is one figure of a sample: what (M), of what (D: a disk's mount, an
// interface; "" for the computer as a whole), and how much (V).
type Value struct {
	M string  `json:"m"`
	D string  `json:"d,omitempty"`
	V float64 `json:"v"`
}

// Sample is the computer at one moment (T, Unix milliseconds).
type Sample struct {
	T int64   `json:"t"`
	V []Value `json:"v"`
}

// Sampler takes samples; it keeps the counters it measures rates from, so
// keep one per cadence.
type Sampler struct {
	cpu     cpu.TimesStat
	haveCPU bool
	net     map[string]gnet.IOCountersStat
	disk    map[string]disk.IOCountersStat
	at      time.Time

	volumes   []inventory.Disk
	volumesAt time.Time
	ifaces    map[string]bool
	ifacesAt  time.Time
}

// Take samples the computer now. Rates (CPU, network, disk) need a sample
// before them, so the first one leaves them out.
func (s *Sampler) Take(ctx context.Context) Sample {
	now := time.Now()
	out := Sample{T: now.UnixMilli()}
	add := func(m, d string, v float64) { out.V = append(out.V, Value{M: m, D: d, V: round(v)}) }
	elapsed := now.Sub(s.at).Seconds()
	first := s.at.IsZero() || elapsed <= 0

	if times, err := cpu.TimesWithContext(ctx, false); err == nil && len(times) > 0 {
		t := times[0]
		if s.haveCPU {
			total := t.Total() - s.cpu.Total()
			idle := (t.Idle + t.Iowait) - (s.cpu.Idle + s.cpu.Iowait)
			if total > 0 {
				add("cpu_percent", "", clamp(100*(total-idle)/total))
			}
		}
		s.cpu, s.haveCPU = t, true
	}
	if runtime.GOOS != "windows" {
		if l, err := load.AvgWithContext(ctx); err == nil {
			add("load1", "", l.Load1)
			add("load5", "", l.Load5)
			add("load15", "", l.Load15)
		}
	}
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		add("mem_used_percent", "", vm.UsedPercent)
		add("mem_used_bytes", "", float64(vm.Used))
		add("mem_total_bytes", "", float64(vm.Total))
	}
	if sw, err := mem.SwapMemoryWithContext(ctx); err == nil && sw.Total > 0 {
		add("swap_used_bytes", "", float64(sw.Used))
	}

	if s.volumes == nil || now.Sub(s.volumesAt) > 5*time.Minute {
		s.volumes, s.volumesAt = inventory.Volumes(ctx), now
	}
	for _, v := range s.volumes {
		if u, err := disk.UsageWithContext(ctx, v.Mount); err == nil && u.Total > 0 {
			add("disk_used_percent", v.Mount, u.UsedPercent)
			add("disk_used_bytes", v.Mount, float64(u.Used))
			add("disk_free_bytes", v.Mount, float64(u.Free))
		}
	}
	if io, err := disk.IOCountersWithContext(ctx); err == nil {
		var read, write float64
		for name, c := range io {
			if before, ok := s.disk[name]; ok && !first {
				read += float64(c.ReadBytes - min(c.ReadBytes, before.ReadBytes))
				write += float64(c.WriteBytes - min(c.WriteBytes, before.WriteBytes))
			}
		}
		if !first {
			add("disk_read_bps", "", read/elapsed)
			add("disk_write_bps", "", write/elapsed)
		}
		s.disk = io
	}

	if s.ifaces == nil || now.Sub(s.ifacesAt) > 5*time.Minute {
		s.ifaces, s.ifacesAt = interfaces(ctx), now
	}
	if counters, err := gnet.IOCountersWithContext(ctx, true); err == nil {
		next := make(map[string]gnet.IOCountersStat, len(counters))
		for _, c := range counters {
			next[c.Name] = c
			if !s.ifaces[c.Name] {
				continue
			}
			if before, ok := s.net[c.Name]; ok && !first {
				add("net_rx_bps", c.Name, float64(c.BytesRecv-min(c.BytesRecv, before.BytesRecv))/elapsed)
				add("net_tx_bps", c.Name, float64(c.BytesSent-min(c.BytesSent, before.BytesSent))/elapsed)
			}
		}
		s.net = next
	}

	if temps, err := sensors.TemperaturesWithContext(ctx); err == nil || len(temps) > 0 {
		hottest := 0.0
		for _, t := range temps {
			// Readings outside what a part can be are a sensor that isn't there.
			if t.Temperature > hottest && t.Temperature < 150 {
				hottest = t.Temperature
			}
		}
		if hottest > 0 {
			add("temp_max_c", "", hottest)
		}
	}

	s.at = now
	return out
}

// interfaces are those worth measuring: up, not the loopback, with an address beyond their own link.
func interfaces(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	list, err := gnet.InterfacesWithContext(ctx)
	if err != nil {
		return out
	}
	for _, it := range list {
		up, loop := false, false
		for _, f := range it.Flags {
			up = up || f == "up"
			loop = loop || f == "loopback"
		}
		if !up || loop {
			continue
		}
		for _, a := range it.Addrs {
			if !strings.HasPrefix(a.Addr, "fe80:") && !strings.HasPrefix(a.Addr, "169.254.") {
				out[it.Name] = true
				break
			}
		}
	}
	return out
}

func clamp(v float64) float64 { return max(0, min(100, v)) }

// round keeps two decimals: more says nothing and costs bytes.
func round(v float64) float64 {
	if v >= 1e6 || v <= -1e6 {
		return float64(int64(v))
	}
	return float64(int64(v*100)) / 100
}

// Buffer holds samples until they are sent.
type Buffer struct {
	mu      sync.Mutex
	pending []Sample
}

// Add keeps a sample, dropping the oldest past Keep.
func (b *Buffer) Add(s Sample) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = append(b.pending, s)
	if over := len(b.pending) - Keep; over > 0 {
		b.pending = append([]Sample(nil), b.pending[over:]...)
	}
}

// Take removes and returns the oldest samples, Batch at most.
func (b *Buffer) Take() []Sample {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := min(len(b.pending), Batch)
	out := append([]Sample(nil), b.pending[:n]...)
	b.pending = b.pending[n:]
	return out
}

// Return puts samples that couldn't be sent back in front.
func (b *Buffer) Return(samples []Sample) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = append(append([]Sample(nil), samples...), b.pending...)
	if over := len(b.pending) - Keep; over > 0 {
		b.pending = b.pending[over:]
	}
}

// Len is how many wait.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending)
}
