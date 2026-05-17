package main

import (
	"os"
	"time"

	"fyne.io/systray"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/selfbase-dev/s2-sync/internal/service"
)

// trayStart / trayEnd are the hooks returned by
// systray.RunWithExternalLoop. start fires the one-shot native init
// (creates NSStatusItem and invokes onReady); end tears it down. Both
// expect the main thread — we call start from main before wails.Run.
var (
	trayStart func()
	trayEnd   func()
)

// setupTray registers onReady / onExit callbacks and captures the
// native start/end hooks. Must run on the main goroutine so it matches
// systray's package-level runtime.LockOSThread pin.
func setupTray(app *App) {
	trayStart, trayEnd = systray.RunWithExternalLoop(
		func() { onTrayReady(app) },
		func() {},
	)
}

// startTray triggers native-side init. Called after Wails has brought
// up NSApplication so NSStatusItem creation happens inside an active
// run loop.
func startTray() {
	if trayStart != nil {
		trayStart()
	}
}

func stopTray() {
	if trayEnd != nil {
		trayEnd()
	}
}

func onTrayReady(app *App) {
	// Fires on the main thread once NSStatusItem is ready. Must not
	// block — build the menu and hand click handling to a goroutine,
	// then return so the run loop keeps pumping.
	// Windows tray draws the icon image, not the title text, so SetIcon
	// is mandatory there. SetTitle is still set for the macOS menu bar.
	systray.SetIcon(trayIcon)
	systray.SetTitle("S2")
	systray.SetTooltip("S2 Sync — idle")

	mShow := systray.AddMenuItem("Show window", "Open the S2 Sync window")
	mStatus := systray.AddMenuItem("Status: idle", "Current sync status")
	mStatus.Disable()
	systray.AddSeparator()
	mStart := systray.AddMenuItem("Start sync", "Resume syncing")
	mStop := systray.AddMenuItem("Stop sync", "Pause syncing")
	mStop.Hide()
	systray.AddSeparator()
	mAutostart := systray.AddMenuItemCheckbox("Start at login", "Launch S2 Sync automatically when you log in", service.IsAutostartEnabled())
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit S2 Sync", "Exit")

	// Keep tray label/visibility in sync with service state. Polling
	// at 1Hz is cheap and avoids wiring a second observer mechanism on
	// SyncService now that all state changes route through *slog.Logger.
	// Track both Status and the in-flight Syncing flag so the tray can
	// distinguish "up to date" from "syncing now" while still Running.
	type trayKey struct {
		status  service.Status
		syncing bool
	}
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		var last trayKey
		for range t.C {
			st := app.svc.Status()
			cur := trayKey{status: st.Status, syncing: st.Syncing}
			if cur != last {
				last = cur
				refreshTray(app, mStatus, mStart, mStop)
			}
		}
	}()

	// One goroutine for menu-click handling — onTrayReady returns
	// immediately so the run loop can pump.
	go func() {
		for {
			select {
			case <-mShow.ClickedCh:
				if app.ctx != nil {
					wailsruntime.WindowShow(app.ctx)
				}
			case <-mStart.ClickedCh:
				if app.ctx == nil {
					continue
				}
				st := app.svc.Status()
				if st.Mount != nil {
					_ = app.svc.Start(app.ctx, *st.Mount)
				} else {
					wailsruntime.WindowShow(app.ctx)
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
				return
			}
		}
	}()
}

func refreshTray(app *App, mStatus, mStart, mStop *systray.MenuItem) {
	// SetTitle is what the macOS menu bar shows. On Windows the tray
	// only renders the icon, so status is also pushed into the tooltip
	// (visible on hover) and the disabled menu row.
	//
	// Running splits into two visually-distinct sub-states based on the
	// Syncing flag: ✓ (up to date) vs ⟳ (sync run in flight). The icon
	// itself stays the same baked-in PNG for now; an animated icon is a
	// follow-up.
	st := app.svc.Status()
	switch st.Status {
	case service.StatusRunning:
		if st.Syncing {
			systray.SetTitle("S2 ⟳")
			systray.SetTooltip("S2 Sync — syncing…")
			mStatus.SetTitle("Status: syncing…")
		} else {
			systray.SetTitle("S2 ✓")
			systray.SetTooltip("S2 Sync — up to date")
			mStatus.SetTitle("Status: up to date")
		}
		mStart.Hide()
		mStop.Show()
	case service.StatusStopping:
		systray.SetTitle("S2 …")
		systray.SetTooltip("S2 Sync — stopping…")
		mStatus.SetTitle("Status: stopping…")
		mStart.Hide()
		mStop.Show()
	case service.StatusError:
		systray.SetTitle("S2 ✕")
		systray.SetTooltip("S2 Sync — error")
		mStatus.SetTitle("Status: error")
		mStart.Show()
		mStop.Hide()
	default:
		systray.SetTitle("S2")
		systray.SetTooltip("S2 Sync — idle")
		mStatus.SetTitle("Status: idle")
		mStart.Show()
		mStop.Hide()
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
