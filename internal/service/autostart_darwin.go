package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const launchAgentLabel = "dev.selfbase.s2sync"

func launchAgentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"), nil
}

// IsAutostartEnabled reports whether a launch agent plist is in place.
func IsAutostartEnabled() bool {
	path, err := launchAgentPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// SetAutostart installs or removes the launch agent plist for the given
// executable path. exePath should point to the Contents/MacOS binary
// inside the .app bundle (or any other concrete binary path).
func SetAutostart(enabled bool, exePath string) error {
	path, err := launchAgentPath()
	if err != nil {
		return err
	}
	if !enabled {
		_ = exec.Command("launchctl", "unload", path).Run()
		return removeIfExists(path)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>ProcessType</key>
    <string>Interactive</string>
</dict>
</plist>
`, launchAgentLabel, exePath)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}
	// Best-effort load. Ignore error (it may already be loaded).
	_ = exec.Command("launchctl", "load", path).Run()
	return nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
