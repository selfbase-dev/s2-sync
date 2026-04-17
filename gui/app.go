package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/selfbase-dev/s2-sync/internal/auth"
	"github.com/selfbase-dev/s2-sync/internal/client"
	"github.com/selfbase-dev/s2-sync/internal/service"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

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

// StartSync begins watching the given folder.
func (a *App) StartSync(folder string) error {
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
