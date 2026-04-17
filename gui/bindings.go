// bindings.go holds the Wails-bound methods on *App that the React
// frontend calls. Each method is a thin adapter over internal/service
// or the Wails runtime — keep non-trivial logic out of here and in the
// service layer so it's testable from Go.

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/selfbase-dev/s2-sync/internal/auth"
	"github.com/selfbase-dev/s2-sync/internal/client"
	"github.com/selfbase-dev/s2-sync/internal/service"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// --- Auth ---

// HasToken reports whether a token is stored in the system keyring.
func (a *App) HasToken() bool {
	t, err := auth.GetKeyring()
	return err == nil && t != ""
}

// SaveToken validates against /api/me and stores a token in the system
// keyring.
func (a *App) SaveToken(token string) error {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, "s2_") {
		return fmt.Errorf("invalid token: must start with s2_")
	}
	c := client.New(a.endpoint, token)
	if _, err := c.Me(); err != nil {
		return fmt.Errorf("token validation failed: %w", err)
	}
	return auth.SetKeyring(token)
}

// ClearToken stops any running sync and removes the stored token.
func (a *App) ClearToken() error {
	if err := a.svc.Stop(); err != nil {
		return err
	}
	return auth.DeleteKeyring()
}

// --- Folder ---

// PickFolder shows a native directory chooser, opening at `current`
// when it's a valid existing directory. CanCreateDirectories enables
// the "New Folder" button so users can create a sync root from the
// dialog.
func (a *App) PickFolder(current string) (string, error) {
	opts := wailsruntime.OpenDialogOptions{
		Title:                "Select folder to sync",
		CanCreateDirectories: true,
	}
	if current != "" {
		if info, err := os.Stat(current); err == nil && info.IsDir() {
			opts.DefaultDirectory = current
		}
	}
	return wailsruntime.OpenDirectoryDialog(a.ctx, opts)
}

// OpenFolder reveals the given path in Finder / Explorer / file manager.
func (a *App) OpenFolder(path string) error {
	if path == "" {
		return nil
	}
	return openInFileManager(path)
}

// EnsureFolder creates the directory if it doesn't exist (used during
// first-run Connect to materialize the default `~/S2` placeholder).
func (a *App) EnsureFolder(path string) error {
	return os.MkdirAll(path, 0o755)
}

// DefaultFolder returns the suggested folder shown as a placeholder
// for new users (see service.DefaultMountPath).
func (a *App) DefaultFolder() string {
	return service.DefaultMountPath()
}

// SavedFolder returns the previously configured folder (or "" if none).
func (a *App) SavedFolder() string {
	c, err := service.LoadConfig()
	if err != nil || c == nil {
		return ""
	}
	return c.MountPath
}

// SetSavedFolder persists the user's chosen sync folder.
func (a *App) SetSavedFolder(path string) error {
	c, err := service.LoadConfig()
	if err != nil {
		c = &service.Config{}
	}
	c.MountPath = path
	return service.SaveConfig(c)
}

// --- Sync ---

// StartSync begins watching the given folder, persisting the choice
// so it auto-loads on next launch.
func (a *App) StartSync(folder string) error {
	if err := a.SetSavedFolder(folder); err != nil {
		return fmt.Errorf("save folder: %w", err)
	}
	return a.svc.Start(a.ctx, service.Mount{Path: folder})
}

// StopSync cancels the running sync.
func (a *App) StopSync() error {
	return a.svc.Stop()
}

// GetStatus returns the current state snapshot.
func (a *App) GetStatus() service.StateInfo {
	return a.svc.Status()
}

// --- Preferences ---

// IsAutostartEnabled reports whether the OS is configured to launch
// s2sync at login.
func (a *App) IsAutostartEnabled() bool {
	return service.IsAutostartEnabled()
}

// SetAutostart turns the OS-level launch-at-login on or off, pointing
// at the currently running executable.
func (a *App) SetAutostart(enabled bool) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	return service.SetAutostart(enabled, exe)
}

// --- Misc ---

// Endpoint returns the configured S2 endpoint URL.
func (a *App) Endpoint() string {
	return a.endpoint
}

// ConfirmDisconnect shows a native Yes/No dialog and reports the choice.
func (a *App) ConfirmDisconnect() (bool, error) {
	res, err := wailsruntime.MessageDialog(a.ctx, wailsruntime.MessageDialogOptions{
		Type:          wailsruntime.QuestionDialog,
		Title:         "Disconnect from S2?",
		Message:       "Stop syncing and remove the saved token from this device. Your files are not deleted.",
		Buttons:       []string{"Disconnect", "Cancel"},
		DefaultButton: "Cancel",
		CancelButton:  "Cancel",
	})
	if err != nil {
		return false, err
	}
	return res == "Disconnect", nil
}
