package sink

import (
	"context"
	"log/slog"
	"time"
)

// Record is the JSON-serializable shape pushed through Callback. It
// matches the JSON Lines written by the File sink so the GUI's "load
// recent from file" path and live event path use the same schema.
type Record struct {
	Time  time.Time      `json:"time"`
	Level string         `json:"level"`
	Event string         `json:"event"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// Callback invokes fn for every record that passes the level filter.
// Used by the Wails app to forward log records to the React frontend
// via runtime.EventsEmit. Keeps wails imports out of internal/.
type Callback struct {
	level slog.Level
	fn    func(Record)

	attrs  []slog.Attr
	groups []string
}

func NewCallback(level slog.Level, fn func(Record)) *Callback {
	return &Callback{level: level, fn: fn}
}

func (c *Callback) Enabled(_ context.Context, l slog.Level) bool {
	return l >= c.level
}

func (c *Callback) Handle(_ context.Context, r slog.Record) error {
	rec := Record{
		Time:  r.Time,
		Level: levelName(r.Level),
		Event: r.Message,
	}
	if total := len(c.attrs) + r.NumAttrs(); total > 0 {
		rec.Attrs = make(map[string]any, total)
		for _, a := range c.attrs {
			rec.Attrs[a.Key] = a.Value.Resolve().Any()
		}
		r.Attrs(func(a slog.Attr) bool {
			rec.Attrs[a.Key] = a.Value.Resolve().Any()
			return true
		})
	}
	c.fn(rec)
	return nil
}

func (c *Callback) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := *c
	out.attrs = append(append([]slog.Attr{}, c.attrs...), attrs...)
	return &out
}

func (c *Callback) WithGroup(name string) slog.Handler {
	out := *c
	out.groups = append(append([]string{}, c.groups...), name)
	return &out
}
