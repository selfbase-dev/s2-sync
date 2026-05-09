package main

import (
	"context"
	"embed"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	winopts "github.com/wailsapp/wails/v2/pkg/options/windows"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app := NewApp()
	// systray.RunWithExternalLoop creates a small owner object and
	// calls applicationDidFinishLaunching on it to install the
	// NSStatusItem. It does NOT set itself as NSApp's delegate, so
	// Wails' own delegate takes over cleanly once wails.Run runs.
	// nativeStart must be called on the main thread — invoking it here
	// from main (before wails.Run) satisfies that without needing a
	// dispatch hop.
	setupTray(app)
	startTray()

	err := wails.Run(&options.App{
		Title:             "S2 Sync",
		Width:             520,
		Height:            640,
		MinWidth:          420,
		MinHeight:         520,
		HideWindowOnClose: true,
		// Menu-bar-only app: LSUIElement=true hides the Dock icon, and
		// the window stays hidden until the user chooses "Show window"
		// from the tray.
		StartHidden: true,
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "dev.selfbase.s2sync",
			OnSecondInstanceLaunch: func(_ options.SecondInstanceData) {
				if app.ctx != nil {
					wailsruntime.WindowShow(app.ctx)
				}
			},
		},
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		Windows: webviewOptions(),
		BackgroundColour: &options.RGBA{R: 255, G: 255, B: 255, A: 1},
		OnStartup:        app.startup,
		OnShutdown: func(ctx context.Context) {
			// Graceful shutdown: signal sync to stop, wait for the
			// run goroutine to drain so state.Close happens before the
			// process exits. Then tear down the tray and close the log
			// file fd.
			_ = app.svc.Stop()
			app.svc.Wait()
			stopTray()
			app.shutdown(ctx)
		},
		Bind: []interface{}{
			app,
		},
	})
	if err != nil {
		println("wails error:", err.Error())
	}
}

// webviewOptions returns Windows-specific options. On non-Windows builds
// the winopts import is still resolved but the field is ignored at
// runtime, so this compiles cross-platform.
func webviewOptions() *winopts.Options {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil
	}
	return &winopts.Options{
		// Keep all app data under one folder (%APPDATA%\s2sync\…) so
		// users know exactly what to remove when resetting. The Wails
		// default would create a separate %APPDATA%\s2sync.exe folder.
		WebviewUserDataPath: filepath.Join(dir, "s2sync", "webview2"),
	}
}
