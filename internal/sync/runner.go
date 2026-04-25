// runner.go — Sync orchestration shared by cmd/sync.go and cmd/watch.go.

package sync

import (
	"fmt"
	"log/slog"

	"github.com/selfbase-dev/s2-sync/internal/client"
	slog2 "github.com/selfbase-dev/s2-sync/internal/log"
	"github.com/selfbase-dev/s2-sync/internal/types"
)

// SyncOptions controls sync behavior. Callers in cmd/ set these
// based on CLI flags (sync) or hardcoded defaults (watch).
type SyncOptions struct {
	DryRun       bool
	Force        bool
	MaxDeletePct int // 0 = no limit; abort if deletes exceed this %
	// Logger receives every structured event the sync engine emits.
	// Required (nil falls back to slog.Default()).
	Logger *slog.Logger
}

func (o SyncOptions) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}

// RunInitialSync orchestrates a full initial sync: clear archive, walk
// local, bootstrap remote (ADR 0046), compare, execute, persist state.
func RunInitialSync(c *client.Client, localDir, remotePrefix string, state *State, opts SyncOptions) error {
	state.ClearFiles()

	caseInsensitive := IsCaseInsensitiveFS(localDir)

	exclude := LoadExclude(localDir)
	walkResult, err := Walk(localDir, state.Files, exclude)
	if err != nil {
		return fmt.Errorf("local scan failed: %w", err)
	}
	localFiles := walkResult.Files

	remoteFiles, snapshotCursor, err := Bootstrap(c)
	if err != nil {
		return fmt.Errorf("bootstrap failed: %w", err)
	}
	remoteFiles, remoteCollisions := NormalizeRemoteMap(remoteFiles, caseInsensitive)
	reportCollisions(collectCollisions(walkResult.Collisions, remoteCollisions), state, opts)

	prefilled := PrefillArchiveForIdempotentApply(state, localFiles, remoteFiles)
	opts.logger().Info(slog2.SyncStart,
		"local_files", len(localFiles),
		"remote_files", len(remoteFiles),
		"already_in_sync", prefilled,
	)

	plans := Compare(localFiles, remoteFiles, state.Files)
	plans = MergeCaseOnlyRenames(plans, localFiles, state.Files)
	plans = NeutralizeLocalRemoteCaseCollisions(plans, localFiles, state.Files, caseInsensitive)

	result, err := executePlans(plans, localDir, remotePrefix, c, state, opts)
	if err != nil {
		return err
	}

	hasErrors := result != nil && len(result.Errors) > 0
	if !hasErrors && snapshotCursor != "" {
		state.Cursor = snapshotCursor
	}

	if !opts.DryRun {
		if err := state.Save(); err != nil {
			return fmt.Errorf("failed to save state: %w", err)
		}
	}
	return nil
}

// RunIncrementalSync orchestrates an incremental sync from the current
// cursor. Falls back to RunInitialSync on cursor expiry.
func RunIncrementalSync(c *client.Client, localDir, remotePrefix string, state *State, opts SyncOptions) error {
	caseInsensitive := IsCaseInsensitiveFS(localDir)

	exclude := LoadExclude(localDir)
	walkResult, err := Walk(localDir, state.Files, exclude)
	if err != nil {
		return fmt.Errorf("local scan failed: %w", err)
	}
	localFiles := walkResult.Files
	var allRemoteCollisions []CollisionGroup

	resp, err := c.PollChanges(state.Cursor)
	if err == client.ErrCursorGone {
		opts.logger().Warn(slog2.SyncStart, "reason", "cursor_expired_falling_back_to_full")
		state.Cursor = ""
		return RunInitialSync(c, localDir, remotePrefix, state, opts)
	}
	if err != nil {
		return fmt.Errorf("poll changes failed: %w", err)
	}

	var remoteChanges []types.ChangeEntry
	for _, ch := range resp.Changes {
		if state.IsPushedSeq(ch.Seq) {
			continue
		}
		remoteChanges = append(remoteChanges, ch)
	}

	var dirChanges, fileChanges []types.ChangeEntry
	for _, ch := range remoteChanges {
		if ch.IsDir {
			dirChanges = append(dirChanges, ch)
		} else {
			fileChanges = append(fileChanges, ch)
		}
	}

	dirOutcome, err := HandleIncrementalDirEvents(c, localDir, state, dirChanges)
	if err != nil {
		return fmt.Errorf("dir event handling: %w", err)
	}
	for i := range dirOutcome.SubtreeSnapshots {
		filtered, coll := NormalizeRemoteMap(dirOutcome.SubtreeSnapshots[i].Remote, caseInsensitive)
		dirOutcome.SubtreeSnapshots[i].Remote = filtered
		allRemoteCollisions = append(allRemoteCollisions, coll...)
	}
	if dirOutcome.LocalChanged || len(dirOutcome.SubtreeSnapshots) > 0 {
		walkResult, err = Walk(localDir, state.Files, exclude)
		if err != nil {
			return fmt.Errorf("local re-scan failed: %w", err)
		}
		localFiles = walkResult.Files
	}
	reportCollisions(collectCollisions(walkResult.Collisions, allRemoteCollisions), state, opts)

	subtreePlans := dirOutcome.SubtreeComparePlans(localFiles, state.Files)
	fileLevelPlans := CompareIncremental(localFiles, state.Files, fileChanges)
	plans := MergePlansByPath(fileLevelPlans, subtreePlans, dirOutcome.ArchiveWalkPlans)
	plans = MergeCaseOnlyRenames(plans, localFiles, state.Files)
	plans = NeutralizeLocalRemoteCaseCollisions(plans, localFiles, state.Files, caseInsensitive)

	hasLocalChanges := HasLocalChanges(localFiles, state.Files)

	if len(plans) == 0 && len(remoteChanges) == 0 && !hasLocalChanges {
		opts.logger().Info(slog2.SyncIdle)
	}

	result, err := executePlans(plans, localDir, remotePrefix, c, state, opts)
	if err != nil {
		return err
	}

	hasErrors := result != nil && len(result.Errors) > 0
	if !hasErrors {
		if dirOutcome.NewPrimaryCursor != "" {
			state.Cursor = dirOutcome.NewPrimaryCursor
		} else if resp.NextCursor != "" {
			state.Cursor = resp.NextCursor
		}
	}

	if len(resp.Changes) > 0 {
		state.PrunePushedSeqs(resp.Changes[0].Seq)
	}

	if !opts.DryRun {
		if err := state.Save(); err != nil {
			return fmt.Errorf("failed to save state: %w", err)
		}
	}
	return nil
}

// collectCollisions gathers local + remote collision groups into a
// unified set keyed by FoldKey for debounced warning reporting.
func collectCollisions(groups ...[]CollisionGroup) []CollisionGroup {
	var all []CollisionGroup
	for _, g := range groups {
		for _, c := range g {
			if len(c.Paths) > 1 {
				all = append(all, c)
			}
		}
	}
	return all
}

// reportCollisions applies debounce: only logs groups whose FoldKey was
// not in state.ReportedCollisions, and logs "resolved: X" for keys that
// dropped out since the last sync (deterministic tie-break: warnings
// happen on state transitions only).
func reportCollisions(groups []CollisionGroup, state *State, opts SyncOptions) {
	keys := make([]string, 0, len(groups))
	byKey := make(map[string]CollisionGroup, len(groups))
	for _, g := range groups {
		keys = append(keys, g.Key)
		byKey[g.Key] = g
	}
	added, resolved := state.SetReportedCollisions(keys)
	for _, k := range added {
		g := byKey[k]
		opts.logger().Warn(slog2.FileConflict,
			"kind", "case_unicode_collision",
			"key", g.Key,
			"paths", g.Paths,
			"syncing_only", g.Paths[0],
		)
	}
	for _, k := range resolved {
		opts.logger().Info(slog2.FileConflict, "kind", "case_unicode_collision_resolved", "key", k)
	}
}

// executePlans runs the max-delete safety check, prints plan summary,
// and executes. Returns nil result when plans is empty.
func executePlans(plans []types.SyncPlan, localDir, remotePrefix string, c *client.Client, state *State, opts SyncOptions) (*ExecuteResult, error) {
	if len(plans) == 0 {
		return nil, nil
	}

	// Max-delete safety check
	if opts.MaxDeletePct > 0 {
		deleteCount := 0
		for _, p := range plans {
			if p.Action == types.DeleteLocal || p.Action == types.DeleteRemote {
				deleteCount++
			}
		}
		totalTracked := len(state.Files)
		if totalTracked > 0 && !opts.Force && deleteCount*100/totalTracked > opts.MaxDeletePct {
			return nil, fmt.Errorf("safety: %d deletes out of %d tracked files (%d%%) exceeds --max-delete=%d%%. Use --force to override",
				deleteCount, totalTracked, deleteCount*100/totalTracked, opts.MaxDeletePct)
		}
	}

	counts := make(map[types.SyncAction]int)
	for _, p := range plans {
		counts[p.Action]++
	}
	opts.logger().Info(slog2.SyncPlan,
		"push", counts[types.Push],
		"pull", counts[types.Pull],
		"delete", counts[types.DeleteLocal]+counts[types.DeleteRemote],
		"conflict", counts[types.Conflict],
		"dry_run", opts.DryRun,
	)

	result, err := executeWithLogger(plans, localDir, remotePrefix, c, state, opts.DryRun, opts.logger())
	if err != nil {
		return nil, err
	}

	if len(result.Errors) > 0 {
		for _, e := range result.Errors {
			opts.logger().Error(slog2.SyncError, "err", e.Error())
		}
	}

	return result, nil
}

// executeWithLogger is a thin wrapper used by runner.go to thread the
// logger into Execute without breaking the public Execute signature
// (still used by tests / external callers).
func executeWithLogger(plans []types.SyncPlan, localRoot, remotePrefix string, c *client.Client, state *State, dryRun bool, logger *slog.Logger) (*ExecuteResult, error) {
	return execute(plans, localRoot, remotePrefix, c, state, dryRun, executeDeps{logger: logger})
}

