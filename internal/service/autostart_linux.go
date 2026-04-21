package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const linuxDesktopFilename = "s2sync.desktop"

// autostartDesktopPath honors $XDG_CONFIG_HOME only when it is an
// absolute path, per the XDG Base Directory Specification. Relative or
// empty values fall back to ~/.config.
func autostartDesktopPath() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" && filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "autostart", linuxDesktopFilename), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "autostart", linuxDesktopFilename), nil
}

// escapeDesktopExecArg quotes a value for the Exec= field of a .desktop
// file per the Desktop Entry Specification. Inside double quotes, the
// spec requires backslash-escaping of `\`, `"`, backtick, and `$`. A
// literal `%` must be doubled because `%` introduces field codes in the
// Exec field. Escaping `\` first preserves the other escapes.
func escapeDesktopExecArg(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "`", "\\`")
	s = strings.ReplaceAll(s, `$`, `\$`)
	s = strings.ReplaceAll(s, `%`, `%%`)
	return `"` + s + `"`
}

// IsAutostartEnabled reports whether the autostart .desktop file is in place.
func IsAutostartEnabled() bool {
	path, err := autostartDesktopPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// SetAutostart writes or removes the XDG autostart .desktop file for the
// given executable path. File presence is the source of truth — see the
// freedesktop.org Autostart spec.
func SetAutostart(enabled bool, exePath string) error {
	path, err := autostartDesktopPath()
	if err != nil {
		return err
	}
	if !enabled {
		return removeIfExists(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	content := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=s2sync
Exec=%s
X-GNOME-Autostart-enabled=true
`, escapeDesktopExecArg(exePath))
	return os.WriteFile(path, []byte(content), 0o644)
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
