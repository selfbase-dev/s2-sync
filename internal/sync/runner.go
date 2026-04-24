// runner.go — Sync orchestration shared by cmd/sync.go and cmd/watch.go.

package sync

import (
	"fmt"
	"io"

	"github.com/selfbase-dev/s2-sync/internal/client"
	"github.com/selfbase-dev/s2-sync/internal/types"
)

// SyncOptions controls sync behavior. Callers in cmd/ set these
// based on CLI flags (sync) or hardcoded defaults (watch).
type SyncOptions struct {
	DryRun       bool
	Force        bool
	MaxDeletePct int // 0 = no limit; abort if deletes exceed this %
	Stdout       io.Writer
	Stderr       io.Writer
}

func (o SyncOptions) printf(format string, a ...any) {
	fmt.Fprintf(o.Stdout, format, a...)
}

func (o SyncOptions) errorf(format string, a ...any) {
	fmt.Fprintf(o.Stderr, format, a...)
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
	opts.printf("Local: %d files, Remote: %d files (%d already in sync)\n",
		len(localFiles), len(remoteFiles), prefilled)

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
		opts.printf("Cursor expired, falling back to full sync...\n")
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
		opts.printf("Everything is in sync.\n")
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
		opts.errorf("warning: case/unicode collision for %q: %v (syncing %q only)\n",
			g.Key, g.Paths, g.Paths[0])
	}
	for _, k := range resolved {
		opts.printf("resolved: case/unicode collision for %q\n", k)
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
	opts.printf("Plan: %d push, %d pull, %d delete, %d conflict\n",
		counts[types.Push], counts[types.Pull],
		counts[types.DeleteLocal]+counts[types.DeleteRemote], counts[types.Conflict])

	result, err := Execute(plans, localDir, remotePrefix, c, state, opts.DryRun)
	if err != nil {
		return nil, err
	}

	if len(result.Errors) > 0 {
		for _, e := range result.Errors {
			opts.errorf("  error: %v\n", e)
		}
	}

	return result, nil
}
