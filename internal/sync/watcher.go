package sync

import (
	"context"
	"os"
	"path/filepath"
	stdsync "sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// WatchCallbacks plug per-caller behavior into RunWatchLoop. SyncFn and
// PollFn are required; the rest are optional status hooks.
type WatchCallbacks struct {
	// SyncFn runs one full sync cycle. Called serialized under the
	// internal syncMu, so it never races with itself.
	SyncFn func() error

	// PollFn issues one remote poll and reports whether the loop should
	// trigger a sync. Called serialized under syncMu (it reads cursor).
	PollFn func() (hasChanges, needResync bool, err error)

	// OnSyncErr / OnSyncOK fire after each sync attempt (initial and
	// debounced). Either may be nil.
	OnSyncErr func(error)
	OnSyncOK  func()
}

// WatchOptions tunes the loop. Zero values fall back to sensible
// defaults so callers only set what differs.
type WatchOptions struct {
	LocalDir     string
	Exclude      func(string) bool
	PollInterval time.Duration // default 10s
	Debounce     time.Duration // default 2s
}

// RunWatchLoop runs the canonical s2-sync watch protocol:
//
//  1. Initial sync (returns the error if it fails — caller never enters the loop).
//  2. fsnotify subscription on every directory under LocalDir.
//  3. Main loop: debounced sync on local events, ticker-driven remote polls.
//  4. ctx cancelled → stop timers, drain any in-flight sync via syncMu,
//     close state, return.
//
// The function owns syncMu and state.Close() so callers (cmd/watch and
// service.SyncService) can stay thin and identical.
func RunWatchLoop(ctx context.Context, opts WatchOptions, state *State, cb WatchCallbacks) error {
	if opts.PollInterval == 0 {
		opts.PollInterval = 10 * time.Second
	}
	if opts.Debounce == 0 {
		opts.Debounce = 2 * time.Second
	}

	var syncMu stdsync.Mutex

	// Wrap callbacks so syncMu is the single fence against concurrent
	// state access — and so the deferred state.Close() at the end can
	// safely block any late AfterFunc.
	doSync := func() error {
		syncMu.Lock()
		defer syncMu.Unlock()
		return cb.SyncFn()
	}
	report := func(err error) {
		if err != nil {
			if cb.OnSyncErr != nil {
				cb.OnSyncErr(err)
			}
			return
		}
		if cb.OnSyncOK != nil {
			cb.OnSyncOK()
		}
	}

	if err := doSync(); err != nil {
		state.Close()
		return err
	}
	report(nil)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		state.Close()
		return err
	}
	defer watcher.Close()

	if err := addWatchDirs(watcher, opts.LocalDir, opts.Exclude); err != nil {
		state.Close()
		return err
	}

	localChanged := make(chan struct{}, 1)
	go filterFsEvents(ctx, watcher, opts.LocalDir, opts.Exclude, localChanged)

	watchLoopCore(ctx, watchLoopCoreCfg{
		LocalEvents:  localChanged,
		PollInterval: opts.PollInterval,
		Debounce:     opts.Debounce,
		SyncFn: func() {
			if ctx.Err() != nil {
				return
			}
			report(doSync())
		},
		PollFn: cb.PollFn,
	})

	// Hold syncMu across Close so any AfterFunc callback that already
	// fired waits for us, and any one that missed the ctx check wakes up
	// after Close (Save-after-Close is a no-op).
	syncMu.Lock()
	closeErr := state.Close()
	syncMu.Unlock()
	return closeErr
}

// watchLoopCoreCfg is the pure-loop dependencies for watchLoopCore.
// Extracted so unit tests can drive the timer/channel logic without
// spinning up a real fsnotify watcher or sqlite state.
type watchLoopCoreCfg struct {
	LocalEvents  <-chan struct{}
	PollInterval time.Duration
	Debounce     time.Duration
	SyncFn       func()
	PollFn       func() (hasChanges, needResync bool, err error)
}

// watchLoopCore is the timer + debounce + poll core. Returns when ctx
// is cancelled. SyncFn is responsible for its own serialization.
func watchLoopCore(ctx context.Context, cfg watchLoopCoreCfg) {
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	var debounceTimer *time.Timer

	for {
		select {
		case <-ctx.Done():
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			return

		case <-cfg.LocalEvents:
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(cfg.Debounce, cfg.SyncFn)

		case <-ticker.C:
			hasChanges, needResync, err := cfg.PollFn()
			if err != nil {
				continue
			}
			if hasChanges || needResync {
				if debounceTimer != nil {
					debounceTimer.Stop()
				}
				cfg.SyncFn()
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
		if rel == ".s2" || hasS2Prefix(rel) {
			return filepath.SkipDir
		}
		if rel != "." && exclude != nil && exclude(rel) {
			return filepath.SkipDir
		}
		return watcher.Add(path)
	})
}

// filterFsEvents reads fsnotify events, filters .s2 / excluded paths,
// auto-watches newly created directories, and signals localChanged. Exits
// when ctx is cancelled or watcher closes.
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
			if rel == ".s2" || hasS2Prefix(rel) {
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

func hasS2Prefix(rel string) bool {
	return len(rel) >= 4 && rel[:4] == ".s2/"
}
