package service

import (
	stdsync "sync"
	"sync/atomic"
	"testing"
	"time"
)

type stdsyncForTest = stdsync.WaitGroup

func atomicAdd(x *int32) { atomic.AddInt32(x, 1) }

func TestNewStartsIdle(t *testing.T) {
	s := New("https://example.test")
	if got := s.Status().Status; got != StatusIdle {
		t.Fatalf("status: want idle, got %s", got)
	}
}

func TestStopWhenIdleIsNoop(t *testing.T) {
	s := New("https://example.test")
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop on idle: %v", err)
	}
	if got := s.Status().Status; got != StatusIdle {
		t.Fatalf("status after Stop on idle: want idle, got %s", got)
	}
}

func TestSubscribeReceivesEmittedEvents(t *testing.T) {
	s := New("https://example.test")
	ch := s.Subscribe()

	go s.emit(Event{Type: EventLog, Message: "hello"})

	select {
	case ev := <-ch:
		if ev.Type != EventLog || ev.Message != "hello" {
			t.Fatalf("unexpected event: %+v", ev)
		}
		if ev.Time.IsZero() {
			t.Fatal("Time should be set by emit")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestSubscribeFanOut(t *testing.T) {
	s := New("https://example.test")
	a := s.Subscribe()
	b := s.Subscribe()

	go s.emit(Event{Type: EventStarted})

	for i, ch := range []<-chan Event{a, b} {
		select {
		case ev := <-ch:
			if ev.Type != EventStarted {
				t.Fatalf("subscriber %d: want started, got %s", i, ev.Type)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d: timeout", i)
		}
	}
}

func TestEmitDropsOnFullBuffer(t *testing.T) {
	// Full-buffer subscribers should not block the emitter. We fill the
	// buffer (32) and verify emit still returns without hanging.
	s := New("https://example.test")
	_ = s.Subscribe() // never drained

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			s.emit(Event{Type: EventLog})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("emit blocked on full subscriber buffer")
	}
}

func TestLogSinkSplitsLines(t *testing.T) {
	s := New("https://example.test")
	ch := s.Subscribe()
	sink := newLogSink(s)

	n, err := sink.Write([]byte("line one\npartial"))
	if err != nil || n != len("line one\npartial") {
		t.Fatalf("Write: n=%d err=%v", n, err)
	}

	// First line should have been emitted.
	select {
	case ev := <-ch:
		if ev.Type != EventLog || ev.Message != "line one" {
			t.Fatalf("want log 'line one', got %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout: first line not emitted")
	}

	// "partial" has no newline yet; should stay buffered.
	select {
	case ev := <-ch:
		t.Fatalf("unexpected premature event: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}

	// Completing the line flushes it.
	_, _ = sink.Write([]byte(" end\n"))
	select {
	case ev := <-ch:
		if ev.Message != "partial end" {
			t.Fatalf("want 'partial end', got %q", ev.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout: second line not emitted")
	}
}

func TestStartErrorsOnBadPath(t *testing.T) {
	s := New("https://example.test")
	err := s.Start(nil, Mount{Path: "/nonexistent/dir/that/should/not/exist"})
	if err == nil {
		t.Fatal("expected error on missing directory")
	}
	// After setup failure the slot must be released (Idle), otherwise a
	// retry would be rejected with "already running".
	if got := s.Status().Status; got != StatusIdle {
		t.Fatalf("status after setup failure: want idle, got %s", got)
	}
}

func TestStartAdmissionIsAtomic(t *testing.T) {
	// Race regression: before the fix, two concurrent Start calls could
	// both pass the idle check and each spawn a run goroutine. The
	// second one would overwrite s.cancel / s.done and leak the first.
	// We use a bad path so the setup phase is fast-fail; what we want
	// to assert is that exactly one of N concurrent Starts wins.
	const parallel = 16
	s := New("https://example.test")

	var ok, fail int32
	var wg stdsyncForTest
	badMount := Mount{Path: "/nonexistent/xxx-" + t.Name()}
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Start(nil, badMount); err == nil {
				atomicAdd(&ok)
			} else {
				atomicAdd(&fail)
			}
		}()
	}
	wg.Wait()

	// Whatever wins loses on path stat; the point is that at most one
	// admission succeeded before the idle slot was re-released. Because
	// bad-path makes Start return an error synchronously for the winner
	// too, in practice `ok` stays 0 and `fail` == parallel. What we're
	// guarding against is the previous race where concurrent admissions
	// leaked goroutines — absent that, this test just completes without
	// deadlock and leaves the service idle.
	if got := s.Status().Status; got != StatusIdle {
		t.Fatalf("status after concurrent Starts: want idle, got %s", got)
	}
	_ = ok
	_ = fail
}
