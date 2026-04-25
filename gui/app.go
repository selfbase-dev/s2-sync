package main

import (
	"context"
	"log/slog"
	"os"
	"sync"

	slog2 "github.com/selfbase-dev/s2-sync/internal/log"
	"github.com/selfbase-dev/s2-sync/internal/log/sink"
	"github.com/selfbase-dev/s2-sync/internal/service"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// App wires the desktop sync service to the Wails frontend. It owns the
// Wails context captured during startup and exposes bound methods in
// bindings.go for the React UI to call.
type App struct {
	ctx       context.Context
	svc       *service.SyncService
	endpoint  string
	logFile   string
	fileSink  *sink.File
	logger    *slog.Logger

	loginMu     sync.Mutex
	loginCancel context.CancelFunc
}

// NewApp constructs the App with an endpoint read from the S2_ENDPOINT
// env var, defaulting to the production URL. Builds the logger stack
// (file + Wails sinks); the Wails sink defers EventsEmit until startup
// has captured ctx.
func NewApp() *App {
	endpoint := os.Getenv("S2_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://scopeds.dev"
	}
	a := &App{
		endpoint: endpoint,
		logFile:  slog2.DefaultLogPath(),
	}
	fh, err := sink.NewFile(a.logFile, slog.LevelInfo, 0)
	if err == nil {
		a.fileSink = fh
	}
	cb := sink.NewCallback(slog.LevelInfo, func(r sink.Record) {
		if a.ctx == nil {
			return
		}
		wailsruntime.EventsEmit(a.ctx, "log", r)
	})
	handlers := []slog.Handler{cb}
	if fh != nil {
		handlers = append(handlers, fh)
	}
	a.logger = slog.New(slog2.Multi(handlers...))
	a.svc = service.New(endpoint, a.logger)
	return a
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	go a.maybeAutoResume()
	makeAccessoryApp()
}

// shutdown is invoked from main.OnShutdown so the file sink fd is
// flushed and released cleanly.
func (a *App) shutdown(_ context.Context) {
	if a.fileSink != nil {
		_ = a.fileSink.Close()
	}
}

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
