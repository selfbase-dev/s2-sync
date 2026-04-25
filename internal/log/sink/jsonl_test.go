package sink

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileWritesJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sync.log")
	h, err := NewFile(path, slog.LevelDebug, 0)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(h)
	logger.Info("file.push", "path", "a.txt")
	logger.Error("sync.error", "err", "boom")
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	f, _ := os.Open(path)
	defer f.Close()
	var lines []map[string]any
	s := bufio.NewScanner(f)
	for s.Scan() {
		var m map[string]any
		if err := json.Unmarshal(s.Bytes(), &m); err != nil {
			t.Fatalf("not JSON: %s", s.Text())
		}
		lines = append(lines, m)
	}
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	if lines[0]["msg"] != "file.push" || lines[0]["path"] != "a.txt" {
		t.Fatalf("first line wrong: %v", lines[0])
	}
}

func TestFileRotatesOnceAtMaxBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sync.log")
	h, err := NewFile(path, slog.LevelDebug, 256)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(h)
	for i := 0; i < 50; i++ {
		logger.Info("file.push", "path", strings.Repeat("x", 30))
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
	cur, _ := os.Stat(path)
	if cur.Size() == 0 {
		t.Fatal("current file should have post-rotation records")
	}
}
