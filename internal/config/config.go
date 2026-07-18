package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"bridge/internal/tlsx"
	"bridge/internal/utils"
)

type Config struct {
	APIBaseURL     string   `json:"api_base_url"`
	BridgeID       string   `json:"bridge_id,omitempty"`
	OrganizationID string   `json:"organization_id,omitempty"`
	ClientID       string   `json:"client_id,omitempty"`
	ClientSecret   string   `json:"client_secret,omitempty"`
	AllowedOrigin  []string `json:"allowed_origin,omitempty"`
	DataRoot       string   `json:"data_root"`
	Version        string   `json:"version"`

	// TLSDomain is the parent zone the API assigns at claim time (e.g.
	// "lan.viaveritas.app"); with Subdomain it locates the per-bridge wildcard
	// cert *.<Subdomain>.<TLSDomain>. Set by `pair`; the cert files live under
	// <Dir()>/tls/ managed by internal/tlsx.
	TLSDomain string `json:"tls_domain,omitempty"`
	Subdomain string `json:"subdomain,omitempty"`

	// File-watch tuning. Zero means "use the default"; read via ScanInterval().
	ScanIntervalSeconds int  `json:"scan_interval_seconds,omitempty"`
	IncludeHidden       bool `json:"include_hidden,omitempty"`

	// SocketLogs streams the service log to ViaVeritas. Defaults true; no
	// omitempty so an explicit false survives a save/reload round-trip instead
	// of being dropped and re-defaulting to true.
	SocketLogs bool `json:"socket_logs"`

	// BridgePort is the port the single bridge HTTP listener binds (all file
	// endpoints + /healthz). Zero means unconfigured; read via BridgeAddr(). The
	// only network port the bridge opens.
	BridgePort int `json:"bridge_port,omitempty"`

	// LogDirectory overrides where the log file is written. Empty means the
	// default ("logs" next to the executable). Read via LogDir().
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
var DefaultVersion = "0.0.7"

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

// exeDir is the directory of the running executable, resolving symlinks so an
// installed/symlinked binary points at its real location. Falls back to CWD, then ".".
func exeDir() string {
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

// Path is the config file location: the --config override, else config.json
// next to the executable.
func Path() string {
	if customConfigPath != "" {
		return customConfigPath
	}
	return filepath.Join(exeDir(), "config.json")
}

// Dir is the directory containing the config file (also where index.txt and other state live).
func Dir() string { return filepath.Dir(Path()) }

// LogDir returns where the log file is written: the override if set, else a
// "logs" folder next to the executable.
func (c Config) LogDir() string {
	if c.LogDirectory != "" {
		return c.LogDirectory
	}
	return filepath.Join(exeDir(), "logs")
}

// LogFile is the service log path, shared by the writer, `status`, and `logs`.
func (c Config) LogFile() string {
	return filepath.Join(c.LogDir(), "bridge.log")
}

// DataDir is the resolved data root. A relative DataRoot is taken relative to
// the executable's directory, NOT the process CWD: services start with CWD "/"
// (read-only on macOS), so "./data" would resolve to an unwritable "/data".
// Empty stays empty so callers can detect "no data root configured".
func (c Config) DataDir() string {
	if c.DataRoot == "" {
		return ""
	}
	if filepath.IsAbs(c.DataRoot) {
		return filepath.Clean(c.DataRoot)
	}
	return filepath.Join(exeDir(), c.DataRoot)
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
	if dr := cfg.DataDir(); dr != "" {
		// 0755, not 0700: the bridge serves this dir to the CRM and may run
		// as a different account than created it (non-root service user, NFS
		// root-squash). config.json itself stays 0600.
		if err := os.MkdirAll(dr, 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	// Atomic rename over the target: config.json is only ever replaced by a
	// complete, valid file — a crash mid-write leaves the old one intact.
	return utils.AtomicWrite(Path(), b, 0o600)
}
