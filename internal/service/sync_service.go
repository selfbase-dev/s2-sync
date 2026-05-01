// Package service provides a desktop-shaped wrapper around the s2-sync
// core. It exposes a context-driven Start/Stop/Status API so GUI hosts
// (Wails app, future daemon) can drive sync without going through the
// CLI's stdout/signal-based plumbing.
//
// All event reporting flows through *slog.Logger (DI'd by the caller).
// The frontend learns of state changes by either polling Status() or
// reacting to log records emitted via the Wails sink.
package service

import (
	"context"
	"fmt"
	"log/slog"
	stdsync "sync"
	"time"

	slog2 "github.com/selfbase-dev/s2-sync/internal/log"
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
	logger       *slog.Logger
	pollInterval time.Duration
	debounce     time.Duration

	mu     stdsync.Mutex
	state  StateInfo
	cancel context.CancelFunc
	done   chan struct{}
}

// New constructs a SyncService. logger may be nil (defaults to slog.Default).
func New(endpoint string, logger *slog.Logger) *SyncService {
	if logger == nil {
		logger = slog.Default()
	}
	return &SyncService{
		endpoint:     endpoint,
		logger:       logger,
		pollInterval: defaultPollInterval,
		debounce:     defaultDebounce,
		state:        StateInfo{Status: StatusIdle},
	}
}

func (s *SyncService) Status() StateInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *SyncService) Logger() *slog.Logger { return s.logger }

func (s *SyncService) setError(err error) {
	s.mu.Lock()
	s.state.Status = StatusError
	s.state.Error = err.Error()
	s.mu.Unlock()
	s.logger.Error(slog2.SyncError, "err", err.Error())
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
	s.logger.Info(slog2.SyncDone)
}

// Start begins watching the given mount. Returns immediately; sync runs
// in a background goroutine until Stop is called or ctx is cancelled.
//
// Admission is atomic: state, cancel, and done channel are claimed under
// the lock before any I/O. Concurrent Starts always see StatusRunning.
func (s *SyncService) Start(ctx context.Context, mount Mount) error {
	if ctx == nil {
		ctx = context.Background()
	}

	s.mu.Lock()
	switch s.state.Status {
	case StatusRunning:
		s.mu.Unlock()
		return fmt.Errorf("already running")
	case StatusStopping:
		s.mu.Unlock()
		return fmt.Errorf("still stopping; try again shortly")
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.state = StateInfo{Status: StatusRunning, Mount: &mount}
	s.cancel = cancel
	s.done = make(chan struct{})
	done := s.done
	s.mu.Unlock()

	fail := func(err error) error {
		cancel()
		s.mu.Lock()
		s.state = StateInfo{Status: StatusIdle}
		s.mu.Unlock()
		close(done)
		return err
	}

	c, state, err := s2sync.Open(mount.Path, s.endpoint)
	if err != nil {
		return fail(err)
	}

	s.logger.Info(slog2.ServiceStart, "mount", mount.Path)

	// Bind the client to runCtx so Stop's cancel aborts any in-flight
	// HTTP request (Bootstrap / Pull / Push etc.).
	go s.run(runCtx, c.WithContext(runCtx), state, mount.Path)
	return nil
}

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
	s.logger.Info(slog2.ServiceStop, "phase", "requested")
	return nil
}

func (s *SyncService) Wait() {
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done != nil {
		<-done
	}
}
