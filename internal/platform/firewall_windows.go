//go:build windows

package platform

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// firewallRuleName is the display name and stable key of the Windows Defender
// Firewall rule (find/delete for idempotent Ensure and Remove); must not change
// between releases without a migration.
const firewallRuleName = "ViaVeritas LAN Bridge"

// ruleProfiles selects which network profiles the rule applies to. Accepted
// `netsh advfirewall` values:
//
//	"any"            -> domain, private and public networks
//	"domain,private" -> exclude public (e.g. coffee-shop) networks
//	"private"        -> private (home/work) networks only
//	"domain"         -> domain-joined networks only
//
// Default excludes "public": if the bridge stays unreachable, the host network
// is likely classified Public — set it to Private, or "any".
const ruleProfiles = "domain,private"

// FirewallRuleName returns the stable display name of the bridge firewall rule.
func (osImpl) FirewallRuleName() string { return firewallRuleName }

// EnsureFirewallRule idempotently installs an inbound-allow rule for the bridge
// exe: any prior rule with the same name is deleted first (a "no rule matched"
// result ignored), then re-added — which also picks up a moved exe path.
func (osImpl) EnsureFirewallRule() error {
	exe, err := bridgeExePath()
	if err != nil {
		return fmt.Errorf("resolve bridge executable: %w", err)
	}

	// Best-effort delete of any stale rule; a missing rule (netsh exits
	// non-zero with "No rules match...") is deliberately ignored.
	_, _ = deleteRule()

	out, err := runNetsh(
		"advfirewall", "firewall", "add", "rule",
		"name="+firewallRuleName,
		"dir=in",
		"action=allow",
		"protocol=tcp",
		"program="+exe,
		"profile="+ruleProfiles,
		"enable=yes",
		"description=Allows the ViaVeritas CRM on the LAN to reach this bridge.",
	)
	if err != nil {
		return fmt.Errorf("netsh add rule failed: %w (output: %s)", err, out)
	}
	return nil
}

// RemoveFirewallRule deletes the bridge firewall rule by name; a missing rule
// is not an error (uninstall is idempotent).
func (osImpl) RemoveFirewallRule() error {
	out, err := deleteRule()
	if err != nil && !isNoRuleMatched(out) {
		return fmt.Errorf("netsh delete rule failed: %w (output: %s)", err, out)
	}
	return nil
}

// deleteRule removes every rule named firewallRuleName. netsh exits non-zero
// when no rule matches; the caller decides whether that's benign.
func deleteRule() (string, error) {
	return runNetsh("advfirewall", "firewall", "delete", "rule", "name="+firewallRuleName)
}

// isNoRuleMatched reports whether netsh output indicates no rule existed to
// delete (benign for an idempotent delete).
func isNoRuleMatched(out string) bool {
	return strings.Contains(strings.ToLower(out), "no rules match")
}

// runNetsh runs `netsh <args...>` with the console window hidden (HideWindow
// suppresses the console flash when the elevated setup-UI spawns netsh),
// returning trimmed combined output. Each key=value is its own argv element, so
// an exe path with spaces needs no quoting.
func runNetsh(args ...string) (string, error) {
	cmd := exec.Command("netsh", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// bridgeExePath returns the absolute path of the running bridge executable for
// the rule's program= key. Symlinks are resolved so the rule keys on the real
// image path (also normalizes, harmless on Windows); on a resolve failure the
// raw os.Executable() value is used.
func bridgeExePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	return filepath.Clean(exe), nil
}
