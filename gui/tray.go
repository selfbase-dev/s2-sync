package main

import (
	"os"

	"fyne.io/systray"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/selfbase-dev/s2-sync/internal/service"
)

// runTray launches the system tray icon. Must be called from the main
// goroutine on macOS (systray uses NSStatusItem). Wails runs in a
// separate goroutine and shares the same NSApplication.
func runTray(app *App) {
	systray.Run(func() { onTrayReady(app) }, func() {})
}

func onTrayReady(app *App) {
	systray.SetTitle("S2")
	systray.SetTooltip("s2sync")

	mShow := systray.AddMenuItem("Show window", "Open the s2sync window")
	mStatus := systray.AddMenuItem("Status: idle", "Current sync status")
	mStatus.Disable()
	systray.AddSeparator()
	mStart := systray.AddMenuItem("Start sync", "Resume syncing")
	mStop := systray.AddMenuItem("Stop sync", "Pause syncing")
	mStop.Hide()
	systray.AddSeparator()
	mAutostart := systray.AddMenuItemCheckbox("Start at login", "Launch s2sync automatically when you log in", service.IsAutostartEnabled())
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit s2sync", "Exit")

	go func() {
		ch := app.svc.Subscribe()
		for ev := range ch {
			updateTrayFromEvent(app, mStatus, mStart, mStop, ev)
		}
	}()

	for {
		select {
		case <-mShow.ClickedCh:
			if app.ctx != nil {
				wailsruntime.WindowShow(app.ctx)
			}
		case <-mStart.ClickedCh:
			if app.ctx != nil {
				st := app.svc.Status()
				if st.Mount != nil {
					_ = app.svc.Start(app.ctx, *st.Mount)
				} else {
					wailsruntime.WindowShow(app.ctx)
				}
			}
		case <-mStop.ClickedCh:
			_ = app.svc.Stop()
		case <-mAutostart.ClickedCh:
			toggleAutostart(mAutostart)
		case <-mQuit.ClickedCh:
			_ = app.svc.Stop()
			if app.ctx != nil {
				wailsruntime.Quit(app.ctx)
			}
			systray.Quit()
			return
		}
	}
}

func toggleAutostart(item *systray.MenuItem) {
	enable := !item.Checked()
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if err := service.SetAutostart(enable, exe); err != nil {
		return
	}
	if enable {
		item.Check()
	} else {
		item.Uncheck()
	}
}

func updateTrayFromEvent(app *App, mStatus, mStart, mStop *systray.MenuItem, ev service.Event) {
	st := app.svc.Status()
	switch st.Status {
	case service.StatusRunning:
		systray.SetTitle("S2 ●")
		mStatus.SetTitle("Status: running")
		mStart.Hide()
		mStop.Show()
	case service.StatusError:
		systray.SetTitle("S2 ✕")
		mStatus.SetTitle("Status: error")
		mStart.Show()
		mStop.Hide()
	default:
		systray.SetTitle("S2")
		mStatus.SetTitle("Status: idle")
		mStart.Show()
		mStop.Hide()
	}
}
