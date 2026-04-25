package log

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type capture struct {
	mu      sync.Mutex
	records []slog.Record
	err     error
	level   slog.Level
}

func (c *capture) Enabled(_ context.Context, l slog.Level) bool {
	return l >= c.level
}

func (c *capture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
	return c.err
}

func (c *capture) WithAttrs(_ []slog.Attr) slog.Handler { return c }
func (c *capture) WithGroup(_ string) slog.Handler      { return c }

func TestMultiFansOut(t *testing.T) {
	a := &capture{level: slog.LevelDebug}
	b := &capture{level: slog.LevelDebug}
	logger := slog.New(Multi(a, b))
	logger.Info("file.push", "path", "x")

	if len(a.records) != 1 || len(b.records) != 1 {
		t.Fatalf("each handler should see 1 record, got a=%d b=%d", len(a.records), len(b.records))
	}
}

func TestMultiSkipsDisabled(t *testing.T) {
	debug := &capture{level: slog.LevelDebug}
	errOnly := &capture{level: slog.LevelError}
	logger := slog.New(Multi(debug, errOnly))
	logger.Info("file.push")
	if len(debug.records) != 1 {
		t.Fatalf("debug handler missed info record")
	}
	if len(errOnly.records) != 0 {
		t.Fatalf("error-only handler should not see info record")
	}
}

func TestMultiAggregatesErrors(t *testing.T) {
	a := &capture{level: slog.LevelDebug, err: errors.New("a")}
	b := &capture{level: slog.LevelDebug, err: errors.New("b")}
	m := Multi(a, b)
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "x", 0)
	if err := m.Handle(context.Background(), r); err == nil {
		t.Fatal("expected joined error")
	}
}
