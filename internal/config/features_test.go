package config

import (
	"encoding/json"
	"testing"
)

func TestFeatureDefaults(t *testing.T) {
	c := Defaults()
	for _, f := range Features {
		want := !f.Remote() && f != FeatureInventoryPersonal
		if c.Allows(f) != want {
			t.Errorf("%s allowed = %v by default, want %v", f, c.Allows(f), want)
		}
		if !c.PortalAllows(f) {
			t.Errorf("%s off in ViaVeritas before it said anything", f)
		}
	}
	if !c.Effective(FeatureFiles) || c.Effective(FeatureTerminal) {
		t.Fatal("files run and the terminal doesn't, by default")
	}
}

func TestBothSidesDecide(t *testing.T) {
	c := Defaults().WithFeature(FeatureTerminal, true)
	if !c.Effective(FeatureTerminal) {
		t.Fatal("on here, unsaid in ViaVeritas: runs")
	}
	off, changed := c.WithPortalFeatures(map[Feature]bool{FeatureTerminal: false, "made_up": false})
	if !changed || off.Effective(FeatureTerminal) {
		t.Fatal("off in ViaVeritas: doesn't run")
	}
	if _, ok := off.PortalFeatures["made_up"]; ok {
		t.Fatal("kept a feature this bridge doesn't know")
	}
	if _, again := off.WithPortalFeatures(map[Feature]bool{FeatureTerminal: false}); again {
		t.Fatal("the same switches counted as a change")
	}
	paused := c
	paused.RemotePaused = true
	if paused.Effective(FeatureTerminal) || !paused.Effective(FeatureFiles) {
		t.Fatal("pausing holds the remote features only")
	}
	if !SwitchesChanged(c, paused) || !SwitchesChanged(c, off) || SwitchesChanged(c, c) {
		t.Fatal("SwitchesChanged")
	}
	here := c.WithFeature(FeatureFiles, false)
	if c.Allows(FeatureFiles) != true || here.Allows(FeatureFiles) {
		t.Fatal("WithFeature changed the config it was called on")
	}
}

func TestFeaturesSurviveSaving(t *testing.T) {
	c := Defaults().WithFeature(FeatureTerminal, true)
	c, _ = c.WithPortalFeatures(map[Feature]bool{FeatureFiles: false})
	c.RemotePaused = true
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var back Config
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Allows(FeatureTerminal) || back.PortalAllows(FeatureFiles) || !back.RemotePaused {
		t.Fatalf("read back %s as %+v", b, back)
	}
}

// The system's usual logs are listed until the list is edited here; one taken off stays off.
func TestDefaultLogSources(t *testing.T) {
	c := Defaults()
	defaults := DefaultLogSources()
	if got := c.ListedLogSources(); len(got) != len(defaults)+1 || got[0].ID != BridgeLogID {
		t.Fatalf("listed %+v, defaults %+v", got, defaults)
	}
	for _, s := range defaults {
		if err := ValidateLogSource(s); err != nil {
			t.Errorf("%s: %v", s.ID, err)
		}
	}
	if len(defaults) == 0 {
		t.Skip("this system has none of the usual logs")
	}
	off, ok := c.WithoutLogSource(defaults[0].ID)
	if !ok || off.LogSources == nil || len(off.ConfiguredLogSources()) != len(defaults)-1 {
		t.Fatalf("taking %s off: %v %+v", defaults[0].ID, ok, off.LogSources)
	}
	b, _ := json.Marshal(off)
	var back Config
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.LogSources == nil || len(back.ConfiguredLogSources()) != len(defaults)-1 {
		t.Fatalf("read back %+v from %s", back.LogSources, b)
	}
	if _, ok := back.LogSourceByID(defaults[0].ID); ok {
		t.Fatal("a usual log taken off came back")
	}
	// Every one taken off: an empty list, not the usual logs again.
	empty := back
	for _, s := range back.ConfiguredLogSources() {
		empty, _ = empty.WithoutLogSource(s.ID)
	}
	b, _ = json.Marshal(empty)
	var none Config
	_ = json.Unmarshal(b, &none)
	if len(none.ConfiguredLogSources()) != 0 {
		t.Fatalf("an emptied list came back as %+v", none.ConfiguredLogSources())
	}
}
