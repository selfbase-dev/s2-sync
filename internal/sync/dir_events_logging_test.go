package sync

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/selfbase-dev/s2-sync/internal/auth"
	"github.com/selfbase-dev/s2-sync/internal/client"
	slog2 "github.com/selfbase-dev/s2-sync/internal/log"
	"github.com/selfbase-dev/s2-sync/internal/types"
)

// captureHandler is a minimal slog.Handler that records every emitted
// record in memory for assertion. Safe for concurrent use.
type captureHandler struct {
	mu      sync.Mutex
	records []capturedRecord
}

type capturedRecord struct {
	Level slog.Level
	Event string
	Attrs map[string]any
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	rec := capturedRecord{
		Level: r.Level,
		Event: r.Message,
		Attrs: make(map[string]any, r.NumAttrs()),
	}
	r.Attrs(func(a slog.Attr) bool {
		rec.Attrs[a.Key] = a.Value.Resolve().Any()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, rec)
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// For test simplicity inline the With-attrs into a wrapper handler
	// that prepends them to each record.
	return &withAttrsHandler{inner: h, attrs: attrs}
}

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

type withAttrsHandler struct {
	inner *captureHandler
	attrs []slog.Attr
}

func (h *withAttrsHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *withAttrsHandler) Handle(ctx context.Context, r slog.Record) error {
	r2 := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r2.AddAttrs(h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		r2.AddAttrs(a)
		return true
	})
	return h.inner.Handle(ctx, r2)
}

func (h *withAttrsHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &withAttrsHandler{inner: h.inner, attrs: merged}
}

func (h *withAttrsHandler) WithGroup(string) slog.Handler { return h }

func newCaptureLogger() (*slog.Logger, *captureHandler) {
	h := &captureHandler{}
	return slog.New(h), h
}

func (h *captureHandler) eventsWithName(name string) []capturedRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []capturedRecord
	for _, r := range h.records {
		if r.Event == name {
			out = append(out, r)
		}
	}
	return out
}

// --- HandleIncrementalDirEvents: dir.event lifecycle ---

func TestHandleIncrementalDirEvents_LoggingMkdirLifecycle(t *testing.T) {
	dir := t.TempDir()
	c := client.New("http://invalid", auth.NewStaticSource("s2_test"))
	log, cap := newCaptureLogger()

	changes := []types.ChangeEntry{
		{Action: "mkdir", IsDir: true, PathAfter: "/new"},
	}
	_, err := HandleIncrementalDirEvents(c, dir, testStateFromArchive(nil), changes, log)
	if err != nil {
		t.Fatal(err)
	}

	lifecycle := cap.eventsWithName(slog2.DirEvent)
	if len(lifecycle) < 2 {
		t.Fatalf("got %d dir.event records, want >=2 (received + done): %+v", len(lifecycle), lifecycle)
	}
	kinds := make([]string, 0, len(lifecycle))
	for _, r := range lifecycle {
		kinds = append(kinds, r.Attrs["kind"].(string))
	}
	if !contains(kinds, "received") || !contains(kinds, "done") {
		t.Errorf("kinds = %v, want received + done", kinds)
	}

	mkdirEvents := cap.eventsWithName(slog2.DirMkdir)
	if len(mkdirEvents) != 1 {
		t.Fatalf("got %d dir.mkdir records, want 1: %+v", len(mkdirEvents), mkdirEvents)
	}
	if mkdirEvents[0].Attrs["kind"] != "dir_event" {
		t.Errorf("dir.mkdir kind = %v, want dir_event", mkdirEvents[0].Attrs["kind"])
	}
	if mkdirEvents[0].Attrs["path"] != "new/" {
		t.Errorf("dir.mkdir path = %v, want new/", mkdirEvents[0].Attrs["path"])
	}
}

func TestHandleIncrementalDirEvents_LoggingMoveEmitsPerFileMove(t *testing.T) {
	dir := t.TempDir()
	c := client.New("http://invalid", auth.NewStaticSource("s2_test"))
	log, cap := newCaptureLogger()

	h := writeLocalFileExpectHash(t, dir, "old/a.txt", "content")
	archive := map[string]types.FileState{
		"old/a.txt": {LocalHash: h, ContentVersion: 1},
	}

	changes := []types.ChangeEntry{
		{Action: "move", IsDir: true, PathBefore: "/old", PathAfter: "/new"},
	}
	_, err := HandleIncrementalDirEvents(c, dir, testStateFromArchive(archive), changes, log)
	if err != nil {
		t.Fatal(err)
	}

	moves := cap.eventsWithName(slog2.FileMove)
	if len(moves) != 1 {
		t.Fatalf("got %d file.move records, want 1: %+v", len(moves), moves)
	}
	m := moves[0]
	if m.Attrs["kind"] != "dir_event" {
		t.Errorf("file.move kind = %v, want dir_event", m.Attrs["kind"])
	}
	if m.Attrs["side"] != "local" {
		t.Errorf("file.move side = %v, want local", m.Attrs["side"])
	}
	if m.Attrs["from"] != "old/a.txt" || m.Attrs["to"] != "new/a.txt" {
		t.Errorf("file.move from/to = %v/%v, want old/a.txt → new/a.txt", m.Attrs["from"], m.Attrs["to"])
	}

	lifecycle := cap.eventsWithName(slog2.DirEvent)
	if len(lifecycle) < 2 {
		t.Fatalf("got %d dir.event records, want >=2: %+v", len(lifecycle), lifecycle)
	}
}

func TestHandleIncrementalDirEvents_LoggingDeleteEmitsLifecycle(t *testing.T) {
	dir := t.TempDir()
	c := client.New("http://invalid", auth.NewStaticSource("s2_test"))
	log, cap := newCaptureLogger()

	h := writeLocalFileExpectHash(t, dir, "docs/a.txt", "hello")
	archive := map[string]types.FileState{
		"docs/a.txt": {LocalHash: h},
	}
	changes := []types.ChangeEntry{
		{Action: "delete", IsDir: true, PathBefore: "/docs"},
	}
	outcome, err := HandleIncrementalDirEvents(c, dir, testStateFromArchive(archive), changes, log)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.ArchiveWalkPlans) != 1 {
		t.Fatalf("got %d plans, want 1", len(outcome.ArchiveWalkPlans))
	}
	if outcome.ArchiveWalkPlans[0].Origin != "dir_event" {
		t.Errorf("plan origin = %q, want dir_event", outcome.ArchiveWalkPlans[0].Origin)
	}

	lifecycle := cap.eventsWithName(slog2.DirEvent)
	var hasApplied, hasDone bool
	var expandedCount int
	for _, r := range lifecycle {
		k, _ := r.Attrs["kind"].(string)
		if k == "applied" {
			hasApplied = true
			if v, ok := r.Attrs["expanded_count"].(int64); ok {
				expandedCount = int(v)
			} else if v, ok := r.Attrs["expanded_count"].(int); ok {
				expandedCount = v
			}
		}
		if k == "done" {
			hasDone = true
		}
	}
	if !hasApplied || !hasDone {
		t.Errorf("lifecycle missing applied/done: %+v", lifecycle)
	}
	if expandedCount != 1 {
		t.Errorf("expanded_count = %d, want 1", expandedCount)
	}
}

// --- executor: per-file plan emitted from dir_event tagged correctly ---

func TestExecutor_DeleteLocalFromDirEventTagsKind(t *testing.T) {
	dir := t.TempDir()
	writeLocalFile(t, dir, "docs/a.txt", "hello")

	log, cap := newCaptureLogger()
	state := testStateFromArchive(map[string]types.FileState{
		"docs/a.txt": {LocalHash: "h"},
	})
	plans := []types.SyncPlan{
		{Path: "docs/a.txt", Action: types.DeleteLocal, Origin: "dir_event"},
	}
	if _, err := execute(plans, dir, nil, state, false, executeDeps{logger: log}); err != nil {
		t.Fatal(err)
	}

	deletes := cap.eventsWithName(slog2.FileDelete)
	if len(deletes) != 1 {
		t.Fatalf("got %d file.delete records, want 1: %+v", len(deletes), deletes)
	}
	if deletes[0].Attrs["kind"] != "dir_event" {
		t.Errorf("file.delete kind = %v, want dir_event", deletes[0].Attrs["kind"])
	}
	if deletes[0].Attrs["path"] != "docs/a.txt" {
		t.Errorf("file.delete path = %v, want docs/a.txt", deletes[0].Attrs["path"])
	}
}

// --- runner sync.plan emission with dir_events > 0 ---

func TestSyncPlanEmittedWhenOnlyDirEvents(t *testing.T) {
	// executePlans is internal; assert directly that an empty plan list
	// + non-zero dirEventCount still emits sync.plan with dir_events=N.
	log, cap := newCaptureLogger()
	opts := SyncOptions{Logger: log}
	state := testStateFromArchive(nil)

	if _, err := executePlans(nil, 5, t.TempDir(), nil, state, opts); err != nil {
		t.Fatal(err)
	}

	planEvents := cap.eventsWithName(slog2.SyncPlan)
	if len(planEvents) != 1 {
		t.Fatalf("got %d sync.plan records, want 1: %+v", len(planEvents), planEvents)
	}
	dirEvents := planEvents[0].Attrs["dir_events"]
	switch v := dirEvents.(type) {
	case int:
		if v != 5 {
			t.Errorf("dir_events = %d, want 5", v)
		}
	case int64:
		if v != 5 {
			t.Errorf("dir_events = %d, want 5", v)
		}
	default:
		t.Errorf("dir_events attr type = %T, value %v", dirEvents, dirEvents)
	}
}

func TestSyncPlanSkippedWhenNothingHappens(t *testing.T) {
	log, cap := newCaptureLogger()
	opts := SyncOptions{Logger: log}
	state := testStateFromArchive(nil)

	if _, err := executePlans(nil, 0, t.TempDir(), nil, state, opts); err != nil {
		t.Fatal(err)
	}

	planEvents := cap.eventsWithName(slog2.SyncPlan)
	if len(planEvents) != 0 {
		t.Fatalf("got %d sync.plan records, want 0 (no plans, no dir events)", len(planEvents))
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want || strings.EqualFold(x, want) {
			return true
		}
	}
	return false
}
