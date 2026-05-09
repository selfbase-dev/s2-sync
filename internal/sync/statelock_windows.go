//go:build windows

package sync

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

type fileLock struct {
	f *os.File
}

func tryLock(path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	ol := new(windows.Overlapped)
	const flags = windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY
	if err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, ol); err != nil {
		f.Close()
		return nil, fmt.Errorf("another s2 process is using this sync root (%s): %w", path, err)
	}
	return &fileLock{f: f}, nil
}

func (l *fileLock) Close() error {
	ol := new(windows.Overlapped)
	_ = windows.UnlockFileEx(windows.Handle(l.f.Fd()), 0, 1, 0, ol)
	return l.f.Close()
}
