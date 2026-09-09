// Package config owns this daemon persistent identity: a stable device UUID, a
// human-readable name, and the creation timestamp. The identity is written once
// to the user config directory and reused on every subsequent start so that
// paired peers keep recognising this machine across restarts.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/google/uuid"

	"github.com/zeroaxiis/sharing/daemon/protocol"
)

// Version is the daemon build version reported over HTTP and WebSocket.
const Version = "0.1.0"

// AppDir is the per-user configuration directory name, created inside
// os.UserConfigDir().
const AppDir = "sharing"

// FileName is the config file name inside AppDir.
const FileName = "config.json"

// File permissions. The identity is not a secret in the cryptographic sense
// yet, but it uniquely fingerprints the machine, so keep it owner-only from the
// start rather than tightening it later.
const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// Config is the on-disk device identity. Field names are the exact JSON keys
// written to config.json.
type Config struct {
	DeviceID  string    `json:"deviceId"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
}

// Platform maps runtime.GOOS onto the Platform union shared with the extension.
// Anything outside the three supported operating systems collapses to
// "unknown" rather than leaking a raw GOOS value the TypeScript side cannot type.
func Platform() string {
	switch runtime.GOOS {
	case "windows":
		return protocol.PlatformWindows
	case "darwin":
		return protocol.PlatformDarwin
	case "linux":
		return protocol.PlatformLinux
	default:
		return protocol.PlatformUnknown
	}
}

// Dir returns the directory holding config.json.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(base, AppDir), nil
}

// Path returns the absolute path of config.json.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// Load reads the persisted identity, creating it on first run.
//
// A missing or corrupt file is not fatal: a fresh identity is generated and
// written, and the event is logged at warn level. Losing the identity only
// costs the user their existing pairings, so refusing to start would be a worse
// trade than silently re-provisioning.
//
// nameOverride, when non-empty (the --name flag), replaces the stored name and
// is persisted so the choice survives the next restart.
func Load(logger *slog.Logger, nameOverride string) (*Config, error) {
	if logger == nil {
		logger = slog.Default()
	}

	path, err := Path()
	if err != nil {
		return nil, err
	}

	cfg, err := read(path)
	switch {
	case err == nil:
		// Loaded cleanly.
	case errors.Is(err, os.ErrNotExist):
		logger.Info("no config found, generating device identity", "path", path)
		cfg = nil
	default:
		logger.Warn("config unreadable, regenerating device identity", "path", path, "error", err)
		cfg = nil
	}

	dirty := false
	if cfg == nil {
		cfg = &Config{CreatedAt: time.Now().UTC().Truncate(time.Second)}
		dirty = true
	}

	if cfg.DeviceID == "" || uuid.Validate(cfg.DeviceID) != nil {
		if cfg.DeviceID != "" {
			logger.Warn("stored deviceId is not a valid UUID, regenerating", "deviceId", cfg.DeviceID)
		}
		cfg.DeviceID = uuid.NewString()
		dirty = true
	}

	if nameOverride != "" && nameOverride != cfg.Name {
		cfg.Name = nameOverride
		dirty = true
	}
	if cfg.Name == "" {
		cfg.Name = defaultName(logger)
		dirty = true
	}
	if cfg.CreatedAt.IsZero() {
		cfg.CreatedAt = time.Now().UTC().Truncate(time.Second)
		dirty = true
	}

	if dirty {
		if err := cfg.Save(); err != nil {
			return nil, err
		}
		logger.Info("wrote device identity", "path", path, "deviceId", cfg.DeviceID)
	}

	return cfg, nil
}

// Save writes the config atomically, creating the directory if needed.
func (c *Config) Save() error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("create config dir %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')

	path := filepath.Join(dir, FileName)
	tmp := path + ".tmp"

	// Write to a temp file then rename, so a crash mid-write cannot leave a
	// truncated config behind.
	if err := os.WriteFile(tmp, data, filePerm); err != nil {
		return fmt.Errorf("write config %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// Best effort cleanup; the rename error is the one worth reporting.
		if rmErr := os.Remove(tmp); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return fmt.Errorf("rename config into place: %w (and cleanup failed: %v)", err, rmErr)
		}
		return fmt.Errorf("rename config into place: %w", err)
	}

	// os.WriteFile only applies the mode when it creates the file, so re-assert
	// it for the case where a pre-existing temp file was reused.
	if err := os.Chmod(path, filePerm); err != nil {
		return fmt.Errorf("chmod config %s: %w", path, err)
	}
	return nil
}

// DaemonInfo renders the identity as the GET /info payload.
func (c *Config) DaemonInfo() protocol.DaemonInfo {
	return protocol.DaemonInfo{
		ID:              c.DeviceID,
		Name:            c.Name,
		Version:         Version,
		Platform:        Platform(),
		ProtocolVersion: protocol.ProtocolVersion,
		Capabilities:    []string{"text"},
	}
}

// SelfDevice renders the identity as embedded in a WebSocket ready message.
func (c *Config) SelfDevice() protocol.SelfDevice {
	return protocol.SelfDevice{
		ID:       c.DeviceID,
		Name:     c.Name,
		Version:  Version,
		Platform: Platform(),
	}
}

func read(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		// Pass os.ErrNotExist through unwrapped-enough for errors.Is.
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return &cfg, nil
}

func defaultName(logger *slog.Logger) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		logger.Warn("hostname unavailable, falling back to a generic device name", "error", err)
		return "Sharing Device"
	}
	return host
}
