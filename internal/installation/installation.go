// Package installation manages the per-install identity that s2-sync sends
// to /oauth/authorize as `installation_id`. Multiple machines running
// s2-sync share the same OAuth client_id ("s2-sync-desktop", hardcoded by
// ADR 0056), so without a per-install identifier the server would collapse
// them into a single grant and re-logins on a new machine would invalidate
// the others' refresh tokens.
//
// Identity layer:
//   - installation_id: stable UUID v4 generated once on first launch and
//     persisted at $XDG_CONFIG_HOME/s2-sync/installation.json (or the OS
//     equivalent). Used by the server as part of the grant uniqueness key.
//     Treat as identity, not display.
//   - device_label: human-friendly label, hostname by default, mutable.
//     Display only; never used as identity.
package installation

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// FileName is the on-disk filename within the s2-sync config directory.
const FileName = "installation.json"

// Info is the persisted shape on disk.
type Info struct {
	Version       int    `json:"version"`
	InstallationID string `json:"installation_id"`
	DeviceLabel    string `json:"device_label"`
}

const schemaVersion = 1

// LoadOrCreate returns the installation info, generating + persisting a
// fresh UUID v4 on first use. Path is computed via os.UserConfigDir() so
// it matches XDG on Linux, Application Support on macOS, AppData on
// Windows.
func LoadOrCreate() (*Info, error) {
	path, err := defaultPath()
	if err != nil {
		return nil, err
	}
	if existing, err := load(path); err == nil {
		return existing, nil
	}

	id, err := newUUIDv4()
	if err != nil {
		return nil, err
	}
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "s2-sync"
	}
	info := &Info{
		Version:        schemaVersion,
		InstallationID: id,
		DeviceLabel:    hostname,
	}
	if err := save(path, info); err != nil {
		return nil, err
	}
	return info, nil
}

func defaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "s2-sync", FileName), nil
}

func load(path string) (*Info, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var info Info
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, err
	}
	if info.Version != schemaVersion || info.InstallationID == "" {
		return nil, fmt.Errorf("invalid installation.json")
	}
	return &info, nil
}

func save(path string, info *Info) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// newUUIDv4 generates an RFC 4122 §4.4 random UUID without pulling in a
// dependency. The output is canonical lowercase 8-4-4-4-12 hex.
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	hexs := hex.EncodeToString(b[:])
	return fmt.Sprintf(
		"%s-%s-%s-%s-%s",
		hexs[0:8], hexs[8:12], hexs[12:16], hexs[16:20], hexs[20:32],
	), nil
}
