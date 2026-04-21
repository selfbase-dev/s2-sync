//go:build !darwin

package main

// makeAccessoryApp is a no-op on non-macOS platforms. The
// "Accessory" activation policy is a macOS concept tied to
// NSApp / LSUIElement (see accessory_darwin.go); Windows and Linux
// rely on the system tray icon for the menu-bar-only UX instead.
func makeAccessoryApp() {}
