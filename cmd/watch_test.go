package cmd

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestShouldProcessEvent(t *testing.T) {
	exclude := func(path string) bool {
		return path == ".DS_Store" || strings.HasPrefix(path, "node_modules")
	}

	tests := []struct {
		rel  string
		want bool
	}{
		{"readme.md", true},
		{"docs/notes.txt", true},
		{".s2", false},
		{".s2/state.json", false},
		{".DS_Store", false},
		{"node_modules/pkg", false},
	}

	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			got := shouldProcessEvent(tt.rel, exclude)
			if got != tt.want {
				t.Errorf("shouldProcessEvent(%q) = %v, want %v", tt.rel, got, tt.want)
			}
		})
	}
}

func TestWatchLoop_LocalEventDebounce(t *testing.T) {
	var syncCount atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	localEvents := make(chan struct{}, 10)

	go watchLoop(WatchLoopConfig{
		SyncFn: func() error {
			syncCount.Add(1)
			return nil
		},
		PollFn: func() (bool, bool, error) {
			return false, false, nil
		},
		LocalEvents:  localEvents,
		PollInterval: 1 * time.Hour,
		Debounce:     50 * time.Millisecond,
		Ctx:          ctx,
	})

	// Send 5 rapid events — should coalesce to 1 sync
	for i := 0; i < 5; i++ {
		localEvents <- struct{}{}
		time.Sleep(10 * time.Millisecond)
	}

	time.Sleep(200 * time.Millisecond)
	cancel()

	if got := syncCount.Load(); got != 1 {
		t.Errorf("sync count = %d, want 1 (debounced)", got)
	}
}

func TestWatchLoop_PollBehavior(t *testing.T) {
	tests := []struct {
		name       string
		hasChanges bool
		needResync bool
		wantSync   bool
	}{
		{"remote changes trigger sync", true, false, true},
		{"cursor invalid triggers sync", false, true, true},
		{"no changes no sync", false, false, false},
		// Bug fix: self-only batches return hasChanges=true so cursor advances.
		// watchLoop must call syncFn; runIncrementalSyncInner filters self-changes
		// and advances cursor even when no real remote work is done.
		{"self-only batch triggers sync for cursor advancement", true, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var syncCount atomic.Int32
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()

			go watchLoop(WatchLoopConfig{
				SyncFn: func() error {
					syncCount.Add(1)
					cancel()
					return nil
				},
				PollFn: func() (bool, bool, error) {
					return tt.hasChanges, tt.needResync, nil
				},
				LocalEvents:  make(chan struct{}),
				PollInterval: 50 * time.Millisecond,
				Debounce:     10 * time.Millisecond,
				Ctx:          ctx,
			})

			<-ctx.Done()
			time.Sleep(50 * time.Millisecond)

			got := syncCount.Load() > 0
			if got != tt.wantSync {
				t.Errorf("synced = %v, want %v", got, tt.wantSync)
			}
		})
	}
}

func TestWatchLoop_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		watchLoop(WatchLoopConfig{
			SyncFn:       func() error { return nil },
			PollFn:       func() (bool, bool, error) { return false, false, nil },
			LocalEvents:  make(chan struct{}),
			PollInterval: 1 * time.Hour,
			Debounce:     1 * time.Second,
			Ctx:          ctx,
		})
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Error("watchLoop did not exit after context cancel")
	}
}
