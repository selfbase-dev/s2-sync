//go:build !darwin

package service

import "errors"

// IsAutostartEnabled is a non-darwin stub.
func IsAutostartEnabled() bool { return false }

// SetAutostart is a non-darwin stub.
func SetAutostart(_ bool, _ string) error {
	return errors.New("autostart is only implemented on macOS")
}
