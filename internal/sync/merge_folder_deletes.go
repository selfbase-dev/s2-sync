package sync

import (
	"sort"
	"strings"

	"github.com/selfbase-dev/s2-sync/internal/types"
)

// countFilesUnderPrefix counts archive entries whose path starts with
// prefix. Shared by MergeFolderDeletes' eligibility scan, executor
// dry-run accounting, and the max-delete safety valve.
func countFilesUnderPrefix(archive map[string]types.FileState, prefix string) int {
	n := 0
	for path := range archive {
		if strings.HasPrefix(path, prefix) {
			n++
		}
	}
	return n
}

// MergeFolderDeletes collapses DeleteRemote plans that drain an entire
// directory subtree into one DeleteRemoteDir plan, matching the
// server's cascade soft-delete. Eligibility: every archive entry under
// a prefix is being deleted AND the local walk has no remaining files
// under it. The shallowest covering prefix wins.
func MergeFolderDeletes(
	plans []types.SyncPlan,
	localFiles map[string]types.LocalFile,
	archive map[string]types.FileState,
) []types.SyncPlan {
	deleteSet := make(map[string]struct{})
	for _, p := range plans {
		if p.Action == types.DeleteRemote {
			deleteSet[p.Path] = struct{}{}
		}
	}
	if len(deleteSet) == 0 {
		return plans
	}

	candidates := make(map[string]struct{})
	for path := range deleteSet {
		for i := 0; i < len(path); i++ {
			if path[i] == '/' {
				candidates[path[:i+1]] = struct{}{}
			}
		}
	}
	if len(candidates) == 0 {
		return plans
	}

	var eligible []string
	for prefix := range candidates {
		allDeleted := true
		for path := range archive {
			if !strings.HasPrefix(path, prefix) {
				continue
			}
			if _, beingDeleted := deleteSet[path]; !beingDeleted {
				allDeleted = false
				break
			}
		}
		if !allDeleted {
			continue
		}
		hasLocal := false
		for path := range localFiles {
			if strings.HasPrefix(path, prefix) {
				hasLocal = true
				break
			}
		}
		if hasLocal {
			continue
		}
		eligible = append(eligible, prefix)
	}
	if len(eligible) == 0 {
		return plans
	}

	sort.Slice(eligible, func(i, j int) bool { return len(eligible[i]) < len(eligible[j]) })
	var keep []string
	for _, p := range eligible {
		covered := false
		for _, kept := range keep {
			if strings.HasPrefix(p, kept) {
				covered = true
				break
			}
		}
		if !covered {
			keep = append(keep, p)
		}
	}

	covers := func(path string) bool {
		for _, prefix := range keep {
			if strings.HasPrefix(path, prefix) {
				return true
			}
		}
		return false
	}

	out := make([]types.SyncPlan, 0, len(plans))
	for _, p := range plans {
		if p.Action == types.DeleteRemote && covers(p.Path) {
			continue
		}
		out = append(out, p)
	}
	for _, prefix := range keep {
		out = append(out, types.SyncPlan{
			Path:   strings.TrimSuffix(prefix, "/"),
			Action: types.DeleteRemoteDir,
		})
	}
	sortPlansByPath(out)
	return out
}
