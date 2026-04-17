package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	stdsync "sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/selfbase-dev/s2-sync/internal/client"
	s2sync "github.com/selfbase-dev/s2-sync/internal/sync"
)

// run is the long-lived sync goroutine: initial sync, fsnotify watch,
// poll loop, debounced re-sync. Mirrors cmd/watch.go but driven by ctx
// instead of signals and reports via events.
func (s *SyncService) run(ctx context.Context, c *client.Client, state *s2sync.State, localDir, remotePrefix string) {
	defer close(s.done)
	defer func() {
		s.mu.Lock()
		// Preserve error status; otherwise return to idle.
		if s.state.Status != StatusError {
			s.state.Status = StatusIdle
		}
		s.mu.Unlock()
		s.emit(Event{Type: EventStopped})
	}()

	var syncMu stdsync.Mutex
	logSink := newLogSink(s)
	mkOpts := func() s2sync.SyncOptions {
		return s2sync.SyncOptions{Stdout: logSink, Stderr: logSink}
	}

	doSync := func() error {
		syncMu.Lock()
		defer syncMu.Unlock()
		if state.Cursor == "" {
			return s2sync.RunInitialSync(c, localDir, remotePrefix, state, mkOpts())
		}
		return s2sync.RunIncrementalSync(c, localDir, remotePrefix, state, mkOpts())
	}

	s.emit(Event{Type: EventLog, Message: "running initial sync..."})
	if err := doSync(); err != nil {
		s.setError(fmt.Errorf("initial sync: %w", err))
		state.Close()
		return
	}
	s.markSynced()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		s.setError(fmt.Errorf("watcher: %w", err))
		state.Close()
		return
	}
	defer watcher.Close()

	exclude := s2sync.LoadExclude(localDir)
	if err := addWatchDirs(watcher, localDir, exclude); err != nil {
		s.setError(fmt.Errorf("watch: %w", err))
		state.Close()
		return
	}

	localChanged := make(chan struct{}, 1)
	go filterFsEvents(ctx, watcher, localDir, exclude, localChanged)

	pollFn := func() (hasChanges, needResync bool, err error) {
		syncMu.Lock()
		cursor := state.Cursor
		syncMu.Unlock()
		if cursor == "" {
			return false, true, nil
		}
		resp, err := c.PollChanges(cursor)
		if err == client.ErrCursorGone {
			return false, true, nil
		}
		if err != nil {
			return false, false, err
		}
		hasRemote := false
		for _, ch := range resp.Changes {
			if state.IsPushedSeq(ch.Seq) {
				continue
			}
			hasRemote = true
			break
		}
		return hasRemote || len(resp.Changes) > 0, false, nil
	}

	syncFn := func() {
		if ctx.Err() != nil {
			return
		}
		if err := doSync(); err != nil {
			s.setError(err)
		} else {
			s.markSynced()
		}
	}

	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	var debounceTimer *time.Timer

	for {
		select {
		case <-ctx.Done():
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			syncMu.Lock()
			_ = state.Close()
			syncMu.Unlock()
			return

		case <-localChanged:
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(s.debounce, syncFn)

		case <-ticker.C:
			hasChanges, needResync, err := pollFn()
			if err != nil {
				continue
			}
			if hasChanges || needResync {
				if debounceTimer != nil {
					debounceTimer.Stop()
				}
				go syncFn()
			}
		}
	}
}

// addWatchDirs walks root and registers each directory with the watcher,
// skipping .s2 and excluded paths.
func addWatchDirs(watcher *fsnotify.Watcher, root string, exclude func(string) bool) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == ".s2" || strings.HasPrefix(rel, ".s2/") {
			return filepath.SkipDir
		}
		if rel != "." && exclude != nil && exclude(rel) {
			return filepath.SkipDir
		}
		return watcher.Add(path)
	})
}

// filterFsEvents reads fsnotify events, filters .s2 / excluded paths,
// auto-watches new directories, and signals localChanged. Exits when
// ctx is cancelled or watcher closes.
func filterFsEvents(ctx context.Context, watcher *fsnotify.Watcher, localDir string, exclude func(string) bool, localChanged chan<- struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			rel, err := filepath.Rel(localDir, event.Name)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			if rel == ".s2" || strings.HasPrefix(rel, ".s2/") {
				continue
			}
			if exclude != nil && exclude(rel) {
				continue
			}
			if event.Has(fsnotify.Create) {
				if fi, err := os.Stat(event.Name); err == nil && fi.IsDir() {
					_ = addWatchDirs(watcher, event.Name, exclude)
				}
			}
			select {
			case localChanged <- struct{}{}:
			default:
			}
		case _, ok := <-watcher.Errors:
			if !ok {
				return
			}
		}
	}
}
