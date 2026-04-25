package main

import (
	"context"
	"os"

	"github.com/selfbase-dev/s2-sync/internal/service"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// App wires the desktop sync service to the Wails frontend. It owns the
// Wails context captured during startup and exposes bound methods in
// bindings.go for the React UI to call.
type App struct {
	ctx      context.Context
	svc      *service.SyncService
	endpoint string
}

// NewApp constructs the App with an endpoint read from the S2_ENDPOINT
// env var, defaulting to the production URL.
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

// startup is invoked by Wails after NSApplication is ready. It captures
// the Wails ctx, pumps service events to the frontend, auto-resumes the
// previously configured sync, and flips the activation policy to
// Accessory (Wails sets Regular, which overrides Info.plist).
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	go a.forwardEvents()
	go a.maybeAutoResume()
	makeAccessoryApp()
}

// forwardEvents pumps service events out to the frontend via Wails
// runtime. Runs for the lifetime of the app.
func (a *App) forwardEvents() {
	ch := a.svc.Subscribe()
	for ev := range ch {
		wailsruntime.EventsEmit(a.ctx, "sync:event", ev)
	}
}

// maybeAutoResume kicks off sync at launch when both a token and a
// saved folder are present. Keeps the "open the app and it just works"
// expectation; users explicitly Stop if they want to pause.
func (a *App) maybeAutoResume() {
	if !a.HasValidSession() {
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
