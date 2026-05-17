// dir_events.go — Hybrid strategy for is_dir change events (ADR 0040).

package sync

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/selfbase-dev/s2-sync/internal/client"
	"github.com/selfbase-dev/s2-sync/internal/types"
)

// SubtreeSnapshot carries a remote file map that the caller must
// Compare() AFTER re-walking the local tree. RemoteDirs lists the
// live directory rows under the subtree so the executor can
// materialize empty folders the user created on the web UI; the file
// pipeline alone wouldn't see them (no file payload to ride into
// existence on).
type SubtreeSnapshot struct {
	Prefix     string
	Remote     map[string]types.RemoteFile
	RemoteDirs []string
}

// DirEventOutcome carries the results of hybrid-strategy processing.
type DirEventOutcome struct {
	ArchiveWalkPlans []types.SyncPlan
	SubtreeSnapshots []SubtreeSnapshot
	NewPrimaryCursor string
	LocalChanged     bool
}

// HandleIncrementalDirEvents applies the hybrid strategy from ADR 0040
// to every is_dir entry in `changes`. Dir-level moves rename archive
// rows in-place via `state.MoveFile`; dir-level deletes expand into
// per-file plans that the executor will later carry out.
func HandleIncrementalDirEvents(
	c *client.Client,
	localDir string,
	state *State,
	changes []types.ChangeEntry,
) (*DirEventOutcome, error) {
	archive := state.Files
	outcome := &DirEventOutcome{}

	for _, ch := range changes {
		if !ch.IsDir {
			continue
		}
		switch ch.Action {
		case "mkdir":
			p, err := safeRelPrefix(ch.PathAfter)
			if err != nil {
				return nil, fmt.Errorf("mkdir %s: %w", ch.PathAfter, err)
			}
			if p == "" {
				continue
			}
			target, err := safeJoin(localDir, strings.TrimSuffix(p, "/"))
			if err != nil {
				return nil, fmt.Errorf("mkdir %s: %w", ch.PathAfter, err)
			}
			if err := os.MkdirAll(target, 0755); err != nil {
				return nil, fmt.Errorf("mkdir %s: %w", p, err)
			}
			outcome.LocalChanged = true

		case "delete":
			prefix, err := safeRelPrefix(ch.PathBefore)
			if err != nil {
				return nil, fmt.Errorf("delete %s: %w", ch.PathBefore, err)
			}
			plans, err := expandArchiveDelete(archive, localDir, prefix)
			if err != nil {
				return nil, fmt.Errorf("expand delete %s: %w", prefix, err)
			}
			outcome.ArchiveWalkPlans = append(outcome.ArchiveWalkPlans, plans...)

		case "move":
			fromPrefix, err := safeRelPrefix(ch.PathBefore)
			if err != nil {
				return nil, fmt.Errorf("move from %s: %w", ch.PathBefore, err)
			}
			toPrefix, err := safeRelPrefix(ch.PathAfter)
			if err != nil {
				return nil, fmt.Errorf("move to %s: %w", ch.PathAfter, err)
			}
			plans, localMutated, err := expandArchiveMove(
				state, localDir, fromPrefix, toPrefix,
			)
			if err != nil {
				return nil, fmt.Errorf("move %s → %s: %w", fromPrefix, toPrefix, err)
			}
			outcome.ArchiveWalkPlans = append(outcome.ArchiveWalkPlans, plans...)
			if localMutated {
				outcome.LocalChanged = true
			}

		case "put":
			isRoot := ch.PathAfter == "" || ch.PathAfter == "/"

			if isRoot {
				outcome.ArchiveWalkPlans = nil
				outcome.SubtreeSnapshots = nil

				remote, remoteDirs, cursor, err := BootstrapWithDirs(c)
				if err != nil {
					return nil, fmt.Errorf("bootstrap (scope-root put): %w", err)
				}
				outcome.SubtreeSnapshots = append(outcome.SubtreeSnapshots, SubtreeSnapshot{
					Prefix:     "",
					Remote:     remote,
					RemoteDirs: remoteDirs,
				})
				outcome.NewPrimaryCursor = cursor
			} else {
				if _, err := safeRelPrefix(ch.PathAfter); err != nil {
					return nil, fmt.Errorf("put %s: %w", ch.PathAfter, err)
				}
				remote, remoteDirs, err := FetchRemoteMapWithDirs(c, ch.PathAfter)
				if err != nil {
					if errors.Is(err, client.ErrNotFound) {
						continue
					}
					return nil, fmt.Errorf("fetch subtree %s: %w", ch.PathAfter, err)
				}
				prefix, _ := safeRelPrefix(ch.PathAfter)
				outcome.SubtreeSnapshots = append(outcome.SubtreeSnapshots, SubtreeSnapshot{
					Prefix:     prefix,
					Remote:     remote,
					RemoteDirs: remoteDirs,
				})
			}
		}
	}
	return outcome, nil
}

// SubtreeComparePlans runs Compare() against each snapshot in the
// outcome and appends MkdirLocal plans for any live dir rows that
// don't already exist locally. Dir materialization is plan-emitted
// (rather than done eagerly) so it flows through the same dry-run /
// max-delete safety net as file-level plans.
func (o *DirEventOutcome) SubtreeComparePlans(
	localFiles map[string]types.LocalFile,
	archive map[string]types.FileState,
) []types.SyncPlan {
	return o.subtreeComparePlansForRoot(localFiles, archive, "")
}

// SubtreeComparePlansForLocalRoot is the local-root-aware variant of
// SubtreeComparePlans. It needs the path so MkdirLocal emission can
// stat the local FS and skip dirs that already exist (avoiding a
// noisy redundant plan when the dir is materialised by an earlier
// file pull's MkdirAll).
func (o *DirEventOutcome) SubtreeComparePlansForLocalRoot(
	localFiles map[string]types.LocalFile,
	archive map[string]types.FileState,
	localRoot string,
) []types.SyncPlan {
	return o.subtreeComparePlansForRoot(localFiles, archive, localRoot)
}

func (o *DirEventOutcome) subtreeComparePlansForRoot(
	localFiles map[string]types.LocalFile,
	archive map[string]types.FileState,
	localRoot string,
) []types.SyncPlan {
	var plans []types.SyncPlan
	for _, snap := range o.SubtreeSnapshots {
		localSub := filterByPrefix(localFiles, snap.Prefix)
		archiveSub := filterByPrefix(archive, snap.Prefix)
		plans = append(plans, Compare(localSub, snap.Remote, archiveSub)...)
		plans = append(plans, MaterializeDirPlans(snap.RemoteDirs, snap.Remote, localRoot)...)
	}
	return plans
}

// MaterializeDirPlans returns MkdirLocal plans for every directory in
// `remoteDirs` that is NOT the implicit parent of an already-pending
// file pull (those parents will be created by os.MkdirAll inside the
// pull executor). When localRoot is non-empty it is also used to skip
// directories that already exist on disk — purely a noise reduction;
// the executor's MkdirAll is idempotent so the duplicate plan is
// harmless if localRoot is unknown.
func MaterializeDirPlans(remoteDirs []string, remoteFiles map[string]types.RemoteFile, localRoot string) []types.SyncPlan {
	if len(remoteDirs) == 0 {
		return nil
	}
	// Pre-compute the set of directory paths that some pending file
	// will implicitly create via os.MkdirAll. We don't actually have
	// the plan list here (would create a cycle); we approximate it by
	// taking the parent dirs of every file in the remote map.
	implicit := make(map[string]struct{}, len(remoteFiles))
	for f := range remoteFiles {
		parent := f
		for {
			idx := strings.LastIndex(parent, "/")
			if idx <= 0 {
				break
			}
			parent = parent[:idx]
			implicit[parent] = struct{}{}
		}
	}
	plans := make([]types.SyncPlan, 0, len(remoteDirs))
	for _, d := range remoteDirs {
		if _, covered := implicit[d]; covered {
			continue
		}
		if localRoot != "" {
			abs, err := safeJoin(localRoot, d)
			if err != nil {
				continue
			}
			if info, err := os.Stat(abs); err == nil && info.IsDir() {
				continue
			}
		}
		plans = append(plans, types.SyncPlan{Path: d, Action: types.MkdirLocal})
	}
	return plans
}

// MergePlansByPath returns the union of plan lists with last-writer-wins dedup.
func MergePlansByPath(lists ...[]types.SyncPlan) []types.SyncPlan {
	total := 0
	for _, l := range lists {
		total += len(l)
	}
	byPath := make(map[string]types.SyncPlan, total)
	for _, list := range lists {
		for _, p := range list {
			byPath[p.Path] = p
		}
	}
	out := make([]types.SyncPlan, 0, len(byPath))
	for _, p := range byPath {
		out = append(out, p)
	}
	sortPlansByPath(out)
	return out
}

// filterByPrefix returns a new map limited to keys matching `prefix`.
func filterByPrefix[T any](m map[string]T, prefix string) map[string]T {
	out := make(map[string]T)
	for k, v := range m {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			out[k] = v
		}
	}
	return out
}
