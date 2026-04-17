package service

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Config holds GUI-side preferences that persist across launches.
// Per-mount sync state lives inside the watched folder (.s2/state.db)
// per ADR 0047; this config only tracks user choices like which
// folder is currently configured.
type Config struct {
	MountPath string `json:"mountPath,omitempty"`
}

// ConfigPath returns the per-OS config file location.
func ConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "s2sync", "config.json"), nil
}

// LoadConfig reads the config file. Returns an empty Config when the
// file is absent.
func LoadConfig() (*Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// SaveConfig writes the config atomically.
func SaveConfig(c *Config) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// DefaultMountPath returns a sensible suggested folder (`~/S2`) the GUI
// can show as a placeholder. The directory is not created.
func DefaultMountPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "S2")
}
