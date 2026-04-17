package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/selfbase-dev/s2-sync/internal/auth"
	"github.com/selfbase-dev/s2-sync/internal/client"
	"github.com/selfbase-dev/s2-sync/internal/service"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// openInFileManager reveals path in the OS file manager.
func openInFileManager(path string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", path).Start()
	case "windows":
		return exec.Command("explorer", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}

type App struct {
	ctx     context.Context
	svc     *service.SyncService
	endpoint string
}

func NewApp() *App {
	endpoint := os.Getenv("S2_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://scopeds.dev"
	}
	return &App{
		svc:      service.New(endpoint),
		endpoint: endpoint,
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	go a.forwardEvents()
	go a.maybeAutoResume()
}

// maybeAutoResume kicks off sync at launch when both a token and a
// saved folder are present. Keeps the "open the app and it just works"
// expectation; users explicitly Stop if they want to pause.
func (a *App) maybeAutoResume() {
	if !a.HasToken() {
		return
	}
	folder := a.SavedFolder()
	if folder == "" {
		return
	}
	if _, err := os.Stat(folder); err != nil {
		return
	}
	_ = a.svc.Start(a.ctx, service.Mount{Path: folder})
}

// forwardEvents pumps service events out to the frontend via Wails
// runtime. Bound automatically when the window is ready.
func (a *App) forwardEvents() {
	ch := a.svc.Subscribe()
	for ev := range ch {
		wailsruntime.EventsEmit(a.ctx, "sync:event", ev)
	}
}

// --- Bound methods (callable from JS) ---

// HasToken reports whether a token is stored in the system keyring.
func (a *App) HasToken() bool {
	t, err := auth.GetKeyring()
	return err == nil && t != ""
}

// SaveToken validates and stores a token in the system keyring.
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

// ClearToken removes the stored token.
func (a *App) ClearToken() error {
	if err := a.svc.Stop(); err != nil {
		return err
	}
	return auth.DeleteKeyring()
}

// PickFolder shows a native directory chooser.
func (a *App) PickFolder() (string, error) {
	return wailsruntime.OpenDirectoryDialog(a.ctx, wailsruntime.OpenDialogOptions{
		Title: "Select folder to sync",
	})
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

// Endpoint returns the configured S2 endpoint URL.
func (a *App) Endpoint() string {
	return a.endpoint
}

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
