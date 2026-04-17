package main

import (
	"os/exec"
	"runtime"
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
