//go:build windows

package sync

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

var zeroOverlapped windows.Overlapped

// fileLock holds an exclusive LockFileEx lock on a file. Close releases
// the lock and closes the handle. OS cleans up automatically on process exit.
type fileLock struct {
	f *os.File
	h windows.Handle
}

// tryLock acquires a non-blocking exclusive lock on path via LockFileEx.
// Returns a lock handle if acquired; error otherwise (including when
// another process holds the lock).
func tryLock(path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	h := windows.Handle(f.Fd())
	const flags = windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY
	if err := windows.LockFileEx(h, flags, 0, 1, 0, &zeroOverlapped); err != nil {
		f.Close()
		return nil, fmt.Errorf("another s2 process is using this sync root (%s): %w", path, err)
	}
	return &fileLock{f: f, h: h}, nil
}

func (l *fileLock) Close() error {
	_ = windows.UnlockFileEx(l.h, 0, 1, 0, &zeroOverlapped)
	return l.f.Close()
}
