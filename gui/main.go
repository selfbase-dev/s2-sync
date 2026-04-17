package main

import (
	"context"
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app := NewApp()
	// systray.RunWithExternalLoop's nativeStart creates the
	// NSStatusItem and installs its own SystrayAppDelegate — do it on
	// main before wails.Run so NSApp sharedApplication is set up once
	// and the AppKit calls run on the right thread.
	setupTray(app)
	startTray()

	err := wails.Run(&options.App{
		Title:             "s2sync",
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
		BackgroundColour: &options.RGBA{R: 255, G: 255, B: 255, A: 1},
		OnStartup:        app.startup,
		OnShutdown: func(_ context.Context) {
			stopTray()
		},
		Bind: []interface{}{
			app,
		},
	})
	if err != nil {
		println("wails error:", err.Error())
	}
}
