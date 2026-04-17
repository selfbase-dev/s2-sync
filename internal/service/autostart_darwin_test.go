package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppBundlePathFromContentsMacOS(t *testing.T) {
	got := appBundlePath("/Applications/Foo.app/Contents/MacOS/foo")
	if got != "/Applications/Foo.app" {
		t.Fatalf("want /Applications/Foo.app, got %q", got)
	}
}

func TestAppBundlePathBareBinary(t *testing.T) {
	if got := appBundlePath("/usr/local/bin/foo"); got != "" {
		t.Fatalf("want empty for non-bundle path, got %q", got)
	}
}

func TestAppBundlePathPartialMatch(t *testing.T) {
	// MacOS dir without a Contents parent is not a bundle.
	if got := appBundlePath("/tmp/MacOS/foo"); got != "" {
		t.Fatalf("want empty for /tmp/MacOS/foo, got %q", got)
	}
}

func TestSetAutostartEnableWritesPlist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	exe := filepath.Join(home, "fake-bin")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("create fake exe: %v", err)
	}

	if err := SetAutostart(true, exe); err != nil {
		t.Fatalf("SetAutostart: %v", err)
	}

	plistPath := filepath.Join(home, "Library", "LaunchAgents", "dev.selfbase.s2sync.plist")
	body, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	if !strings.Contains(string(body), "<key>RunAtLoad</key>") {
		t.Errorf("plist missing RunAtLoad: %s", body)
	}
	if !strings.Contains(string(body), "dev.selfbase.s2sync") {
		t.Errorf("plist missing label: %s", body)
	}
	if !strings.Contains(string(body), exe) {
		t.Errorf("plist missing exe path %q: %s", exe, body)
	}
	if !IsAutostartEnabled() {
		t.Error("IsAutostartEnabled should be true after SetAutostart(true)")
	}
}

func TestSetAutostartUsesOpenForAppBundle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Construct a fake .app bundle with a Contents/MacOS binary.
	bundle := filepath.Join(home, "S2.app")
	macosDir := filepath.Join(bundle, "Contents", "MacOS")
	if err := os.MkdirAll(macosDir, 0o755); err != nil {
		t.Fatalf("mkdir bundle: %v", err)
	}
	exe := filepath.Join(macosDir, "s2sync")
	if err := os.WriteFile(exe, nil, 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}

	if err := SetAutostart(true, exe); err != nil {
		t.Fatalf("SetAutostart: %v", err)
	}

	plistPath := filepath.Join(home, "Library", "LaunchAgents", "dev.selfbase.s2sync.plist")
	body, _ := os.ReadFile(plistPath)
	if !strings.Contains(string(body), "/usr/bin/open") {
		t.Errorf("expected `open` invocation for bundled exe; got %s", body)
	}
	if !strings.Contains(string(body), bundle) {
		t.Errorf("expected bundle path %q in plist; got %s", bundle, body)
	}
}

func TestSetAutostartDisableRemovesPlist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	exe := filepath.Join(home, "fake")
	_ = os.WriteFile(exe, nil, 0o755)
	if err := SetAutostart(true, exe); err != nil {
		t.Fatalf("enable: %v", err)
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "dev.selfbase.s2sync.plist")
	if _, err := os.Stat(plistPath); err != nil {
		t.Fatalf("plist not present after enable: %v", err)
	}

	if err := SetAutostart(false, exe); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Errorf("plist should be removed after disable, stat err: %v", err)
	}
	if IsAutostartEnabled() {
		t.Error("IsAutostartEnabled should be false after disable")
	}
}

func TestSetAutostartDisableNoopWhenAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := SetAutostart(false, ""); err != nil {
		t.Errorf("disable on absent plist should be no-op, got %v", err)
	}
}
