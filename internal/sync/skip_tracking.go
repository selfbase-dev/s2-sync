// skip_tracking.go — degenerate-loop diagnostics for revision-fetch 404
// skips. A single revision-fetch 404 is harmless: the runner advances
// the cursor past the event and moves on. But the same (path,
// revision_id) showing up across multiple syncs is a smell — either a
// client bug (we keep re-reading the same range of change_logs) or a
// server bug (the cursor never advances past this seq). N consecutive
// repeats raise a warn-level log so the user can investigate.
//
// The counter persists in state.db because a degenerate loop requires
// multiple sync invocations to detect; in-memory state would reset on
// every restart and hide the problem.

package sync

import (
	"log/slog"

	slog2 "github.com/selfbase-dev/s2-sync/internal/log"
)

// degenerateSkipThreshold is the number of consecutive skips of the
// same (path, revision_id) at which we emit a warn log. Picked to
// tolerate normal "two syncs read the same range due to a transient
// failure between push and cursor advance" without crying wolf, while
// still catching loops within a handful of runs.
const degenerateSkipThreshold = 3

// recordSkippedEvents persists this sync's revision-fetch 404 skips
// into state.db and emits a degenerate-loop warn when the same event
// is skipped degenerateSkipThreshold times or more across syncs.
//
// Keys present in this sync get their counter incremented (or inserted
// at 1). Keys absent from this sync are pruned — a successful pass
// past the same path/revision means the loop, if any, is broken.
func recordSkippedEvents(state *State, result *ExecuteResult) {
	if state == nil {
		return
	}
	var events []SkippedEvent
	if result != nil {
		events = result.RevisionSkipped
	}
	state.RecordSkippedRevisions(events)
}

// logSkippedSummary emits a per-sync rollup of revision-fetch 404
// skips. The count is always logged when non-zero (info), and elevated
// to warn when it crosses skippedSummaryWarnThreshold so an operator
// sees the spike in their log feed.
func logSkippedSummary(logger *slog.Logger, result *ExecuteResult, state *State) {
	if logger == nil || result == nil || len(result.RevisionSkipped) == 0 {
		return
	}
	paths := make([]string, 0, len(result.RevisionSkipped))
	for _, e := range result.RevisionSkipped {
		paths = append(paths, e.Path)
	}
	level := slog.LevelInfo
	if len(result.RevisionSkipped) >= skippedSummaryWarnThreshold {
		level = slog.LevelWarn
	}
	logger.Log(nil, level, slog2.SyncSkippedSummary,
		"skipped_count", len(result.RevisionSkipped),
		"skipped_paths", paths,
	)

	// Degenerate-loop warn is independent of the per-sync count: even a
	// single event can be a degenerate loop if it has been seen N times.
	if state == nil {
		return
	}
	for _, e := range result.RevisionSkipped {
		count := state.SkippedRevisionCount(e.Key())
		if count >= degenerateSkipThreshold {
			logger.Warn(slog2.SyncSkipDegenerate,
				"path", e.Path,
				"revision_id", e.RevisionID,
				"reason", e.Reason,
				"consecutive_skips", count,
			)
		}
	}
}

// skippedSummaryWarnThreshold is the per-sync skip count above which
// the summary log is elevated from info to warn. Picked to surface
// burst skips (e.g. a whole subtree's revisions pruned in one go) for
// the operator without warning on a single skipped event.
const skippedSummaryWarnThreshold = 10
