package service

import (
	"os"
	"path/filepath"
	"testing"
)

// withConfigDir redirects os.UserConfigDir() (which ConfigPath consults
// via the XDG / Apple-standard fallback) at a tmp directory for the
// duration of the test. On Windows, os.UserConfigDir reads %APPDATA%;
// os.UserHomeDir reads %USERPROFILE%. Set all three so tests are stable
// across platforms.
func withConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir) // Linux
	t.Setenv("HOME", dir)            // macOS + Linux fallback for UserHomeDir
	t.Setenv("APPDATA", dir)         // Windows UserConfigDir source
	t.Setenv("USERPROFILE", dir)     // Windows UserHomeDir source
	return dir
}

func TestLoadConfigMissingReturnsEmpty(t *testing.T) {
	withConfigDir(t)
	c, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c == nil || c.MountPath != "" {
		t.Fatalf("want empty config, got %+v", c)
	}
}

func TestSaveThenLoadRoundTrip(t *testing.T) {
	withConfigDir(t)
	if err := SaveConfig(&Config{MountPath: "/Users/alice/S2"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	got, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.MountPath != "/Users/alice/S2" {
		t.Fatalf("want /Users/alice/S2, got %q", got.MountPath)
	}
}

func TestSaveCreatesParentDir(t *testing.T) {
	withConfigDir(t)
	// Verify the parent directory is auto-created by Save (no MkdirAll
	// from the caller required).
	if err := SaveConfig(&Config{MountPath: "/x"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	path, err := ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("config dir not created: %v", err)
	}
}

func TestSaveAtomic(t *testing.T) {
	withConfigDir(t)
	// First write should succeed and leave no .tmp behind.
	if err := SaveConfig(&Config{MountPath: "/v1"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	path, _ := ConfigPath()
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("tmp file should not remain after rename: %v", err)
	}
	// Second write replaces atomically.
	if err := SaveConfig(&Config{MountPath: "/v2"}); err != nil {
		t.Fatalf("SaveConfig second: %v", err)
	}
	got, _ := LoadConfig()
	if got.MountPath != "/v2" {
		t.Fatalf("want /v2, got %q", got.MountPath)
	}
}

func TestDefaultMountPathInsideHome(t *testing.T) {
	home := t.TempDir()
	// os.UserHomeDir consults HOME on unix and USERPROFILE on Windows.
	// Set both so the test is portable across CI runners.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	got := DefaultMountPath()
	want := filepath.Join(home, "S2")
	if got != want {
		t.Fatalf("DefaultMountPath want %q got %q", want, got)
	}
}
