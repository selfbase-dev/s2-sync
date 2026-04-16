// dir_events.go — Hybrid strategy for is_dir change events (ADR 0040).

package sync

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/selfbase-dev/s2-cli/internal/client"
	"github.com/selfbase-dev/s2-cli/internal/types"
)

// SubtreeSnapshot carries a remote file map that the caller must
// Compare() AFTER re-walking the local tree.
type SubtreeSnapshot struct {
	Prefix string
	Remote map[string]types.RemoteFile
}

// DirEventOutcome carries the results of hybrid-strategy processing.
type DirEventOutcome struct {
	ArchiveWalkPlans []types.SyncPlan
	SubtreeSnapshots []SubtreeSnapshot
	NewPrimaryCursor string
	LocalChanged     bool
}

// HandleIncrementalDirEvents applies the hybrid strategy from ADR 0040
// to every is_dir entry in `changes`.
func HandleIncrementalDirEvents(
	c *client.Client,
	localDir string,
	archive map[string]types.FileState,
	changes []types.ChangeEntry,
) (*DirEventOutcome, error) {
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
				archive, localDir, fromPrefix, toPrefix,
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

				remote, cursor, err := Bootstrap(c)
				if err != nil {
					return nil, fmt.Errorf("bootstrap (scope-root put): %w", err)
				}
				outcome.SubtreeSnapshots = append(outcome.SubtreeSnapshots, SubtreeSnapshot{
					Prefix: "",
					Remote: remote,
				})
				outcome.NewPrimaryCursor = cursor
			} else {
				if _, err := safeRelPrefix(ch.PathAfter); err != nil {
					return nil, fmt.Errorf("put %s: %w", ch.PathAfter, err)
				}
				remote, err := FetchRemoteMap(c, ch.PathAfter)
				if err != nil {
					if errors.Is(err, client.ErrNotFound) {
						continue
					}
					return nil, fmt.Errorf("fetch subtree %s: %w", ch.PathAfter, err)
				}
				prefix, _ := safeRelPrefix(ch.PathAfter)
				outcome.SubtreeSnapshots = append(outcome.SubtreeSnapshots, SubtreeSnapshot{
					Prefix: prefix,
					Remote: remote,
				})
			}
		}
	}
	return outcome, nil
}

// SubtreeComparePlans runs Compare() against each snapshot in the outcome.
func (o *DirEventOutcome) SubtreeComparePlans(
	localFiles map[string]types.LocalFile,
	archive map[string]types.FileState,
) []types.SyncPlan {
	var plans []types.SyncPlan
	for _, snap := range o.SubtreeSnapshots {
		localSub := filterByPrefix(localFiles, snap.Prefix)
		archiveSub := filterByPrefix(archive, snap.Prefix)
		plans = append(plans, Compare(localSub, snap.Remote, archiveSub)...)
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
