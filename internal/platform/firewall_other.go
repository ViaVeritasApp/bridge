//go:build !windows

package platform

// firewallRuleName is the firewall rule's display name / stable key. Declared
// on both platforms so call sites compile everywhere; only Windows installs a
// rule.
const firewallRuleName = "ViaVeritas LAN Bridge"

// EnsureFirewallRule is a no-op on macOS/Linux: only Windows needs an explicit
// inbound-allow rule.
func (osImpl) EnsureFirewallRule() error { return nil }

// RemoveFirewallRule is a no-op on macOS/Linux.
func (osImpl) RemoveFirewallRule() error { return nil }

// FirewallRuleName returns the stable display name of the bridge firewall rule.
func (osImpl) FirewallRuleName() string { return firewallRuleName }
