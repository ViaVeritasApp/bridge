// Package platform abstracts OS-specific bridge behavior behind one
// OperatingSystem interface; exactly one osImpl is compiled per GOOS (build
// tags select it, required because impls use OS-only APIs that don't compile
// elsewhere), and Current returns it. osImpl is declared once per GOOS in
// os_<goos>.go; its methods live across the per-concern build-tagged files.
package platform

import (
	"errors"
	"os"

	"github.com/kardianos/service"
)

// OperatingSystem is the single abstraction over per-OS behavior; Current
// returns the macOS/Linux/Windows implementation for this GOOS.
type OperatingSystem interface {
	// EnsureFirewallRule idempotently installs an inbound-allow rule for the
	// bridge exe. Windows adds a Defender rule via netsh; no-op on macOS/Linux.
	EnsureFirewallRule() error
	// RemoveFirewallRule deletes the bridge firewall rule. No-op on macOS/Linux.
	RemoveFirewallRule() error
	// FirewallRuleName is the stable display name / key of the firewall rule.
	FirewallRuleName() string

	// IsAdmin reports whether the current process holds admin/root rights.
	IsAdmin() bool
	// ElevationHint is the human-facing "how to retry elevated" sentence.
	ElevationHint() string
	// GUIElevationAvailable reports whether a native GUI elevation prompt
	// (UAC / osascript / pkexec) can be shown right now.
	GUIElevationAvailable() bool
	// Relaunch re-executes `exe setup` elevated via the native GUI prompt,
	// blocking until it finishes. Returns ErrElevationCancelled if dismissed.
	Relaunch(exe string) error

	// DiskFreeBytes returns the bytes available to the caller on the volume
	// backing path, or 0 if it cannot be determined.
	DiskFreeBytes(path string) int64

	// ComputeFileID derives the stable OS file identity (Unix device+inode,
	// Windows volume serial + NTFS file index) for move detection; ok=false if
	// unresolved.
	ComputeFileID(path string, info os.FileInfo) (id string, ok bool)

	// SetServiceUmask sets the service process umask (Unix: 022) or is a no-op
	// (Windows uses ACLs).
	SetServiceUmask()
	// HardenDir keeps other users out of the bridge's install folder: on
	// Windows only SYSTEM and Administrators may touch it or anything in it,
	// elsewhere only its owner writes to it.
	HardenDir(dir string) error
	// StopService stops the named installed service and guarantees its process
	// is gone (Windows force-terminates a process a graceful stop left alive).
	StopService(s service.Service, name string) error

	// OpenBrowser opens url in the operator's default browser (best-effort).
	OpenBrowser(url string)
	// FilesystemRoot is the folder-browser root: "" on Windows (the drive list
	// stands in for a root), "/" elsewhere.
	FilesystemRoot() string
	// DriveLetters lists mounted drives (Windows C:\..Z:\); nil elsewhere.
	DriveLetters() []DirEntry
	// Breadcrumbs splits absPath into folder-browser breadcrumb segments.
	Breadcrumbs(absPath string) []Crumb
}

// Crumb is one breadcrumb segment for the setup-UI folder browser. JSON tags
// match the wire format the wizard's frontend expects.
type Crumb struct {
	Label string `json:"label"`
	Path  string `json:"path"`
}

// DirEntry is a directory or drive entry for the setup-UI folder browser.
type DirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// ErrNotElevated is returned by Require so cobra RunE can exit non-zero without
// a redundant error line.
var ErrNotElevated = errors.New("insufficient privileges")

// ErrElevationCancelled is returned (via RelaunchElevated) when the user
// dismisses the native OS elevation prompt; the caller should exit quietly.
var ErrElevationCancelled = errors.New("elevation cancelled by user")

// current is the OperatingSystem impl for this GOOS, set by init() in os_<goos>.go.
var current OperatingSystem

// Current returns the OperatingSystem implementation for this GOOS.
func Current() OperatingSystem { return current }
