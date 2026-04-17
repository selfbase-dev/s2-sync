//go:build windows

package sync

import (
	"errors"
	"os"
)

type fileLock struct {
	f *os.File
}

// Windows lock is not implemented yet. Releases today target macOS/Linux;
// add a LockFileEx-based implementation (golang.org/x/sys/windows) when
// Windows native support lands.
func tryLock(path string) (*fileLock, error) {
	return nil, errors.New("single-process lock not implemented on Windows")
}

func (l *fileLock) Close() error { return l.f.Close() }
