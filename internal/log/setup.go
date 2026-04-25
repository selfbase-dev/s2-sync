package log

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/selfbase-dev/s2-sync/internal/log/sink"
)

type CLIFormat string

const (
	FormatText CLIFormat = "text"
	FormatJSON CLIFormat = "json"
)

func ParseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// CLILogger builds the standard CLI handler stack: console on stderr +
// optional file mirror. Caller closes the returned io.Closer to flush.
func CLILogger(stderr io.Writer, format CLIFormat, level slog.Level, filePath string) (*slog.Logger, io.Closer, error) {
	var handlers []slog.Handler
	if format == FormatJSON {
		handlers = append(handlers, slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: level}))
	} else {
		handlers = append(handlers, sink.NewConsole(stderr, level, isTTY(stderr)))
	}
	var closer io.Closer
	if filePath != "" {
		fh, err := sink.NewFile(filePath, level, 0)
		if err != nil {
			return nil, nil, err
		}
		handlers = append(handlers, fh)
		closer = fh
	}
	return slog.New(Multi(handlers...)), closer, nil
}

func DefaultLogPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "s2sync", "sync.log")
}

func isTTY(w io.Writer) bool {
	type fder interface{ Fd() uintptr }
	f, ok := w.(fder)
	if !ok {
		return false
	}
	fi, err := os.NewFile(f.Fd(), "").Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
