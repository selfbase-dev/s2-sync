package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxSetAutostartEnableWritesDesktop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	exe := filepath.Join(home, "s2sync")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("create fake exe: %v", err)
	}

	if err := SetAutostart(true, exe); err != nil {
		t.Fatalf("SetAutostart: %v", err)
	}

	desktopPath := filepath.Join(home, ".config", "autostart", "s2sync.desktop")
	body, err := os.ReadFile(desktopPath)
	if err != nil {
		t.Fatalf("read desktop file: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"[Desktop Entry]",
		"Type=Application",
		"Name=s2sync",
		`Exec="` + exe + `"`,
		"X-GNOME-Autostart-enabled=true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("desktop file missing %q:\n%s", want, got)
		}
	}
	if !IsAutostartEnabled() {
		t.Error("IsAutostartEnabled should be true after SetAutostart(true)")
	}
}

func TestLinuxSetAutostartUsesXDGConfigHome(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)

	if err := SetAutostart(true, "/usr/local/bin/s2sync"); err != nil {
		t.Fatalf("SetAutostart: %v", err)
	}

	desktopPath := filepath.Join(xdg, "autostart", "s2sync.desktop")
	if _, err := os.Stat(desktopPath); err != nil {
		t.Fatalf("desktop file should be under XDG_CONFIG_HOME: %v", err)
	}
	fallback := filepath.Join(home, ".config", "autostart", "s2sync.desktop")
	if _, err := os.Stat(fallback); !os.IsNotExist(err) {
		t.Errorf("fallback path should not be used when XDG_CONFIG_HOME is set: %v", err)
	}
}

func TestLinuxSetAutostartDisableRemovesDesktop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	exe := filepath.Join(home, "s2sync")
	_ = os.WriteFile(exe, nil, 0o755)
	if err := SetAutostart(true, exe); err != nil {
		t.Fatalf("enable: %v", err)
	}
	desktopPath := filepath.Join(home, ".config", "autostart", "s2sync.desktop")
	if _, err := os.Stat(desktopPath); err != nil {
		t.Fatalf("desktop not present after enable: %v", err)
	}

	if err := SetAutostart(false, exe); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := os.Stat(desktopPath); !os.IsNotExist(err) {
		t.Errorf("desktop should be removed after disable, stat err: %v", err)
	}
	if IsAutostartEnabled() {
		t.Error("IsAutostartEnabled should be false after disable")
	}
}

func TestLinuxSetAutostartDisableNoopWhenAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	if err := SetAutostart(false, ""); err != nil {
		t.Errorf("disable on absent desktop should be no-op, got %v", err)
	}
}

func TestLinuxSetAutostartIgnoresRelativeXDGConfigHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Per XDG spec, relative paths in XDG_CONFIG_HOME are invalid and
	// must be ignored. We must fall back to ~/.config rather than
	// writing the .desktop file under cwd.
	t.Setenv("XDG_CONFIG_HOME", "relative/path")

	if err := SetAutostart(true, "/usr/local/bin/s2sync"); err != nil {
		t.Fatalf("SetAutostart: %v", err)
	}

	fallback := filepath.Join(home, ".config", "autostart", "s2sync.desktop")
	if _, err := os.Stat(fallback); err != nil {
		t.Errorf("expected fallback to ~/.config when XDG_CONFIG_HOME is relative: %v", err)
	}
}

func TestLinuxSetAutostartEscapesExecSpecialChars(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	// A path with a space and a literal % triggers two distinct escape
	// rules: spaces force the quoted form, and % must be doubled so the
	// .desktop parser does not treat it as a field-code prefix.
	exe := filepath.Join(home, "S2 Sync", "s2sync 100%")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(exe, nil, 0o755); err != nil {
		t.Fatalf("write fake exe: %v", err)
	}

	if err := SetAutostart(true, exe); err != nil {
		t.Fatalf("SetAutostart: %v", err)
	}

	desktopPath := filepath.Join(home, ".config", "autostart", "s2sync.desktop")
	body, err := os.ReadFile(desktopPath)
	if err != nil {
		t.Fatalf("read desktop: %v", err)
	}
	// Expect the Exec line to be double-quoted with the raw path inside
	// (spaces preserved) and % doubled to %%.
	wantLine := `Exec="` + strings.ReplaceAll(exe, "%", "%%") + `"`
	if !strings.Contains(string(body), wantLine) {
		t.Errorf("expected Exec line %q in:\n%s", wantLine, body)
	}
}
