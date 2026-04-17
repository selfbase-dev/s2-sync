package main

import (
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// wails.Run must own the main thread on macOS — both because it
	// blocks the binding-generation introspection step and because
	// AppKit lives there. Tray runs in a goroutine launched from
	// OnStartup, after Wails has initialized NSApplication.
	app := NewApp()
	err := wails.Run(&options.App{
		Title:             "s2sync",
		Width:             520,
		Height:            640,
		MinWidth:          420,
		MinHeight:         520,
		HideWindowOnClose: true,
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
		Bind: []interface{}{
			app,
		},
	})
	if err != nil {
		println("wails error:", err.Error())
	}
}
