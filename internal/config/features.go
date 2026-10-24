package config

import "slices"

// Feature is one thing the bridge does that this computer can switch off, and
// ViaVeritas separately too: it runs only while both allow it (Effective).
// Turned off here, the portal can't turn it on.
type Feature string

const (
	FeatureFiles             Feature = "files"
	FeatureInventory         Feature = "inventory"
	FeatureInventoryPersonal Feature = "inventory_personal"
	FeatureMetrics           Feature = "metrics"
	FeatureSpeedtest         Feature = "speedtest"
	FeatureLogs              Feature = "logs"
	// FeatureTerminal is a shell, and commands run one at a time: both run
	// anything on the computer as the same account, so one switch says so.
	FeatureTerminal Feature = "terminal"
	FeatureProxy    Feature = "proxy"
)

// Features is every feature, in the order the dashboard and the portal list them.
var Features = []Feature{
	FeatureFiles, FeatureInventory, FeatureInventoryPersonal, FeatureMetrics, FeatureSpeedtest,
	FeatureLogs, FeatureTerminal, FeatureProxy,
}

// Built is what this version of the bridge can do: the others are kept in its
// settings as they were switched, and offered once a version has them.
var Built = []Feature{FeatureFiles, FeatureInventory, FeatureInventoryPersonal, FeatureMetrics, FeatureSpeedtest, FeatureLogs, FeatureTerminal, FeatureProxy}

// CapabilityUpdates says this version installs a new release when someone asks
// (here or in ViaVeritas). It is no switch: nothing updates by itself, and what
// happens to the bridge (its telemetry) is always told.
const CapabilityUpdates = "updates"

// Capabilities is what this version tells ViaVeritas it can do: its features,
// and updating itself.
func Capabilities() []string {
	out := make([]string, 0, len(Built)+1)
	for _, f := range Built {
		out = append(out, string(f))
	}
	return append(out, CapabilityUpdates)
}

// Protocol is the version of what the bridge and ViaVeritas say over the socket:
// 2 added streams (stream_open, stream_data…) and the feature switches.
const Protocol = 2

// Remote reports whether the feature has someone act on this computer from
// ViaVeritas (a terminal or a command, a request into the network, its logs).
// Those start off, and "Pause all remote access" stops them all at once.
func (f Feature) Remote() bool {
	switch f {
	case FeatureLogs, FeatureTerminal, FeatureProxy:
		return true
	}
	return false
}

// Default is a feature's switch on this computer until someone changes it: the
// remote ones and what names the people using it start off.
func (f Feature) Default() bool {
	return !f.Remote() && f != FeatureInventoryPersonal
}

// Known reports whether f is one of Features.
func (f Feature) Known() bool { return slices.Contains(Features, f) }

// IsBuilt reports whether this version can do f.
func (f Feature) IsBuilt() bool { return slices.Contains(Built, f) }

// Allows is this computer's switch for f.
func (c Config) Allows(f Feature) bool {
	if on, ok := c.Features[f]; ok {
		return on
	}
	return f.Default()
}

// PortalAllows is ViaVeritas's switch for f, as it last said: on until it says otherwise.
func (c Config) PortalAllows(f Feature) bool {
	if on, ok := c.PortalFeatures[f]; ok {
		return on
	}
	return true
}

// Paused reports whether f is held by "Pause all remote access".
func (c Config) Paused(f Feature) bool { return f.Remote() && c.RemotePaused }

// Effective reports whether f runs now: allowed here and in ViaVeritas, and not paused.
func (c Config) Effective(f Feature) bool {
	return c.Allows(f) && c.PortalAllows(f) && !c.Paused(f)
}

// ComputerFeatures is this computer's switch for every feature.
func (c Config) ComputerFeatures() map[Feature]bool {
	out := make(map[Feature]bool, len(Features))
	for _, f := range Features {
		out[f] = c.Allows(f)
	}
	return out
}

// WithFeature is c with this computer's switch for f set to on.
func (c Config) WithFeature(f Feature, on bool) Config {
	next := make(map[Feature]bool, len(c.Features)+1)
	for k, v := range c.Features {
		next[k] = v
	}
	next[f] = on
	c.Features = next
	return c
}

// WithPortalFeatures is c with ViaVeritas's switches as it said them, keeping
// only the features this bridge knows; changed reports whether any differs.
func (c Config) WithPortalFeatures(portal map[Feature]bool) (Config, bool) {
	next := make(map[Feature]bool, len(portal))
	for k, v := range portal {
		if k.Known() {
			next[k] = v
		}
	}
	changed := false
	for _, f := range Features {
		if c.PortalAllows(f) != portalValue(next, f) {
			changed = true
		}
	}
	c.PortalFeatures = next
	return c, changed
}

func portalValue(m map[Feature]bool, f Feature) bool {
	if on, ok := m[f]; ok {
		return on
	}
	return true
}
