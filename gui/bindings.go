// bindings.go holds the Wails-bound methods on *App that the React
// frontend calls. Each method is a thin adapter over internal/service
// or the Wails runtime — keep non-trivial logic out of here and in the
// service layer so it's testable from Go.

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/selfbase-dev/s2-sync/internal/auth"
	"github.com/selfbase-dev/s2-sync/internal/client"
	"github.com/selfbase-dev/s2-sync/internal/installation"
	"github.com/selfbase-dev/s2-sync/internal/oauth"
	"github.com/selfbase-dev/s2-sync/internal/service"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// --- Auth ---

// HasValidSession reports whether a structured OAuth session is stored
// in the system keyring. It does not contact the server — the dashboard
// will surface a real auth failure on the next API call.
func (a *App) HasValidSession() bool {
	return auth.HasValidSession()
}

// StartOAuthLogin runs the full Authorization Code + PKCE + loopback
// flow: opens the system browser, waits for the redirect on a private
// loopback port, exchanges the code at /oauth/token, and persists the
// resulting session. Blocks until the user completes consent, cancels
// in the browser, the 5-minute default timeout fires, or
// CancelOAuthLogin is called from the UI (e.g. user closed the browser
// tab and wants to retry without waiting for the timeout).
func (a *App) StartOAuthLogin() error {
	a.loginMu.Lock()
	if a.loginCancel != nil {
		a.loginCancel()
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.loginCancel = cancel
	a.loginMu.Unlock()
	defer func() {
		a.loginMu.Lock()
		if a.loginCancel != nil {
			a.loginCancel = nil
		}
		a.loginMu.Unlock()
		cancel()
	}()

	inst, err := installation.LoadOrCreate()
	if err != nil {
		return fmt.Errorf("load installation: %w", err)
	}
	tr, err := oauth.Login(ctx, a.endpoint, oauth.LoginOpts{
		InstallationID: inst.InstallationID,
		DeviceLabel:    inst.DeviceLabel,
	})
	if err != nil {
		return err
	}
	if err := auth.SaveSession(a.endpoint, tr); err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	// Sanity-check the new credentials before reporting success.
	source, err := auth.NewSource(a.endpoint)
	if err != nil {
		return err
	}
	if _, err := client.New(a.endpoint, source).Me(); err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	return nil
}

// CancelOAuthLogin aborts an in-flight StartOAuthLogin. No-op if no
// login is running. The pending StartOAuthLogin call returns
// context.Canceled so the UI can re-enable the Sign-in button without
// waiting for the 5-minute timeout.
func (a *App) CancelOAuthLogin() {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	if a.loginCancel != nil {
		a.loginCancel()
		a.loginCancel = nil
	}
}

// SignOut stops any running sync and removes the stored session.
func (a *App) SignOut() error {
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
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("could not create folder %q: %w", path, err)
	}
	return nil
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

// --- Logs ---

// LogFile returns the absolute path to the JSON Lines log file. Empty
// if the file sink failed to open at startup.
func (a *App) LogFile() string {
	return a.logFile
}

// OpenLogFile reveals the log file in the OS file manager.
func (a *App) OpenLogFile() error {
	if a.logFile == "" {
		return nil
	}
	return openInFileManager(a.logFile)
}

// RecentLogs returns the last n records from the log file as raw JSON
// lines (most recent last). The frontend uses this to repopulate the
// Logs panel after a reload so trouble-shooting context survives across
// app restarts.
func (a *App) RecentLogs(n int) ([]string, error) {
	if a.logFile == "" || n <= 0 {
		return nil, nil
	}
	return tailFile(a.logFile, n)
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
		Title:         "Sign out of S2?",
		Message:       "Stop syncing and remove the saved session from this device. Your files are not deleted.",
		Buttons:       []string{"Sign out", "Cancel"},
		DefaultButton: "Cancel",
		CancelButton:  "Cancel",
	})
	if err != nil {
		return false, err
	}
	return res == "Sign out", nil
}
