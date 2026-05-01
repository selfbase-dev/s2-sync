package sync

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"

	"github.com/selfbase-dev/s2-sync/internal/client"
	slog2 "github.com/selfbase-dev/s2-sync/internal/log"
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

// Execute applies the sync plans against local filesystem and remote storage.
//
// All plan paths are relative to the token's base_path (which the server
// keeps opaque); they are sent to the API as-is.
func Execute(
	plans []types.SyncPlan,
	localRoot string,
	c *client.Client,
	state *State,
	dryRun bool,
) (*ExecuteResult, error) {
	return execute(plans, localRoot, c, state, dryRun, executeDeps{})
}

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

	for _, plan := range plans {
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
