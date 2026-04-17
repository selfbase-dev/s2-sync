// Package service provides a desktop-shaped wrapper around the s2-sync
// core. It exposes a context-driven Start/Stop/Status/Subscribe API so
// GUI hosts (Wails app, future daemon) can drive sync without going
// through the CLI's stdout/signal-based plumbing.
package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	stdsync "sync"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/selfbase-dev/s2-sync/internal/auth"
	"github.com/selfbase-dev/s2-sync/internal/client"
	s2sync "github.com/selfbase-dev/s2-sync/internal/sync"
)

type Status string

const (
	StatusIdle     Status = "idle"
	StatusRunning  Status = "running"
	StatusStopping Status = "stopping"
	StatusError    Status = "error"
)

type Mount struct {
	Path string `json:"path"`
}

type StateInfo struct {
	Status   Status `json:"status"`
	Mount    *Mount `json:"mount,omitempty"`
	Error    string `json:"error,omitempty"`
	LastSync string `json:"lastSync,omitempty"`
}

const (
	defaultPollInterval = 10 * time.Second
	defaultDebounce     = 2 * time.Second
)

type SyncService struct {
	endpoint     string
	pollInterval time.Duration
	debounce     time.Duration

	mu     stdsync.Mutex
	state  StateInfo
	cancel context.CancelFunc
	done   chan struct{}

	subMu       stdsync.Mutex
	subscribers []chan Event
}

func New(endpoint string) *SyncService {
	return &SyncService{
		endpoint:     endpoint,
		pollInterval: defaultPollInterval,
		debounce:     defaultDebounce,
		state:        StateInfo{Status: StatusIdle},
	}
}

// Status returns a snapshot of the current sync state.
func (s *SyncService) Status() StateInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Subscribe returns a channel of events. The caller should drain the
// channel; events are dropped (oldest first) if the buffer fills.
func (s *SyncService) Subscribe() <-chan Event {
	ch := make(chan Event, 32)
	s.subMu.Lock()
	s.subscribers = append(s.subscribers, ch)
	s.subMu.Unlock()
	return ch
}

func (s *SyncService) emit(ev Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	s.subMu.Lock()
	subs := append([]chan Event(nil), s.subscribers...)
	s.subMu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (s *SyncService) setStatus(st Status) {
	s.mu.Lock()
	s.state.Status = st
	if st != StatusError {
		s.state.Error = ""
	}
	s.mu.Unlock()
}

func (s *SyncService) setError(err error) {
	s.mu.Lock()
	s.state.Status = StatusError
	s.state.Error = err.Error()
	s.mu.Unlock()
	s.emit(Event{Type: EventError, Message: err.Error()})
}

func (s *SyncService) markSynced() {
	now := time.Now()
	s.mu.Lock()
	s.state.LastSync = now.Format(time.RFC3339)
	if s.state.Status == StatusError {
		s.state.Status = StatusRunning
		s.state.Error = ""
	}
	s.mu.Unlock()
	s.emit(Event{Type: EventSynced, Time: now})
}

// Start begins watching the given mount. Returns immediately; sync runs
// in a background goroutine until Stop is called or ctx is cancelled.
func (s *SyncService) Start(ctx context.Context, mount Mount) error {
	s.mu.Lock()
	switch s.state.Status {
	case StatusRunning:
		s.mu.Unlock()
		return fmt.Errorf("already running")
	case StatusStopping:
		s.mu.Unlock()
		return fmt.Errorf("still stopping; try again shortly")
	}
	s.mu.Unlock()

	info, err := os.Stat(mount.Path)
	if err != nil {
		return fmt.Errorf("local directory not found: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", mount.Path)
	}

	if err := s2sync.EnsureIgnoreFile(mount.Path); err != nil {
		return fmt.Errorf("create .s2ignore: %w", err)
	}

	token, err := auth.LoadToken()
	if err != nil {
		return err
	}

	c := client.New(s.endpoint, token)
	me, err := c.Me()
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	identity := s2sync.Identity{
		Endpoint: s.endpoint,
		UserID:   me.UserID,
		BasePath: me.BasePath,
	}
	state, err := s2sync.LoadState(mount.Path, identity)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	remotePrefix := strings.TrimPrefix(me.BasePath, "/")

	runCtx, cancel := context.WithCancel(ctx)

	s.mu.Lock()
	s.state = StateInfo{Status: StatusRunning, Mount: &mount}
	s.cancel = cancel
	s.done = make(chan struct{})
	s.mu.Unlock()

	s.emit(Event{Type: EventStarted, Mount: &mount})

	go s.run(runCtx, c, state, mount.Path, remotePrefix)
	return nil
}

// Stop signals the running sync to cancel and returns immediately. The
// goroutine continues until the in-flight sync round (if any) completes,
// then transitions Status to Idle and emits EventStopped. No-op if not
// running. Use Wait if a caller needs to block until fully stopped.
func (s *SyncService) Stop() error {
	s.mu.Lock()
	if s.state.Status != StatusRunning && s.state.Status != StatusError {
		s.mu.Unlock()
		return nil
	}
	cancel := s.cancel
	s.state.Status = StatusStopping
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	s.emit(Event{Type: EventLog, Message: "stop requested..."})
	return nil
}

// Wait blocks until any in-flight sync goroutine has fully exited. Used
// by tests; production callers prefer Stop's fire-and-forget semantics.
func (s *SyncService) Wait() {
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done != nil {
		<-done
	}
}

// run is the long-lived sync goroutine. Mirrors cmd/watch.go's loop but
// driven by ctx instead of signals, and reports via events.
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

	pollFn := func() (bool, bool, error) {
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
// skipping .s2 and excluded paths. Mirrors cmd/watch.go.
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
// ctx is cancelled or watcher closes. Mirrors cmd/watch.go.
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

// logSink converts writer-style output from the sync runner into Log
// events. Buffers partial lines.
type logSink struct {
	svc *SyncService
	buf bytes.Buffer
	mu  stdsync.Mutex
}

func newLogSink(svc *SyncService) *logSink {
	return &logSink{svc: svc}
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Write(p)
	for {
		i := bytes.IndexByte(l.buf.Bytes(), '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(l.buf.Bytes()[:i]), "\r")
		l.buf.Next(i + 1)
		if line != "" {
			l.svc.emit(Event{Type: EventLog, Message: line})
		}
	}
	return len(p), nil
}

var _ io.Writer = (*logSink)(nil)
