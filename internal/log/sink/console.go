// Package sink contains slog.Handler implementations for the three
// places s2-sync logs land: terminal (Console), file (JSONL), and the
// Wails frontend (Callback).
package sink

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
)

// Console writes records as a single human-readable line to w. Optimized
// for "tail this in a terminal during a sync run", not for parsing.
//
// Format: HH:MM:SS LEVEL event key=value key=value ... msg
//
// Color is on when useColor is true (caller should pass isatty(stderr)).
type Console struct {
	mu       *sync.Mutex
	w        io.Writer
	level    slog.Level
	useColor bool
	attrs    []slog.Attr
	groups   []string
}

func NewConsole(w io.Writer, level slog.Level, useColor bool) *Console {
	return &Console{mu: &sync.Mutex{}, w: w, level: level, useColor: useColor}
}

func (c *Console) Enabled(_ context.Context, l slog.Level) bool {
	return l >= c.level
}

func (c *Console) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Time.Format("15:04:05"))
	b.WriteByte(' ')
	b.WriteString(c.colorLevel(r.Level))
	b.WriteByte(' ')
	b.WriteString(c.colorEvent(r.Message))

	write := func(a slog.Attr) {
		if a.Key == "" {
			return
		}
		b.WriteByte(' ')
		b.WriteString(a.Key)
		b.WriteByte('=')
		b.WriteString(formatValue(a.Value))
	}
	for _, a := range c.attrs {
		write(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		write(a)
		return true
	})
	b.WriteByte('\n')

	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := io.WriteString(c.w, b.String())
	return err
}

func (c *Console) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := *c
	out.attrs = append(append([]slog.Attr{}, c.attrs...), attrs...)
	return &out
}

func (c *Console) WithGroup(name string) slog.Handler {
	out := *c
	out.groups = append(append([]string{}, c.groups...), name)
	return &out
}

func (c *Console) colorLevel(l slog.Level) string {
	name := levelName(l)
	if !c.useColor {
		return name
	}
	switch {
	case l >= slog.LevelError:
		return "\x1b[31m" + name + "\x1b[0m"
	case l >= slog.LevelWarn:
		return "\x1b[33m" + name + "\x1b[0m"
	case l >= slog.LevelInfo:
		return "\x1b[36m" + name + "\x1b[0m"
	default:
		return "\x1b[90m" + name + "\x1b[0m"
	}
}

func (c *Console) colorEvent(s string) string {
	if !c.useColor {
		return s
	}
	return "\x1b[1m" + s + "\x1b[0m"
}

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARN "
	case l >= slog.LevelInfo:
		return "INFO "
	default:
		return "DEBUG"
	}
}

func formatValue(v slog.Value) string {
	v = v.Resolve()
	s := v.String()
	if strings.ContainsAny(s, " \t\"") {
		return fmt.Sprintf("%q", s)
	}
	return s
}
