package sink

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// File writes JSON Lines to path, rotating once when size exceeds
// maxBytes (renames to <path>.1, overwriting any previous .1). One
// generation only — the goal is "the user clicks Open log file in the
// GUI and sees recent activity", not auditable archival.
type File struct {
	path     string
	maxBytes int64
	level    slog.Level

	mu      *sync.Mutex
	f       *os.File
	written *atomic.Int64
	inner   slog.Handler
}

func NewFile(path string, level slog.Level, maxBytes int64) (*File, error) {
	if maxBytes <= 0 {
		maxBytes = 5 << 20
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := openAppend(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	written := &atomic.Int64{}
	written.Store(info.Size())
	return &File{
		path:     path,
		maxBytes: maxBytes,
		level:    level,
		mu:       &sync.Mutex{},
		f:        f,
		written:  written,
		inner:    slog.NewJSONHandler(&countingWriter{w: f, n: written}, &slog.HandlerOptions{Level: level}),
	}, nil
}

func openAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
}

func (h *File) Path() string { return h.path }

func (h *File) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level
}

func (h *File) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.written.Load() >= h.maxBytes {
		if err := h.rotateLocked(); err != nil {
			return err
		}
	}
	return h.inner.Handle(ctx, r)
}

func (h *File) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := *h
	out.inner = h.inner.WithAttrs(attrs)
	return &out
}

func (h *File) WithGroup(name string) slog.Handler {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := *h
	out.inner = h.inner.WithGroup(name)
	return &out
}

func (h *File) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.f == nil {
		return nil
	}
	err := h.f.Close()
	h.f = nil
	return err
}

func (h *File) rotateLocked() error {
	if err := h.f.Close(); err != nil {
		return err
	}
	old := h.path + ".1"
	_ = os.Remove(old)
	if err := os.Rename(h.path, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := openAppend(h.path)
	if err != nil {
		return err
	}
	h.f = f
	h.written.Store(0)
	h.inner = slog.NewJSONHandler(&countingWriter{w: f, n: h.written}, &slog.HandlerOptions{Level: h.level})
	return nil
}

type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}
