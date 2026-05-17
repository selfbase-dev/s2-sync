// skip_tracking_test.go — verifies the cursor policy + degenerate skip
// loop diagnostics introduced for the revision-fetch contract.
//
// Two layers of coverage:
//   1. recordSkippedEvents persists across syncs and increments the
//      consecutive-skip counter; absence of a key reverts it to 0.
//   2. logSkippedSummary emits per-sync counts at info / warn and a
//      separate warn when an event crosses the degenerate threshold.

package sync

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestRecordSkippedRevisions_IncrementsAcrossSyncs(t *testing.T) {
	st := testStateFromArchive(nil)
	ev := SkippedEvent{Path: "a.txt", RevisionID: "r1", Reason: "revision_and_path_404"}

	// First sync skips the event → count 1.
	st.RecordSkippedRevisions([]SkippedEvent{ev})
	if got := st.SkippedRevisionCount(ev.Key()); got != 1 {
		t.Errorf("after first skip: count = %d, want 1", got)
	}

	// Second sync re-skips → count 2.
	st.RecordSkippedRevisions([]SkippedEvent{ev})
	if got := st.SkippedRevisionCount(ev.Key()); got != 2 {
		t.Errorf("after second skip: count = %d, want 2", got)
	}

	// Third sync re-skips → count 3 (degenerate threshold).
	st.RecordSkippedRevisions([]SkippedEvent{ev})
	if got := st.SkippedRevisionCount(ev.Key()); got != 3 {
		t.Errorf("after third skip: count = %d, want 3", got)
	}
}

func TestRecordSkippedRevisions_ClearedWhenEventStopsRepeating(t *testing.T) {
	st := testStateFromArchive(nil)
	ev := SkippedEvent{Path: "a.txt", RevisionID: "r1", Reason: "revision_and_path_404"}

	st.RecordSkippedRevisions([]SkippedEvent{ev})
	st.RecordSkippedRevisions([]SkippedEvent{ev})
	if got := st.SkippedRevisionCount(ev.Key()); got != 2 {
		t.Fatalf("setup: count = %d, want 2", got)
	}

	// Next sync did not re-skip → counter resets to 0.
	st.RecordSkippedRevisions(nil)
	if got := st.SkippedRevisionCount(ev.Key()); got != 0 {
		t.Errorf("after clean sync: count = %d, want 0 (loop broken)", got)
	}
}

func TestRecordSkippedRevisions_DifferentRevisionsTrackedSeparately(t *testing.T) {
	st := testStateFromArchive(nil)
	e1 := SkippedEvent{Path: "a.txt", RevisionID: "r1"}
	e2 := SkippedEvent{Path: "a.txt", RevisionID: "r2"}

	st.RecordSkippedRevisions([]SkippedEvent{e1, e2})
	st.RecordSkippedRevisions([]SkippedEvent{e1}) // r2 not re-skipped

	if got := st.SkippedRevisionCount(e1.Key()); got != 2 {
		t.Errorf("r1 count = %d, want 2", got)
	}
	if got := st.SkippedRevisionCount(e2.Key()); got != 0 {
		t.Errorf("r2 count = %d, want 0 (different key, dropped)", got)
	}
}

func TestSkippedEvent_Key_DistinguishesEmptyRevision(t *testing.T) {
	e1 := SkippedEvent{Path: "a.txt", RevisionID: ""}
	e2 := SkippedEvent{Path: "a.txt", RevisionID: "r1"}
	if e1.Key() == e2.Key() {
		t.Errorf("empty-revision key collided with non-empty: %q", e1.Key())
	}
}

// captureLog parses JSON slog output line by line.
type captureLog struct {
	buf *bytes.Buffer
}

func newCaptureLog() (*slog.Logger, *captureLog) {
	buf := &bytes.Buffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), &captureLog{buf: buf}
}

func (c *captureLog) records() []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(c.buf.String(), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err == nil {
			out = append(out, rec)
		}
	}
	return out
}

func (c *captureLog) find(msg string) []map[string]any {
	var out []map[string]any
	for _, r := range c.records() {
		if r["msg"] == msg {
			out = append(out, r)
		}
	}
	return out
}

func TestLogSkippedSummary_NoOpWhenEmpty(t *testing.T) {
	logger, cap := newCaptureLog()
	st := testStateFromArchive(nil)
	logSkippedSummary(logger, &ExecuteResult{}, st)
	if got := cap.find("sync.skipped_summary"); len(got) != 0 {
		t.Errorf("summary emitted with no skips: %v", got)
	}
}

func TestLogSkippedSummary_InfoBelowThreshold(t *testing.T) {
	logger, cap := newCaptureLog()
	st := testStateFromArchive(nil)
	result := &ExecuteResult{RevisionSkipped: []SkippedEvent{
		{Path: "a.txt", RevisionID: "r1", Reason: "revision_and_path_404"},
		{Path: "b.txt", RevisionID: "r2", Reason: "revision_and_path_404"},
	}}
	logSkippedSummary(logger, result, st)

	got := cap.find("sync.skipped_summary")
	if len(got) != 1 {
		t.Fatalf("summary records = %d, want 1", len(got))
	}
	if got[0]["level"] != "INFO" {
		t.Errorf("level = %v, want INFO (2 < 10 threshold)", got[0]["level"])
	}
	if got[0]["skipped_count"] != float64(2) {
		t.Errorf("skipped_count = %v, want 2", got[0]["skipped_count"])
	}
}

func TestLogSkippedSummary_WarnAtOrAboveThreshold(t *testing.T) {
	logger, cap := newCaptureLog()
	st := testStateFromArchive(nil)
	var events []SkippedEvent
	for i := 0; i < skippedSummaryWarnThreshold; i++ {
		events = append(events, SkippedEvent{Path: "x", RevisionID: "r", Reason: "revision_and_path_404"})
	}
	logSkippedSummary(logger, &ExecuteResult{RevisionSkipped: events}, st)

	got := cap.find("sync.skipped_summary")
	if len(got) != 1 {
		t.Fatalf("summary records = %d, want 1", len(got))
	}
	if got[0]["level"] != "WARN" {
		t.Errorf("level = %v, want WARN at %d skips", got[0]["level"], skippedSummaryWarnThreshold)
	}
}

func TestLogSkippedSummary_DegenerateWarnAfterNRepeats(t *testing.T) {
	logger, cap := newCaptureLog()
	st := testStateFromArchive(nil)
	ev := SkippedEvent{Path: "p.txt", RevisionID: "r-stuck", Reason: "revision_and_path_404"}

	// Simulate degenerateSkipThreshold consecutive syncs that skip the
	// same event. The warn is emitted by logSkippedSummary AFTER
	// recordSkippedEvents has updated the counter — same order the
	// runner uses.
	for i := 1; i <= degenerateSkipThreshold; i++ {
		result := &ExecuteResult{RevisionSkipped: []SkippedEvent{ev}}
		recordSkippedEvents(st, result)
		logSkippedSummary(logger, result, st)

		warns := cap.find("sync.skip_degenerate")
		// Before threshold: no warn. At/after: warn fires every sync.
		if i < degenerateSkipThreshold && len(warns) > 0 {
			t.Errorf("sync %d: degenerate warn emitted prematurely (count = %d)", i, i)
		}
		if i >= degenerateSkipThreshold {
			if len(warns) == 0 {
				t.Errorf("sync %d: expected degenerate warn (count = %d)", i, i)
			} else {
				last := warns[len(warns)-1]
				if last["level"] != "WARN" {
					t.Errorf("level = %v, want WARN", last["level"])
				}
				if last["consecutive_skips"] != float64(i) {
					t.Errorf("consecutive_skips = %v, want %d", last["consecutive_skips"], i)
				}
				if last["path"] != "p.txt" || last["revision_id"] != "r-stuck" {
					t.Errorf("unexpected fields: %v", last)
				}
			}
		}
	}
}

func TestLogSkippedSummary_NoDegenerateWarnWhenCleared(t *testing.T) {
	logger, cap := newCaptureLog()
	st := testStateFromArchive(nil)
	ev := SkippedEvent{Path: "p.txt", RevisionID: "r", Reason: "revision_and_path_404"}

	// Build up to threshold-1 skips, then a sync without the event,
	// then re-skip once. Count resets to 1 → no degenerate warn.
	for i := 0; i < degenerateSkipThreshold-1; i++ {
		recordSkippedEvents(st, &ExecuteResult{RevisionSkipped: []SkippedEvent{ev}})
	}
	recordSkippedEvents(st, &ExecuteResult{})                     // clean sync
	result := &ExecuteResult{RevisionSkipped: []SkippedEvent{ev}} // skip again
	recordSkippedEvents(st, result)
	logSkippedSummary(logger, result, st)

	if got := cap.find("sync.skip_degenerate"); len(got) != 0 {
		t.Errorf("degenerate warn emitted after counter reset: %v", got)
	}
}
