package sync

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strings"

	"github.com/selfbase-dev/s2-sync/internal/client"
	slog2 "github.com/selfbase-dev/s2-sync/internal/log"
	"github.com/selfbase-dev/s2-sync/internal/types"
)

// SkippedEvent describes a pull event that was dropped because both the
// revision-pinned and path-based fetches returned 404. The event is not
// an error: the server has the right to prune revisions on its own
// schedule, and the client must let the cursor advance past such events
// instead of retrying forever.
//
// Reason classifies the fail pattern so degenerate-loop diagnostics can
// distinguish "pin 404" (revision pruned mid-flight) from "both 404"
// (file genuinely gone). Both currently advance the cursor; the
// distinction is informational only.
type SkippedEvent struct {
	Path       string
	RevisionID string
	Reason     string // "revision_and_path_404" | "path_404"
}

// reorderRmdirsLast returns a permutation of plans where every
// RmdirLocal entry has been moved to the end of the slice and sorted
// by path length descending (deepest first). The non-rmdir prefix
// keeps its incoming order, so callers that rely on
// MergePlansByPath's deterministic sort still see file-level plans in
// the same sequence.
func reorderRmdirsLast(plans []types.SyncPlan) []types.SyncPlan {
	if len(plans) == 0 {
		return plans
	}
	head := make([]types.SyncPlan, 0, len(plans))
	var rmdirs []types.SyncPlan
	for _, p := range plans {
		if p.Action == types.RmdirLocal {
			rmdirs = append(rmdirs, p)
		} else {
			head = append(head, p)
		}
	}
	if len(rmdirs) == 0 {
		return plans
	}
	// Deepest first so child rmdir runs before its parent.
	sortRmdirsDeepestFirst(rmdirs)
	return append(head, rmdirs...)
}

func sortRmdirsDeepestFirst(plans []types.SyncPlan) {
	// In-place insertion sort by path-segment count descending. Stable
	// over equal depths preserves incoming order, which is sorted by
	// path so deterministic.
	count := func(p string) int { return strings.Count(p, "/") }
	for i := 1; i < len(plans); i++ {
		for j := i; j > 0 && count(plans[j].Path) > count(plans[j-1].Path); j-- {
			plans[j], plans[j-1] = plans[j-1], plans[j]
		}
	}
}

// skipRmdirOnFoldCollision reports whether a RmdirLocal post-action
// should be suppressed because the local filesystem is case-insensitive
// AND a live archive entry under a fold-equivalent (but exact-different)
// directory sits on the same inode. Removing the shell would silently
// delete the live sibling's directory on Mac/Windows.
//
// On case-sensitive filesystems the directory paths are distinct inodes
// so the check is a no-op.
func skipRmdirOnFoldCollision(rmdirPath, localRoot string, archive map[string]types.FileState) bool {
	if !IsCaseInsensitiveFS(localRoot) {
		return false
	}
	foldedPrefix := FoldKey(rmdirPath) + "/"
	exactPrefix := rmdirPath + "/"
	for live := range archive {
		if !strings.HasPrefix(FoldKey(live), foldedPrefix) {
			continue
		}
		if !strings.HasPrefix(live, exactPrefix) {
			return true
		}
	}
	return false
}

// Key returns the persistent dedup key used to detect degenerate loops
// across syncs. Empty RevisionID falls back to path-only so plain
// path-fetch 404s are still tracked.
func (e SkippedEvent) Key() string {
	if e.RevisionID == "" {
		return e.Path + "|"
	}
	return e.Path + "|" + e.RevisionID
}

// ExecuteResult tracks the outcome of sync execution.
//
// RevisionSkipped collects pull events dropped due to 404 on both the
// revision-pinned and path-based fetches. These are NOT errors: the
// runner advances the cursor past them so the sync does not loop
// forever on a pruned revision (the 2026-05-10 incident).
type ExecuteResult struct {
	Pushed          int
	Pulled          int
	Deleted         int
	Moved           int
	Skipped         int
	Conflicts       int
	Errors          []error
	RevisionSkipped []SkippedEvent
}

// executeDeps holds unexported seams for testing timing-dependent behavior
// and the logger. Production code (cmd, service) reaches Execute via the
// runner, which threads opts.Logger in.
type executeDeps struct {
	beforePullCommit func(localPath string)
	logger           *slog.Logger
}

func (d executeDeps) log() *slog.Logger {
	if d.logger != nil {
		return d.logger
	}
	return slog.Default()
}

// Execute is the public entry point used by E2E tests. Production
// callers go through the runner (RunInitialSync / RunIncrementalSync)
// which threads SyncOptions, the structured logger, and the
// max-delete safety check around `execute`. Keeping Execute as a
// thin wrapper preserves the existing test surface without exposing
// the dependency seam.
func Execute(plans []types.SyncPlan, localRoot string, c *client.Client, state *State, dryRun bool) (*ExecuteResult, error) {
	return execute(plans, localRoot, c, state, dryRun, executeDeps{})
}

// execute applies the sync plans against local filesystem and remote storage.
//
// All plan paths are relative to the token's base_path (which the server
// keeps opaque); they are sent to the API as-is.
func execute(
	plans []types.SyncPlan,
	localRoot string,
	c *client.Client,
	state *State,
	dryRun bool,
	deps executeDeps,
) (*ExecuteResult, error) {
	result := &ExecuteResult{}
	log := deps.log()

	// RmdirLocal must run AFTER all the per-file DeleteLocal plans for
	// its prefix have been attempted, otherwise os.Remove always fails
	// with ENOTEMPTY. Deferring them to a second pass also lets the
	// rmdir for `a/b/` run before `a/` (deepest first), so nested
	// shells collapse correctly.
	ordered := reorderRmdirsLast(plans)

	for _, plan := range ordered {
		localPath, err := safeJoin(localRoot, plan.Path)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("unsafe plan path %s: %w", plan.Path, err))
			continue
		}
		remoteKey := plan.Path

		switch plan.Action {
		case types.Push:
			if dryRun {
				log.Info(slog2.FilePush, "path", plan.Path, "dry_run", true)
				result.Pushed++
				continue
			}
			if err := executePush(localPath, remoteKey, plan.Path, c, state); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("push %s: %w", plan.Path, err))
				continue
			}
			result.Pushed++
			log.Info(slog2.FilePush, "path", plan.Path)

		case types.Pull:
			if dryRun {
				log.Info(slog2.FilePull, "path", plan.Path, "dry_run", true)
				result.Pulled++
				continue
			}
			if skipIdempotentPull(plan, state) {
				continue
			}
			if err := executePull(localPath, remoteKey, plan.Path, plan.RevisionID, c, state, deps.beforePullCommit); err != nil {
				if errors.Is(err, errPullAborted) {
					result.Conflicts++
					continue
				}
				if errors.Is(err, client.ErrNotFound) {
					// Both revision-pinned and path-based fetch returned 404.
					// Drop the event and let the cursor advance — retrying
					// will never succeed because the server has pruned this
					// revision (and possibly the file). The fail pattern is
					// distinct from "pin 404 only" because path fallback
					// already happens inside downloadWithFallback.
					ev := SkippedEvent{
						Path:       plan.Path,
						RevisionID: plan.RevisionID,
						Reason:     "revision_and_path_404",
					}
					if plan.RevisionID == "" {
						ev.Reason = "path_404"
					}
					result.RevisionSkipped = append(result.RevisionSkipped, ev)
					log.Warn(slog2.FileSkip,
						"path", plan.Path,
						"revision_id", plan.RevisionID,
						"reason", ev.Reason,
					)
					continue
				}
				result.Errors = append(result.Errors, fmt.Errorf("pull %s: %w", plan.Path, err))
				continue
			}
			result.Pulled++
			log.Info(slog2.FilePull, "path", plan.Path)

		case types.DeleteLocal:
			if dryRun {
				log.Info(slog2.FileDelete, "path", plan.Path, "side", "local", "dry_run", true)
				result.Deleted++
				continue
			}
			if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
				result.Errors = append(result.Errors, fmt.Errorf("delete local %s: %w", plan.Path, err))
				continue
			}
			state.DeleteFile(plan.Path)
			result.Deleted++
			log.Info(slog2.FileDelete, "path", plan.Path, "side", "local")

		case types.DeleteRemote:
			if dryRun {
				log.Info(slog2.FileDelete, "path", plan.Path, "side", "remote", "dry_run", true)
				result.Deleted++
				continue
			}
			delResult, err := c.Delete(remoteKey)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("delete remote %s: %w", plan.Path, err))
				continue
			}
			if delResult != nil && delResult.Seq != nil {
				state.AddPushedSeq(*delResult.Seq)
			}
			state.DeleteFile(plan.Path)
			result.Deleted++
			log.Info(slog2.FileDelete, "path", plan.Path, "side", "remote")

		case types.DeleteRemoteDir:
			prefix := plan.Path + "/"
			if dryRun {
				n := countFilesUnderPrefix(state.Files, prefix)
				log.Info(slog2.FileDelete, "path", plan.Path, "side", "remote", "kind", "dir", "count", n, "dry_run", true)
				result.Deleted += n
				continue
			}
			delResult, err := c.Delete(remoteKey)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("delete remote dir %s: %w", plan.Path, err))
				continue
			}
			if delResult != nil && delResult.Seq != nil {
				state.AddPushedSeq(*delResult.Seq)
			}
			n := state.DeletePrefix(prefix)
			result.Deleted += n
			log.Info(slog2.FileDelete, "path", plan.Path, "side", "remote", "kind", "dir", "count", n)

		case types.Conflict:
			if dryRun {
				log.Info(slog2.FileConflict, "path", plan.Path, "dry_run", true)
				result.Conflicts++
				continue
			}
			if err := executeConflict(localPath, remoteKey, plan.Path, plan.RevisionID, localRoot, c, state); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("conflict %s: %w", plan.Path, err))
				continue
			}
			result.Conflicts++

		case types.PreserveLocalRename:
			if dryRun {
				log.Info(slog2.FileConflict, "path", plan.Path, "kind", "preserve_local_rename", "dry_run", true)
				result.Conflicts++
				continue
			}
			if err := executePreserveLocalRename(localPath, plan.Path, state); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("preserve %s: %w", plan.Path, err))
				continue
			}
			result.Conflicts++

		case types.Move:
			// Case-only rename detected by MergeCaseOnlyRenames.
			// plan.From = archive/remote source, plan.Path = destination.
			// Atomic server MOVE preserves revision history.
			if dryRun {
				log.Info(slog2.FileMove, "from", plan.From, "to", plan.Path, "dry_run", true)
				result.Moved++
				continue
			}
			moveResult, err := c.Move(plan.From, remoteKey)
			if err != nil {
				if errors.Is(err, client.ErrMoveConflict) {
					// destination exists → treat as SkipCaseConflict,
					// not delete+push fallback. Leave archive pointing at From
					// so we keep tracking the source; the user must resolve.
					result.Skipped++
					log.Warn(slog2.FileSkip, "from", plan.From, "to", plan.Path, "reason", "case_conflict_remote_exists")
					continue
				}
				result.Errors = append(result.Errors, fmt.Errorf("move %s → %s: %w", plan.From, plan.Path, err))
				continue
			}
			if moveResult != nil && moveResult.Seq != nil {
				state.AddPushedSeq(*moveResult.Seq)
			}
			state.MoveFile(plan.From, plan.Path)
			if newRow, ok := state.Files[plan.Path]; ok {
				if moveResult != nil {
					newRow.ContentVersion = moveResult.ContentVersion
					state.RecordFile(plan.Path, newRow.LocalHash, moveResult.ContentVersion, newRow.RevisionID)
				}
			}
			result.Moved++
			log.Info(slog2.FileMove, "from", plan.From, "to", plan.Path)

		case types.MoveApply:
			// Pull side of a case-only rename / file move.
			// Server already moved; we apply os.Rename locally to
			// preserve the inode — critical for case-only renames on
			// case-insensitive FS (Mac/Win) where delete+download of
			// the same inode would race and corrupt the file.
			if dryRun {
				log.Info(slog2.FileMove, "from", plan.From, "to", plan.Path, "side", "local", "dry_run", true)
				result.Moved++
				continue
			}
			oldLocal, err := safeJoin(localRoot, plan.From)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("move-apply src %s: %w", plan.From, err))
				continue
			}
			if err := os.MkdirAll(path.Dir(localPath), 0755); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("move-apply mkdir %s: %w", plan.Path, err))
				continue
			}
			if err := os.Rename(oldLocal, localPath); err != nil && !os.IsNotExist(err) {
				result.Errors = append(result.Errors, fmt.Errorf("move-apply rename %s → %s: %w", plan.From, plan.Path, err))
				continue
			}
			state.MoveFile(plan.From, plan.Path)
			result.Moved++
			log.Info(slog2.FileMove, "from", plan.From, "to", plan.Path, "side", "local")

		case types.MkdirLocal:
			if dryRun {
				log.Info(slog2.FilePull, "path", plan.Path, "kind", "dir", "dry_run", true)
				continue
			}
			if err := os.MkdirAll(localPath, 0755); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("mkdir local %s: %w", plan.Path, err))
				continue
			}
			log.Info(slog2.FilePull, "path", plan.Path, "kind", "dir")

		case types.RmdirLocal:
			// Non-recursive rmdir post-action emitted by
			// expandArchiveDelete. The dry-run branch short-circuits
			// before any per-file delete runs, so do the same here.
			//
			// Failure modes that are normal and silent:
			//   - directory non-empty (untracked file like .DS_Store, or
			//     a sibling delete that turned into PreserveLocalRename
			//     after a different folder's expansion ran)
			//   - directory absent (already removed by a parent prefix
			//     rmdir earlier in the plan list)
			//   - case-insensitive FS holds a live fold-equivalent
			//     directory that we must not touch
			if dryRun {
				log.Info(slog2.FileDelete, "path", plan.Path, "kind", "dir", "side", "local", "dry_run", true)
				continue
			}
			if skipRmdirOnFoldCollision(plan.Path, localRoot, state.Files) {
				log.Info(slog2.FileSkip, "path", plan.Path, "kind", "dir", "reason", "case_fold_live_sibling")
				continue
			}
			if err := os.Remove(localPath); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				// ENOTEMPTY / EEXIST are expected: an untracked file or
				// a sibling that drifted is still on disk. Log at info
				// so it's visible without alarming.
				log.Info(slog2.FileSkip, "path", plan.Path, "kind", "dir", "reason", "rmdir_non_empty", "err", err.Error())
				continue
			}
			log.Info(slog2.FileDelete, "path", plan.Path, "kind", "dir", "side", "local")

		case types.SkipCaseConflict:
			// terminal state — do not touch local or remote.
			// Leave archive alone so the collision is re-detected next
			// sync (warning debounce in state prevents log spam).
			if dryRun {
				log.Info(slog2.FileSkip, "path", plan.Path, "reason", "case_conflict", "dry_run", true)
				result.Skipped++
				continue
			}
			result.Skipped++
			log.Warn(slog2.FileSkip, "path", plan.Path, "reason", "case_conflict")
		}
	}

	return result, nil
}
