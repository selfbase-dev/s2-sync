// Package service provides a desktop-shaped wrapper around the s2-sync
// core. It exposes a context-driven Start/Stop/Status/Subscribe API so
// GUI hosts (Wails app, future daemon) can drive sync without going
// through the CLI's stdout/signal-based plumbing.
package service

import (
	"context"
	"fmt"
	"os"
	"strings"
	stdsync "sync"
	"time"

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

	// Bind the client to runCtx so Stop's cancel aborts any in-flight
	// HTTP request (Bootstrap / Pull / Push etc.).
	go s.run(runCtx, c.WithContext(runCtx), state, mount.Path, remotePrefix)
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
