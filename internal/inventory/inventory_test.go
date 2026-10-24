package inventory

import (
	"context"
	"encoding/json"
	"testing"
)

func TestCollectDescribesThisComputer(t *testing.T) {
	inv := Collect(context.Background(), true, BridgeFacts{Version: "test", PID: 1})
	if inv.Host.Hostname == "" || inv.Host.OS == "" {
		t.Errorf("no host: %+v", inv.Host)
	}
	if inv.CPU.Threads <= 0 || inv.Memory.Total == 0 {
		t.Errorf("no processor or memory: %+v %+v", inv.CPU, inv.Memory)
	}
	if len(inv.Disks) == 0 {
		t.Error("no disk")
	}
	if inv.Personal == nil {
		t.Error("asked for who uses it, got nothing")
	}
	if Collect(context.Background(), false, BridgeFacts{}).Personal != nil {
		t.Error("who uses it was collected without asking")
	}
	b, _ := json.MarshalIndent(inv, "", "  ")
	t.Logf("%s (%d bytes)", b, len(b))

	again := Collect(context.Background(), true, BridgeFacts{Version: "test", PID: 2})
	if inv.Hash() != again.Hash() {
		t.Error("the same computer hashed differently")
	}
}
