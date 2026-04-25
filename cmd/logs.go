package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	slog2 "github.com/selfbase-dev/s2-sync/internal/log"
	"github.com/spf13/cobra"
)

var (
	logsTail  int
	logsLevel string
)

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Read the s2-sync log file",
	Long: `Print recent records from the JSON Lines log file.

Defaults to the platform-standard location (--log-file). Use --tail to
limit output and --level to filter (debug|info|warn|error).`,
	RunE: runLogs,
}

func init() {
	logsCmd.Flags().IntVar(&logsTail, "tail", 200, "Number of trailing records to print (0 = all)")
	logsCmd.Flags().StringVar(&logsLevel, "level", "debug", "Minimum level to print")
	rootCmd.AddCommand(logsCmd)
}

func runLogs(cmd *cobra.Command, _ []string) error {
	path := logFile
	if path == "" {
		path = slog2.DefaultLogPath()
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	defer f.Close()
	return printLogs(f, cmd.OutOrStdout(), logsTail, slog2.ParseLevel(logsLevel))
}

func printLogs(r io.Reader, w io.Writer, tail int, minLevel slog.Level) error {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 1<<20)
	var ring [][]byte
	for s.Scan() {
		line := append([]byte(nil), s.Bytes()...)
		if tail > 0 && len(ring) == tail {
			ring = append(ring[1:], line)
		} else {
			ring = append(ring, line)
		}
	}
	if err := s.Err(); err != nil {
		return err
	}
	for _, line := range ring {
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if levelOf(rec) < minLevel {
			continue
		}
		fmt.Fprintln(w, formatRecord(rec))
	}
	return nil
}

func levelOf(rec map[string]any) slog.Level {
	switch strings.ToUpper(stringField(rec, "level")) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func formatRecord(rec map[string]any) string {
	var b strings.Builder
	t, _ := time.Parse(time.RFC3339Nano, stringField(rec, "time"))
	if !t.IsZero() {
		b.WriteString(t.Format("15:04:05"))
		b.WriteByte(' ')
	}
	b.WriteString(stringField(rec, "level"))
	b.WriteByte(' ')
	b.WriteString(stringField(rec, "msg"))
	for k, v := range rec {
		if k == "time" || k == "level" || k == "msg" {
			continue
		}
		b.WriteByte(' ')
		b.WriteString(k)
		b.WriteByte('=')
		fmt.Fprintf(&b, "%v", v)
	}
	return b.String()
}

func stringField(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}
