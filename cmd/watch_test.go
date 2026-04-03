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

func TestWatchLoop_LocalEventTriggersSyncAfterDebounce(t *testing.T) {
	var syncCount atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	localEvents := make(chan struct{}, 1)

	go watchLoop(WatchLoopConfig{
		SyncFn: func() error {
			syncCount.Add(1)
			return nil
		},
		PollFn: func() (bool, bool, error) {
			return false, false, nil
		},
		LocalEvents:  localEvents,
		PollInterval: 1 * time.Hour, // disable polling
		Debounce:     50 * time.Millisecond,
		Ctx:          ctx,
	})

	// Send local event
	localEvents <- struct{}{}

	// Wait for debounce + sync
	time.Sleep(200 * time.Millisecond)
	cancel()

	if got := syncCount.Load(); got != 1 {
		t.Errorf("sync count = %d, want 1", got)
	}
}

func TestWatchLoop_PollTriggersSyncOnRemoteChanges(t *testing.T) {
	var syncCount atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go watchLoop(WatchLoopConfig{
		SyncFn: func() error {
			syncCount.Add(1)
			cancel() // stop after first sync
			return nil
		},
		PollFn: func() (bool, bool, error) {
			return true, false, nil // has remote changes
		},
		LocalEvents:  make(chan struct{}),
		PollInterval: 50 * time.Millisecond,
		Debounce:     10 * time.Millisecond,
		Ctx:          ctx,
	})

	<-ctx.Done()
	time.Sleep(50 * time.Millisecond)

	if got := syncCount.Load(); got < 1 {
		t.Errorf("sync count = %d, want >= 1", got)
	}
}

func TestWatchLoop_NeedResyncTriggersSyncOnCursorInvalid(t *testing.T) {
	var syncCount atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go watchLoop(WatchLoopConfig{
		SyncFn: func() error {
			syncCount.Add(1)
			cancel()
			return nil
		},
		PollFn: func() (bool, bool, error) {
			return false, true, nil // need resync
		},
		LocalEvents:  make(chan struct{}),
		PollInterval: 50 * time.Millisecond,
		Debounce:     10 * time.Millisecond,
		Ctx:          ctx,
	})

	<-ctx.Done()
	time.Sleep(50 * time.Millisecond)

	if got := syncCount.Load(); got < 1 {
		t.Errorf("sync count = %d, want >= 1", got)
	}
}

func TestWatchLoop_NoSyncWhenNoChanges(t *testing.T) {
	var syncCount atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	go watchLoop(WatchLoopConfig{
		SyncFn: func() error {
			syncCount.Add(1)
			return nil
		},
		PollFn: func() (bool, bool, error) {
			return false, false, nil // no changes
		},
		LocalEvents:  make(chan struct{}),
		PollInterval: 50 * time.Millisecond,
		Debounce:     10 * time.Millisecond,
		Ctx:          ctx,
	})

	<-ctx.Done()
	time.Sleep(50 * time.Millisecond)

	if got := syncCount.Load(); got != 0 {
		t.Errorf("sync count = %d, want 0 (no changes)", got)
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
		// OK
	case <-time.After(1 * time.Second):
		t.Error("watchLoop did not exit after context cancel")
	}
}

func TestWatchLoop_DebounceCoalesces(t *testing.T) {
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
		Debounce:     100 * time.Millisecond,
		Ctx:          ctx,
	})

	// Send 5 rapid events
	for i := 0; i < 5; i++ {
		localEvents <- struct{}{}
		time.Sleep(10 * time.Millisecond)
	}

	// Wait for debounce
	time.Sleep(300 * time.Millisecond)
	cancel()

	// Should coalesce to 1 sync (debounce resets on each event)
	if got := syncCount.Load(); got != 1 {
		t.Errorf("sync count = %d, want 1 (debounced)", got)
	}
}
