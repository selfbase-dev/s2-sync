package service

import (
	"context"
	"fmt"

	"github.com/selfbase-dev/s2-sync/internal/client"
	slog2 "github.com/selfbase-dev/s2-sync/internal/log"
	s2sync "github.com/selfbase-dev/s2-sync/internal/sync"
)

// run is the long-lived sync goroutine: initial sync, fsnotify watch,
// poll loop, debounced re-sync. Wires the GUI service's status reporting
// into the shared s2sync.RunWatchLoop so behaviour stays in lockstep
// with the CLI watch command.
func (s *SyncService) run(ctx context.Context, c *client.Client, state *s2sync.State, localDir string) {
	defer close(s.done)
	defer func() {
		s.mu.Lock()
		// Preserve error status; otherwise return to idle.
		if s.state.Status != StatusError {
			s.state.Status = StatusIdle
		}
		s.mu.Unlock()
		s.logger.Info(slog2.ServiceStop, "phase", "stopped")
	}()

	exclude := s2sync.LoadExclude(localDir)
	syncOpts := s2sync.SyncOptions{Logger: s.logger}
	cb := s2sync.WatchCallbacks{
		SyncFn: func() error {
			if state.Cursor == "" {
				return s2sync.RunInitialSync(c, localDir, state, syncOpts)
			}
			return s2sync.RunIncrementalSync(c, localDir, state, syncOpts)
		},
		PollFn: func() (bool, bool, error) {
			cursor := state.Cursor
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
			for _, ch := range resp.Changes {
				if !state.IsPushedSeq(ch.Seq) {
					return true, false, nil
				}
			}
			return len(resp.Changes) > 0, false, nil
		},
		OnSyncOK: s.markSynced,
		OnSyncErr: func(err error) {
			// A user-initiated Stop cancels the client's ctx, which surfaces
			// as a transport error in the sync runner. Treat it as a clean
			// shutdown rather than a failure.
			if ctx.Err() != nil {
				return
			}
			s.setError(err)
		},
	}

	opts := s2sync.WatchOptions{
		LocalDir:     localDir,
		Exclude:      exclude,
		PollInterval: s.pollInterval,
		Debounce:     s.debounce,
	}

	s.logger.Info(slog2.SyncStart, "phase", "initial")
	if err := s2sync.RunWatchLoop(ctx, opts, state, cb); err != nil {
		// Initial sync failed (RunWatchLoop only returns errors from the
		// initial sync or watcher setup). Report once if we haven't been
		// cancelled.
		if ctx.Err() == nil {
			s.setError(fmt.Errorf("initial sync: %w", err))
		}
	}
}
