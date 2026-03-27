package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/selfbase-dev/s2-cli/internal/client"
	s2sync "github.com/selfbase-dev/s2-cli/internal/sync"
	"github.com/selfbase-dev/s2-cli/internal/types"
)

// =============================================================================
// Layer 1a: Unit — shouldProcessEvent
// =============================================================================

func TestShouldProcessEvent(t *testing.T) {
	exclude := s2sync.DefaultExclude()

	tests := []struct {
		name string
		rel  string
		want bool
	}{
		{"SkipsS2Dir", ".s2/state.json", false},
		{"SkipsS2Root", ".s2", false},
		{"SkipsExcludedDir", ".git/objects/xx", false},
		{"SkipsNodeModules", "node_modules/pkg/index.js", false},
		{"SkipsDSStore", ".DS_Store", false},
		{"SkipsSwpFile", "file.swp", false},
		{"PassesNormalFile", "docs/readme.md", true},
		{"PassesNestedFile", "src/lib/utils.go", true},
		{"PassesDotenv", ".env", true},
		{"PassesTopLevelFile", "main.go", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldProcessEvent(tt.rel, exclude)
			if got != tt.want {
				t.Errorf("shouldProcessEvent(%q) = %v, want %v", tt.rel, got, tt.want)
			}
		})
	}
}

// =============================================================================
// Layer 1b: Unit — watchLoop
// =============================================================================

func TestWatchLoop_LocalChange_TriggersSync(t *testing.T) {
	var syncCount atomic.Int32
	localEvents := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())

	cfg := WatchLoopConfig{
		SyncFn: func() error {
			syncCount.Add(1)
			return nil
		},
		PollFn: func() (bool, bool, error) {
			return false, false, nil
		},
		LocalEvents:  localEvents,
		PollInterval: 1 * time.Hour, // don't fire during test
		Debounce:     10 * time.Millisecond,
		Ctx:          ctx,
	}

	go watchLoop(cfg)
	localEvents <- struct{}{}
	time.Sleep(50 * time.Millisecond) // wait for debounce + sync
	cancel()

	if c := syncCount.Load(); c != 1 {
		t.Errorf("expected 1 sync, got %d", c)
	}
}

func TestWatchLoop_Debounce_CoalescesEvents(t *testing.T) {
	var syncCount atomic.Int32
	localEvents := make(chan struct{}, 10)
	ctx, cancel := context.WithCancel(context.Background())

	cfg := WatchLoopConfig{
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
	}

	go watchLoop(cfg)

	// Send 5 rapid events within debounce window
	for i := 0; i < 5; i++ {
		localEvents <- struct{}{}
		time.Sleep(5 * time.Millisecond)
	}

	time.Sleep(100 * time.Millisecond) // wait for debounce + sync
	cancel()

	if c := syncCount.Load(); c != 1 {
		t.Errorf("expected 1 sync (debounced), got %d", c)
	}
}

func TestWatchLoop_Debounce_SeparateBatches(t *testing.T) {
	var syncCount atomic.Int32
	localEvents := make(chan struct{}, 10)
	ctx, cancel := context.WithCancel(context.Background())

	cfg := WatchLoopConfig{
		SyncFn: func() error {
			syncCount.Add(1)
			return nil
		},
		PollFn: func() (bool, bool, error) {
			return false, false, nil
		},
		LocalEvents:  localEvents,
		PollInterval: 1 * time.Hour,
		Debounce:     30 * time.Millisecond,
		Ctx:          ctx,
	}

	go watchLoop(cfg)

	// Batch 1
	localEvents <- struct{}{}
	time.Sleep(80 * time.Millisecond) // wait for debounce to fire

	// Batch 2
	localEvents <- struct{}{}
	time.Sleep(80 * time.Millisecond)

	cancel()

	if c := syncCount.Load(); c != 2 {
		t.Errorf("expected 2 syncs (separate batches), got %d", c)
	}
}

func TestWatchLoop_RemotePoll_TriggersSync(t *testing.T) {
	var syncCount atomic.Int32
	localEvents := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	pollCalled := make(chan struct{}, 1)

	cfg := WatchLoopConfig{
		SyncFn: func() error {
			syncCount.Add(1)
			return nil
		},
		PollFn: func() (bool, bool, error) {
			select {
			case pollCalled <- struct{}{}:
			default:
			}
			return true, false, nil // has changes
		},
		LocalEvents:  localEvents,
		PollInterval: 10 * time.Millisecond,
		Debounce:     1 * time.Hour,
		Ctx:          ctx,
	}

	go watchLoop(cfg)
	<-pollCalled                       // wait for at least one poll
	time.Sleep(30 * time.Millisecond) // let sync happen
	cancel()

	if c := syncCount.Load(); c < 1 {
		t.Errorf("expected at least 1 sync from remote poll, got %d", c)
	}
}

func TestWatchLoop_RemotePoll_NoChanges(t *testing.T) {
	var syncCount atomic.Int32
	localEvents := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())

	cfg := WatchLoopConfig{
		SyncFn: func() error {
			syncCount.Add(1)
			return nil
		},
		PollFn: func() (bool, bool, error) {
			return false, false, nil // no changes
		},
		LocalEvents:  localEvents,
		PollInterval: 10 * time.Millisecond,
		Debounce:     1 * time.Hour,
		Ctx:          ctx,
	}

	go watchLoop(cfg)
	time.Sleep(50 * time.Millisecond)
	cancel()

	if c := syncCount.Load(); c != 0 {
		t.Errorf("expected 0 syncs (no changes), got %d", c)
	}
}

func TestWatchLoop_CursorInvalid_TriggersResync(t *testing.T) {
	var syncCount atomic.Int32
	localEvents := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())

	cfg := WatchLoopConfig{
		SyncFn: func() error {
			syncCount.Add(1)
			return nil
		},
		PollFn: func() (bool, bool, error) {
			return false, true, nil // cursor invalid
		},
		LocalEvents:  localEvents,
		PollInterval: 10 * time.Millisecond,
		Debounce:     1 * time.Hour,
		Ctx:          ctx,
	}

	go watchLoop(cfg)
	time.Sleep(50 * time.Millisecond)
	cancel()

	if c := syncCount.Load(); c < 1 {
		t.Errorf("expected at least 1 sync from cursor invalidation, got %d", c)
	}
}

func TestWatchLoop_PollError_NoSync(t *testing.T) {
	var syncCount atomic.Int32
	localEvents := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())

	cfg := WatchLoopConfig{
		SyncFn: func() error {
			syncCount.Add(1)
			return nil
		},
		PollFn: func() (bool, bool, error) {
			return false, false, fmt.Errorf("network error")
		},
		LocalEvents:  localEvents,
		PollInterval: 10 * time.Millisecond,
		Debounce:     1 * time.Hour,
		Ctx:          ctx,
	}

	go watchLoop(cfg)
	time.Sleep(50 * time.Millisecond)
	cancel()

	if c := syncCount.Load(); c != 0 {
		t.Errorf("expected 0 syncs (poll error), got %d", c)
	}
}

func TestWatchLoop_ContextCancel_Exits(t *testing.T) {
	localEvents := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	cfg := WatchLoopConfig{
		SyncFn:       func() error { return nil },
		PollFn:       func() (bool, bool, error) { return false, false, nil },
		LocalEvents:  localEvents,
		PollInterval: 1 * time.Hour,
		Debounce:     1 * time.Hour,
		Ctx:          ctx,
	}

	go func() {
		watchLoop(cfg)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// OK — loop exited
	case <-time.After(1 * time.Second):
		t.Error("watchLoop did not exit after context cancellation")
	}
}

// =============================================================================
// Layer 1c: Unit — getRemoteState (httptest)
// =============================================================================

func TestGetRemoteState_LatestCursorBeforeList(t *testing.T) {
	var callOrder []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/changes/latest") {
			callOrder = append(callOrder, "latest")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"latest": 10}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/files/") {
			callOrder = append(callOrder, "list")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"items":[]}`)
			return
		}
	}))
	defer server.Close()

	c := client.New(server.URL, "s2_test")
	state := &s2sync.State{Files: make(map[string]types.FileState)}
	_, _, err := getRemoteState(c, "docs/", state)
	if err != nil {
		t.Fatalf("getRemoteState failed: %v", err)
	}

	if len(callOrder) < 2 || callOrder[0] != "latest" || callOrder[1] != "list" {
		t.Errorf("expected [latest, list], got %v", callOrder)
	}
}

func TestGetRemoteState_CursorAdvances(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/changes/latest") {
			fmt.Fprint(w, `{"latest": 42}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[]}`)
	}))
	defer server.Close()

	c := client.New(server.URL, "s2_test")
	state := &s2sync.State{Cursor: 5, Files: make(map[string]types.FileState)}
	_, cursor, err := getRemoteState(c, "docs/", state)
	if err != nil {
		t.Fatalf("getRemoteState failed: %v", err)
	}
	if cursor != 42 {
		t.Errorf("expected cursor 42, got %d", cursor)
	}
}

func TestGetRemoteState_CursorFallback_OnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/changes/latest") {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[]}`)
	}))
	defer server.Close()

	c := client.New(server.URL, "s2_test")
	state := &s2sync.State{Cursor: 7, Files: make(map[string]types.FileState)}
	_, cursor, err := getRemoteState(c, "docs/", state)
	if err != nil {
		t.Fatalf("getRemoteState failed: %v", err)
	}
	if cursor != 7 {
		t.Errorf("expected cursor 7 (fallback), got %d", cursor)
	}
}

func TestGetRemoteState_ListBuildsRemoteFiles(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/changes/latest") {
			fmt.Fprint(w, `{"latest": 0}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[
			{"key":"docs/a.txt","size":100,"uploaded":"2026-01-01T00:00:00Z","hash":"aaa"},
			{"key":"docs/b.txt","size":200,"uploaded":"2026-01-01T00:00:00Z","hash":"bbb"},
			{"key":"docs/sub/c.txt","size":300,"uploaded":"2026-01-01T00:00:00Z","hash":"ccc"}
		]}`)
	}))
	defer server.Close()

	c := client.New(server.URL, "s2_test")
	state := &s2sync.State{Files: make(map[string]types.FileState)}
	remoteFiles, _, err := getRemoteState(c, "docs/", state)
	if err != nil {
		t.Fatalf("getRemoteState failed: %v", err)
	}
	if len(remoteFiles) != 3 {
		t.Fatalf("expected 3 remote files, got %d", len(remoteFiles))
	}
	if _, ok := remoteFiles["a.txt"]; !ok {
		t.Error("expected a.txt in remote files")
	}
	if _, ok := remoteFiles["sub/c.txt"]; !ok {
		t.Error("expected sub/c.txt in remote files")
	}
	if remoteFiles["b.txt"].ETag != "bbb" {
		t.Errorf("expected etag bbb, got %s", remoteFiles["b.txt"].ETag)
	}
}

// =============================================================================
// Layer 2a: Integration — addWatchDirs with real fsnotify
// =============================================================================

func TestAddWatchDirs_SkipsS2Dir(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".s2"), 0755)
	os.MkdirAll(filepath.Join(dir, "src"), 0755)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	if err := addWatchDirs(watcher, dir, nil); err != nil {
		t.Fatal(err)
	}

	for _, p := range watcher.WatchList() {
		rel, _ := filepath.Rel(dir, p)
		if strings.HasPrefix(rel, ".s2") {
			t.Errorf(".s2 should not be watched, found: %s", rel)
		}
	}
}

func TestAddWatchDirs_SkipsExcluded(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "node_modules", "pkg"), 0755)
	os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0755)
	os.MkdirAll(filepath.Join(dir, "src"), 0755)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	exclude := s2sync.DefaultExclude()
	if err := addWatchDirs(watcher, dir, exclude); err != nil {
		t.Fatal(err)
	}

	for _, p := range watcher.WatchList() {
		rel, _ := filepath.Rel(dir, p)
		if strings.HasPrefix(rel, "node_modules") {
			t.Errorf("node_modules should not be watched, found: %s", rel)
		}
		if strings.HasPrefix(rel, ".git") {
			t.Errorf(".git should not be watched, found: %s", rel)
		}
	}
}

func TestAddWatchDirs_AddsSubdirs(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub", "deep"), 0755)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	if err := addWatchDirs(watcher, dir, nil); err != nil {
		t.Fatal(err)
	}

	watchList := watcher.WatchList()
	foundSub := false
	foundDeep := false
	for _, p := range watchList {
		rel, _ := filepath.Rel(dir, p)
		if rel == "sub" {
			foundSub = true
		}
		if rel == filepath.Join("sub", "deep") {
			foundDeep = true
		}
	}
	if !foundSub {
		t.Error("expected sub/ to be watched")
	}
	if !foundDeep {
		t.Error("expected sub/deep/ to be watched")
	}
}

// =============================================================================
// Layer 2b: Integration — fsnotify event detection
// =============================================================================

func TestFsnotify_FileCreate_Detected(t *testing.T) {
	dir := t.TempDir()
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	exclude := s2sync.DefaultExclude()
	addWatchDirs(watcher, dir, exclude)

	localChanged := make(chan struct{}, 1)
	go filterFsEventsWithAutoWatch(watcher, dir, exclude, localChanged)

	// Create a new file
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("hello"), 0644)

	select {
	case <-localChanged:
		// OK
	case <-time.After(2 * time.Second):
		t.Error("expected localChanged notification on file create")
	}
}

func TestFsnotify_FileModify_Detected(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "existing.txt")
	os.WriteFile(filePath, []byte("original"), 0644)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	exclude := s2sync.DefaultExclude()
	addWatchDirs(watcher, dir, exclude)

	localChanged := make(chan struct{}, 1)
	go filterFsEventsWithAutoWatch(watcher, dir, exclude, localChanged)

	// Modify file
	time.Sleep(50 * time.Millisecond) // let watcher settle
	os.WriteFile(filePath, []byte("modified"), 0644)

	select {
	case <-localChanged:
		// OK
	case <-time.After(2 * time.Second):
		t.Error("expected localChanged notification on file modify")
	}
}

func TestFsnotify_S2Dir_Ignored(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".s2"), 0755)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	exclude := s2sync.DefaultExclude()
	addWatchDirs(watcher, dir, exclude)

	localChanged := make(chan struct{}, 1)
	go filterFsEventsWithAutoWatch(watcher, dir, exclude, localChanged)

	// Write to .s2 dir (should be ignored)
	os.WriteFile(filepath.Join(dir, ".s2", "state.json"), []byte("{}"), 0644)

	select {
	case <-localChanged:
		t.Error("should NOT get notification for .s2/ changes")
	case <-time.After(200 * time.Millisecond):
		// OK — no notification
	}
}

func TestFsnotify_NewSubdir_AutoWatched(t *testing.T) {
	dir := t.TempDir()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	exclude := s2sync.DefaultExclude()
	addWatchDirs(watcher, dir, exclude)

	localChanged := make(chan struct{}, 10)
	go filterFsEventsWithAutoWatch(watcher, dir, exclude, localChanged)

	// Create new subdirectory
	subDir := filepath.Join(dir, "newdir")
	os.MkdirAll(subDir, 0755)
	time.Sleep(100 * time.Millisecond) // let auto-watch register

	// Drain any pending events from mkdir
	drainChannel(localChanged)

	// Create file in new subdirectory — should be detected
	os.WriteFile(filepath.Join(subDir, "file.txt"), []byte("content"), 0644)

	select {
	case <-localChanged:
		// OK
	case <-time.After(2 * time.Second):
		t.Error("expected notification for file in auto-watched new subdirectory")
	}
}

// =============================================================================
// Layer 3: Stress Tests
// =============================================================================

func TestWatchLoop_HighFrequencyEvents_100(t *testing.T) {
	var syncCount atomic.Int32
	localEvents := make(chan struct{}, 200)
	ctx, cancel := context.WithCancel(context.Background())

	cfg := WatchLoopConfig{
		SyncFn: func() error {
			syncCount.Add(1)
			time.Sleep(5 * time.Millisecond) // simulate some sync work
			return nil
		},
		PollFn: func() (bool, bool, error) {
			return false, false, nil
		},
		LocalEvents:  localEvents,
		PollInterval: 1 * time.Hour,
		Debounce:     20 * time.Millisecond,
		Ctx:          ctx,
	}

	go watchLoop(cfg)

	// Blast 100 events rapidly
	for i := 0; i < 100; i++ {
		select {
		case localEvents <- struct{}{}:
		default:
			// channel full, skip (simulates real fsnotify behavior)
		}
		time.Sleep(1 * time.Millisecond)
	}

	time.Sleep(200 * time.Millisecond) // let debounce settle
	cancel()

	c := syncCount.Load()
	if c < 1 {
		t.Errorf("expected at least 1 sync, got %d", c)
	}
	if c > 10 {
		t.Errorf("expected debouncing to limit syncs, got %d (should be much less than 100)", c)
	}
}

func TestWatchLoop_MultiplePollCycles_StateConsistency(t *testing.T) {
	var syncCount atomic.Int32
	var pollCount atomic.Int32
	localEvents := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())

	cfg := WatchLoopConfig{
		SyncFn: func() error {
			syncCount.Add(1)
			return nil
		},
		PollFn: func() (bool, bool, error) {
			n := pollCount.Add(1)
			// Alternate: changes, no-changes, changes, no-changes...
			hasChanges := n%2 == 1
			return hasChanges, false, nil
		},
		LocalEvents:  localEvents,
		PollInterval: 20 * time.Millisecond,
		Debounce:     1 * time.Hour,
		Ctx:          ctx,
	}

	go watchLoop(cfg)
	time.Sleep(250 * time.Millisecond)
	cancel()

	polls := pollCount.Load()
	syncs := syncCount.Load()

	if polls < 5 {
		t.Errorf("expected at least 5 polls in 250ms at 20ms interval, got %d", polls)
	}
	// Only half the polls have changes
	expectedSyncs := polls / 2
	if syncs < expectedSyncs-2 || syncs > expectedSyncs+2 {
		t.Errorf("expected ~%d syncs (half of %d polls), got %d", expectedSyncs, polls, syncs)
	}
}

func TestWatchLoop_SyncError_DoesNotBlock(t *testing.T) {
	var syncCount atomic.Int32
	localEvents := make(chan struct{}, 10)
	ctx, cancel := context.WithCancel(context.Background())

	cfg := WatchLoopConfig{
		SyncFn: func() error {
			n := syncCount.Add(1)
			if n == 1 {
				return fmt.Errorf("sync failed on first try")
			}
			return nil
		},
		PollFn: func() (bool, bool, error) {
			return false, false, nil
		},
		LocalEvents:  localEvents,
		PollInterval: 1 * time.Hour,
		Debounce:     10 * time.Millisecond,
		Ctx:          ctx,
	}

	go watchLoop(cfg)

	// First batch — will fail
	localEvents <- struct{}{}
	time.Sleep(50 * time.Millisecond)

	// Second batch — should still trigger despite first error
	localEvents <- struct{}{}
	time.Sleep(50 * time.Millisecond)

	cancel()

	c := syncCount.Load()
	if c < 2 {
		t.Errorf("expected at least 2 sync attempts (error should not block loop), got %d", c)
	}
}

func TestFilterFsEvents_RapidCreateModifyDelete(t *testing.T) {
	dir := t.TempDir()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	exclude := s2sync.DefaultExclude()
	addWatchDirs(watcher, dir, exclude)

	localChanged := make(chan struct{}, 100)
	go filterFsEventsWithAutoWatch(watcher, dir, exclude, localChanged)

	// Rapidly create, modify, and delete 50 files
	for i := 0; i < 50; i++ {
		p := filepath.Join(dir, fmt.Sprintf("rapid_%03d.txt", i))
		os.WriteFile(p, []byte("create"), 0644)
		os.WriteFile(p, []byte("modify"), 0644)
		os.Remove(p)
	}

	// Wait for events to settle
	time.Sleep(200 * time.Millisecond)

	// Should have received at least one notification (and no panic/deadlock)
	eventCount := 0
	for {
		select {
		case <-localChanged:
			eventCount++
		default:
			goto done
		}
	}
done:
	if eventCount < 1 {
		t.Error("expected at least 1 event from rapid create/modify/delete")
	}
	// No panic or deadlock means success
}

// =============================================================================
// Helpers
// =============================================================================

func drainChannel(ch <-chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
