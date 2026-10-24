package metrics

import (
	"context"
	"testing"
	"time"
)

func TestSampleThisComputer(t *testing.T) {
	var s Sampler
	first := s.Take(context.Background())
	// macOS updates its processor counters about once a second when busy: a shorter wait can read them unchanged.
	time.Sleep(1200 * time.Millisecond)
	second := s.Take(context.Background())
	has := func(smp Sample, m string) bool {
		for _, v := range smp.V {
			if v.M == m {
				return true
			}
		}
		return false
	}
	if has(first, "cpu_percent") {
		t.Error("a rate in the first sample")
	}
	for _, m := range []string{"cpu_percent", "mem_used_percent", "disk_used_percent", "net_rx_bps"} {
		if !has(second, m) {
			t.Errorf("no %s in %+v", m, second.V)
		}
	}
	for _, v := range second.V {
		if v.M == "cpu_percent" && (v.V < 0 || v.V > 100) {
			t.Errorf("cpu at %v%%", v.V)
		}
	}
	t.Logf("%d values: %+v", len(second.V), second.V)
}

func TestBufferKeepsTheNewest(t *testing.T) {
	var b Buffer
	for i := 0; i < Keep+10; i++ {
		b.Add(Sample{T: int64(i)})
	}
	if b.Len() != Keep {
		t.Fatalf("kept %d, want %d", b.Len(), Keep)
	}
	batch := b.Take()
	if len(batch) != Batch || batch[0].T != 10 {
		t.Fatalf("took %d from %d", len(batch), batch[0].T)
	}
	b.Return(batch[:2])
	if again := b.Take(); again[0].T != 10 {
		t.Fatalf("a returned sample went to the back: %d", again[0].T)
	}
}
