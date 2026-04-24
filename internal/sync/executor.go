package sync

import (
	"errors"
	"fmt"
	"os"
	"path"

	"github.com/selfbase-dev/s2-sync/internal/client"
	"github.com/selfbase-dev/s2-sync/internal/types"
)

// ExecuteResult tracks the outcome of sync execution.
type ExecuteResult struct {
	Pushed    int
	Pulled    int
	Deleted   int
	Moved     int
	Skipped   int
	Conflicts int
	Errors    []error
}

// executeDeps holds unexported seams for testing timing-dependent behavior.
// Production code always uses the zero value (all fields nil).
type executeDeps struct {
	beforePullCommit func(localPath string)
}

// Execute applies the sync plans against local filesystem and remote storage.
func Execute(
	plans []types.SyncPlan,
	localRoot string,
	remotePrefix string,
	c *client.Client,
	state *State,
	dryRun bool,
) (*ExecuteResult, error) {
	return execute(plans, localRoot, remotePrefix, c, state, dryRun, executeDeps{})
}

func execute(
	plans []types.SyncPlan,
	localRoot string,
	remotePrefix string,
	c *client.Client,
	state *State,
	dryRun bool,
	deps executeDeps,
) (*ExecuteResult, error) {
	result := &ExecuteResult{}

	for _, plan := range plans {
		localPath, err := safeJoin(localRoot, plan.Path)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("unsafe plan path %s: %w", plan.Path, err))
			continue
		}
		remoteKey := path.Join(remotePrefix, plan.Path)

		switch plan.Action {
		case types.Push:
			if dryRun {
				fmt.Printf("[dry-run] push: %s\n", plan.Path)
				result.Pushed++
				continue
			}
			if err := executePush(localPath, remoteKey, plan.Path, c, state); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("push %s: %w", plan.Path, err))
				continue
			}
			result.Pushed++
			fmt.Printf("pushed: %s\n", plan.Path)

		case types.Pull:
			if dryRun {
				fmt.Printf("[dry-run] pull: %s\n", plan.Path)
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
				result.Errors = append(result.Errors, fmt.Errorf("pull %s: %w", plan.Path, err))
				continue
			}
			result.Pulled++
			fmt.Printf("pulled: %s\n", plan.Path)

		case types.DeleteLocal:
			if dryRun {
				fmt.Printf("[dry-run] delete local: %s\n", plan.Path)
				result.Deleted++
				continue
			}
			if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
				result.Errors = append(result.Errors, fmt.Errorf("delete local %s: %w", plan.Path, err))
				continue
			}
			state.DeleteFile(plan.Path)
			result.Deleted++
			fmt.Printf("deleted local: %s\n", plan.Path)

		case types.DeleteRemote:
			if dryRun {
				fmt.Printf("[dry-run] delete remote: %s\n", plan.Path)
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
			fmt.Printf("deleted remote: %s\n", plan.Path)

		case types.Conflict:
			if dryRun {
				fmt.Printf("[dry-run] conflict: %s\n", plan.Path)
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
				fmt.Printf("[dry-run] preserve-local-rename: %s\n", plan.Path)
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
			// Atomic server MOVE preserves revision history (ADR 0053).
			if dryRun {
				fmt.Printf("[dry-run] move: %s → %s\n", plan.From, plan.Path)
				result.Moved++
				continue
			}
			srcKey := path.Join(remotePrefix, plan.From)
			moveResult, err := c.Move(srcKey, remoteKey)
			if err != nil {
				if errors.Is(err, client.ErrMoveConflict) {
					// ADR 0053: destination exists → treat as SkipCaseConflict,
					// not delete+push fallback. Leave archive pointing at From
					// so we keep tracking the source; the user must resolve.
					result.Skipped++
					fmt.Printf("skip (case conflict): %s → %s (destination exists on server)\n", plan.From, plan.Path)
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
			fmt.Printf("moved: %s → %s\n", plan.From, plan.Path)

		case types.MoveApply:
			// Pull side of a case-only rename / file move (ADR 0053).
			// Server already moved; we apply os.Rename locally to
			// preserve the inode — critical for case-only renames on
			// case-insensitive FS (Mac/Win) where delete+download of
			// the same inode would race and corrupt the file.
			if dryRun {
				fmt.Printf("[dry-run] move-apply: %s → %s\n", plan.From, plan.Path)
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
			fmt.Printf("move-apply: %s → %s\n", plan.From, plan.Path)

		case types.SkipCaseConflict:
			// ADR 0053: terminal state — do not touch local or remote.
			// Leave archive alone so the collision is re-detected next
			// sync (warning debounce in state prevents log spam).
			if dryRun {
				fmt.Printf("[dry-run] skip (case conflict): %s\n", plan.Path)
				result.Skipped++
				continue
			}
			result.Skipped++
			fmt.Printf("skip (case conflict): %s\n", plan.Path)
		}
	}

	return result, nil
}
