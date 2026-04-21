package service

import (
	"fmt"

	"golang.org/x/sys/windows/registry"
)

const autostartRegistryPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// autostartRegistryValueName is the Run-key value the app writes to. Tests
// override it to avoid touching the real autostart entry on a developer
// machine or CI runner.
var autostartRegistryValueName = "s2sync"

// IsAutostartEnabled reports whether a Run-key value for s2sync exists.
func IsAutostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartRegistryPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(autostartRegistryValueName)
	return err == nil
}

// SetAutostart writes or removes the Run-key value for the given
// executable path. The value is quoted so paths containing spaces
// (e.g. "C:\Program Files\s2sync\s2sync.exe") launch correctly.
func SetAutostart(enabled bool, exePath string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, autostartRegistryPath, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return fmt.Errorf("open Run key: %w", err)
	}
	defer k.Close()
	if !enabled {
		if err := k.DeleteValue(autostartRegistryValueName); err != nil && err != registry.ErrNotExist {
			return fmt.Errorf("delete Run value: %w", err)
		}
		return nil
	}
	value := fmt.Sprintf(`"%s"`, exePath)
	if err := k.SetStringValue(autostartRegistryValueName, value); err != nil {
		return fmt.Errorf("set Run value: %w", err)
	}
	return nil
}
