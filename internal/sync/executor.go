package sync

import (
	"errors"
	"fmt"
	"os"

	"github.com/selfbase-dev/s2-cli/internal/client"
	"github.com/selfbase-dev/s2-cli/internal/types"
)

// ExecuteResult tracks the outcome of sync execution.
type ExecuteResult struct {
	Pushed    int
	Pulled    int
	Deleted   int
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
		remoteKey := remotePrefix + plan.Path

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
		}
	}

	return result, nil
}
