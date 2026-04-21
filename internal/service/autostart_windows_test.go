package service

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// swapTestValueName points autostart writes to a throwaway Run-key value
// so tests never collide with a real s2sync autostart entry. Cleanup
// removes the value (and is idempotent).
func swapTestValueName(t *testing.T) {
	t.Helper()
	orig := autostartRegistryValueName
	autostartRegistryValueName = "s2sync-test-" + t.Name()
	t.Cleanup(func() {
		k, err := registry.OpenKey(registry.CURRENT_USER, autostartRegistryPath, registry.SET_VALUE)
		if err == nil {
			_ = k.DeleteValue(autostartRegistryValueName)
			_ = k.Close()
		}
		autostartRegistryValueName = orig
	})
}

func TestWindowsSetAutostartEnableWritesRegistry(t *testing.T) {
	swapTestValueName(t)
	exe := `C:\Program Files\s2sync\s2sync.exe`

	if err := SetAutostart(true, exe); err != nil {
		t.Fatalf("SetAutostart: %v", err)
	}

	k, err := registry.OpenKey(registry.CURRENT_USER, autostartRegistryPath, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf("open Run key: %v", err)
	}
	defer k.Close()
	got, _, err := k.GetStringValue(autostartRegistryValueName)
	if err != nil {
		t.Fatalf("get Run value: %v", err)
	}
	if !strings.Contains(got, exe) {
		t.Errorf("Run value %q missing exe path %q", got, exe)
	}
	if !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
		t.Errorf("Run value should be quoted, got %q", got)
	}
	if !IsAutostartEnabled() {
		t.Error("IsAutostartEnabled should be true after SetAutostart(true)")
	}
}

func TestWindowsSetAutostartDisableRemovesRegistryValue(t *testing.T) {
	swapTestValueName(t)
	exe := `C:\s2sync.exe`
	if err := SetAutostart(true, exe); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !IsAutostartEnabled() {
		t.Fatal("expected enabled after SetAutostart(true)")
	}

	if err := SetAutostart(false, exe); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if IsAutostartEnabled() {
		t.Error("IsAutostartEnabled should be false after disable")
	}
}

func TestWindowsSetAutostartDisableNoopWhenAbsent(t *testing.T) {
	swapTestValueName(t)
	if err := SetAutostart(false, ""); err != nil {
		t.Errorf("disable on absent value should be no-op, got %v", err)
	}
}
