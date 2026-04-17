//go:build unix

package sync

import (
	"fmt"
	"os"
	"syscall"
)

// fileLock holds an advisory flock on a file. Close releases the lock
// and closes the fd. OS cleans up automatically on process exit.
type fileLock struct {
	f *os.File
}

// tryLock acquires a non-blocking exclusive advisory lock on path.
// Returns a lock handle if acquired; error otherwise (including when
// another process holds the lock).
func tryLock(path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another s2 process is using this sync root (%s): %w", path, err)
	}
	return &fileLock{f: f}, nil
}

func (l *fileLock) Close() error {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	return l.f.Close()
}
