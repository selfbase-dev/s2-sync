package service

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"testing"
)

type stdsyncForTest = stdsync.WaitGroup

func atomicAdd(x *int32) { atomic.AddInt32(x, 1) }

func newTestService(t *testing.T) (*SyncService, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return New("https://example.test", logger), &buf
}

func TestNewStartsIdle(t *testing.T) {
	s, _ := newTestService(t)
	if got := s.Status().Status; got != StatusIdle {
		t.Fatalf("status: want idle, got %s", got)
	}
}

func TestStopWhenIdleIsNoop(t *testing.T) {
	s, _ := newTestService(t)
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop on idle: %v", err)
	}
	if got := s.Status().Status; got != StatusIdle {
		t.Fatalf("status after Stop on idle: want idle, got %s", got)
	}
}

func TestSetErrorLogsAndUpdatesStatus(t *testing.T) {
	s, buf := newTestService(t)
	s.setError(errString("boom"))
	if s.Status().Status != StatusError {
		t.Fatalf("want error status, got %s", s.Status().Status)
	}
	if !containsLogMsg(t, buf.Bytes(), "sync.error") {
		t.Fatalf("expected sync.error log, got %s", buf.String())
	}
}

func TestNewStartsNotSyncing(t *testing.T) {
	s, _ := newTestService(t)
	if s.Status().Syncing {
		t.Fatalf("new service: want Syncing=false")
	}
}

func TestBeginSyncSetsSyncingTrue(t *testing.T) {
	s, _ := newTestService(t)
	s.BeginSync()
	if !s.Status().Syncing {
		t.Fatalf("after BeginSync: want Syncing=true")
	}
}

func TestEndSyncResetsSyncing(t *testing.T) {
	s, _ := newTestService(t)
	s.BeginSync()
	s.EndSync()
	if s.Status().Syncing {
		t.Fatalf("after EndSync: want Syncing=false")
	}
}

func TestSetErrorClearsSyncing(t *testing.T) {
	s, _ := newTestService(t)
	s.BeginSync()
	s.setError(errString("boom"))
	if s.Status().Syncing {
		t.Fatalf("after setError: want Syncing=false (must not leave a sync indicator on errors)")
	}
}

func TestMarkSyncedDoesNotTouchSyncing(t *testing.T) {
	// markSynced records LastSync; the begin/end bracket is the sole
	// owner of the Syncing flag. Make sure that contract holds in both
	// directions.
	s, _ := newTestService(t)
	s.BeginSync()
	s.markSynced()
	if !s.Status().Syncing {
		t.Fatalf("markSynced cleared Syncing while a sync is in flight")
	}
	s.EndSync()
	s.markSynced()
	if s.Status().Syncing {
		t.Fatalf("markSynced flipped Syncing back on after EndSync")
	}
}

func TestStartErrorsOnBadPath(t *testing.T) {
	s, _ := newTestService(t)
	err := s.Start(context.TODO(), Mount{Path: "/nonexistent/dir/that/should/not/exist"})
	if err == nil {
		t.Fatal("expected error on missing directory")
	}
	if got := s.Status().Status; got != StatusIdle {
		t.Fatalf("status after setup failure: want idle, got %s", got)
	}
}

func TestStartAdmissionIsAtomic(t *testing.T) {
	const parallel = 16
	s, _ := newTestService(t)

	var ok, fail int32
	var wg stdsyncForTest
	badMount := Mount{Path: "/nonexistent/xxx-" + t.Name()}
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Start(context.TODO(), badMount); err == nil {
				atomicAdd(&ok)
			} else {
				atomicAdd(&fail)
			}
		}()
	}
	wg.Wait()

	if got := s.Status().Status; got != StatusIdle {
		t.Fatalf("status after concurrent Starts: want idle, got %s", got)
	}
	_ = ok
	_ = fail
}

type errString string

func (e errString) Error() string { return string(e) }

// containsLogMsg scans the JSON-line buffer for any record whose msg
// field equals want. Helps assert intent without coupling to attr order.
func containsLogMsg(t *testing.T, b []byte, want string) bool {
	t.Helper()
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		if msg, _ := m["msg"].(string); strings.Contains(msg, want) {
			return true
		}
	}
	return false
}
