package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"bridge/internal/tlsx"
	"bridge/internal/utils"
)

type Config struct {
	APIBaseURL     string `json:"api_base_url"`
	BridgeID       string `json:"bridge_id,omitempty"`
	OrganizationID string `json:"organization_id,omitempty"`
	ClientID       string `json:"client_id,omitempty"`
	ClientSecret   string `json:"client_secret,omitempty"`
	// SigningKey checks the download links the API signs for this bridge's files.
	SigningKey    string   `json:"signing_key,omitempty"`
	AllowedOrigin []string `json:"allowed_origin,omitempty"`
	// Folders are what it syncs, each a folder of its own in the bridge's folder in Files.
	Folders []Folder `json:"folders,omitempty"`
	// LegacyDataRoot is the one folder of a bridge from before Folders, read into them by Load.
	LegacyDataRoot string `json:"data_root,omitempty"`
	Version        string `json:"version"`

	// TLSDomain is the parent zone the API assigns at claim time (e.g.
	// "lan.viaveritas.app"); with Subdomain it locates the per-bridge wildcard
	// cert *.<Subdomain>.<TLSDomain>. Set by `pair`; the cert files live under
	// <Dir()>/tls/ managed by internal/tlsx.
	TLSDomain string `json:"tls_domain,omitempty"`
	Subdomain string `json:"subdomain,omitempty"`

	// File-watch tuning. Zero means "use the default"; read via ScanInterval().
	ScanIntervalSeconds int  `json:"scan_interval_seconds,omitempty"`
	IncludeHidden       bool `json:"include_hidden,omitempty"`

	// SocketLogs streams the service log to ViaVeritas over the socket the
	// bridge keeps open for the API's commands. Defaults true; no omitempty so
	// an explicit false survives a save/reload round-trip instead of being
	// dropped and re-defaulting to true.
	SocketLogs bool `json:"socket_logs"`

	// BridgePort is the port the single bridge HTTP listener binds (all file
	// endpoints + /healthz). Zero means unconfigured; read via BridgeAddr(). The
	// only network port the bridge opens.
	BridgePort int `json:"bridge_port,omitempty"`

	// LogDirectory overrides where the log file is written. Empty means the
	// default ("logs" in Dir()). Read via LogDir().
	LogDirectory string `json:"log_dir,omitempty"`

	LastHeartbeatAt string `json:"last_heartbeat_at,omitempty"`
}

// DefaultScanIntervalSeconds is the stat-scan interval when ScanIntervalSeconds is zero.
const DefaultScanIntervalSeconds = 15

// ScanInterval returns the effective stat-scan interval.
func (c Config) ScanInterval() time.Duration {
	s := c.ScanIntervalSeconds
	if s <= 0 {
		s = DefaultScanIntervalSeconds
	}
	return time.Duration(s) * time.Second
}

// DefaultVersion is the build version, overridable at link time via
// -ldflags "-X bridge/internal/config.DefaultVersion=<tag>".
var DefaultVersion = "0.1.0"

func Defaults() Config {
	return Config{
		APIBaseURL: "https://api.viaveritas.app",
		Version:    DefaultVersion,
		SocketLogs: true,
	}
}

// customConfigPath, when non-empty, overrides the default config location.
// Set once at startup from --config (see SetPath).
var customConfigPath string

// SetPath overrides the config file path (from --config). The service runs with
// no arguments and never calls this, so it always uses the default location.
func SetPath(p string) { customConfigPath = p }

// InstallDir is where `install` puts the program and where the bridge keeps
// its state (config.json, index.txt, logs/, tls/), whichever copy of the
// program runs: a copy unpacked anywhere else finds the same, paired bridge.
func InstallDir() string {
	if installDirOverride != "" {
		return installDirOverride
	}
	if runtime.GOOS == "windows" {
		return `C:\opt\viaveritas\bridge`
	}
	return "/opt/viaveritas/bridge"
}

// installDirOverride stands in for InstallDir in tests.
var installDirOverride string

// SetInstallDir points InstallDir elsewhere, for tests; "" restores it.
func SetInstallDir(dir string) { installDirOverride = dir }

// InstalledBinary is the program the service runs.
func InstalledBinary() string {
	name := "bridge"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(InstallDir(), name)
}

// ExeDir is the directory of the running executable, resolving symlinks so an
// installed/symlinked binary points at its real location. Falls back to CWD, then ".".
func ExeDir() string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		if wd, werr := os.Getwd(); werr == nil {
			return wd
		}
		return "."
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}

// present is true for a file that exists, readable or not.
func present(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}

// stateDir is the installation's, unless only the folder beside the program
// holds a config: a bridge set up before installs copied themselves, until
// `install` moves it.
func stateDir() string {
	install := InstallDir()
	if present(filepath.Join(install, "config.json")) {
		return install
	}
	if exe := ExeDir(); present(filepath.Join(exe, "config.json")) {
		return exe
	}
	return install
}

// Path is the config file location: the --config override, else config.json
// in the bridge's state directory.
func Path() string {
	if customConfigPath != "" {
		return customConfigPath
	}
	return filepath.Join(stateDir(), "config.json")
}

// Dir is the directory containing the config file (also where index.txt and other state live).
func Dir() string { return filepath.Dir(Path()) }

// LogDir returns where the log file is written: the override if set, else a
// "logs" folder next to the executable.
func (c Config) LogDir() string {
	if c.LogDirectory != "" {
		return c.LogDirectory
	}
	return filepath.Join(Dir(), "logs")
}

// LogFile is the service log path, shared by the writer, `status`, and `logs`.
func (c Config) LogFile() string {
	return filepath.Join(c.LogDir(), "bridge.log")
}

// TLSEnabled reports whether the bridge should serve HTTPS: both TLS fields set
// AND a cert+key on disk. Single gate the rest of the code consults; false ⇒ the
// byte-for-byte pre-TLS HTTP path. A paired bridge can lack a cert (ACME failed
// at claim, or no heartbeat yet) and runs HTTP until the renewal cron delivers it.
func (c Config) TLSEnabled() bool {
	if c.TLSDomain == "" || c.Subdomain == "" {
		return false
	}
	_, _, ok := tlsx.Loaded(Dir())
	return ok
}

// BridgeAddr is the bind address for the bridge HTTP listener: 0.0.0.0:BridgePort.
// Returns "" when BridgePort is unconfigured so the service can skip the listener
// and surface a clear "run configure" error to the operator.
func (c Config) BridgeAddr() string {
	if c.BridgePort == 0 {
		return ""
	}
	return net.JoinHostPort("0.0.0.0", strconv.Itoa(c.BridgePort))
}

func Load() (Config, error) {
	cfg := Defaults()
	b, err := os.ReadFile(Path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, fmt.Errorf("config not found at %s — run `bridge pair <code>` first", Path())
		}
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("invalid config: %w", err)
	}
	// The version is the running build's: config.json keeps whichever binary
	// saved it last, and an upgrade only replaces the binary.
	cfg.Version = DefaultVersion
	if len(cfg.Folders) == 0 && cfg.LegacyDataRoot != "" {
		root := cfg.LegacyDataRoot
		if !filepath.IsAbs(root) {
			root = filepath.Join(Dir(), root)
		}
		root = filepath.Clean(root)
		cfg.Folders = []Folder{{ID: folderID(root), Name: defaultFolderName(root, nil), Path: root}}
	}
	cfg.LegacyDataRoot = ""
	return cfg, nil
}

// LoadOrDefaults returns Load()'s config, falling back to Defaults() on error.
// Used by commands that must proceed even without an existing config.json.
func LoadOrDefaults() Config {
	if cfg, err := Load(); err == nil {
		return cfg
	}
	return Defaults()
}

// LoadOr returns a freshly loaded config, falling back to the caller's snapshot
// on error, so a long-running component picks up `configure`/`pair` edits
// without a restart and never regresses to defaults.
func LoadOr(fallback Config) Config {
	if fresh, err := Load(); err == nil {
		return fresh
	}
	return fallback
}

func Save(cfg Config) error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	// Folders aren't made here: one that isn't there (an unplugged drive) must
	// not come back as an empty folder on another disk.
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	// Atomic rename over the target: config.json is only ever replaced by a
	// complete, valid file — a crash mid-write leaves the old one intact.
	return utils.AtomicWrite(Path(), b, 0o600)
}
