package service

import (
	"testing"
	"time"
)

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
}
