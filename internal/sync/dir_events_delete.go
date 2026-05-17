package sync

import (
	"strings"

	"github.com/selfbase-dev/s2-sync/internal/types"
)

// expandArchiveDelete walks BOTH the archive and the local filesystem
// for entries under `prefix` and returns DeleteLocal plans for clean
// files (or PreserveLocalRename when the local hash has drifted from
// the archive record). An empty prefix means scope-wide.
//
// When every emitted plan is a clean DeleteLocal (no
// PreserveLocalRename surfacing a drifted file or an untracked file),
// expandArchiveDelete also appends a RmdirLocal plan for the prefix.
// The executor runs that plan after the per-file deletes and falls
// back gracefully if the directory turns out to be non-empty at the
// moment of rmdir (a `.DS_Store` appeared mid-flight, etc.). The
// scope root (empty prefix) is never rmdir'd: the local mount point
// must survive a `delete /` event so the user keeps a place to sync
// into next time.
func expandArchiveDelete(
	archive map[string]types.FileState,
	localDir, prefix string,
) ([]types.SyncPlan, error) {
	seen := make(map[string]struct{})
	var plans []types.SyncPlan
	hasPreserve := false

	for path, fs := range archive {
		if prefix != "" && !strings.HasPrefix(path, prefix) {
			continue
		}
		seen[path] = struct{}{}
		localPath, err := safeJoin(localDir, path)
		if err != nil {
			return nil, err
		}
		localHash, hashErr := hashFile(localPath)
		action := types.DeleteLocal
		if hashErr == nil && localHash != fs.LocalHash {
			action = types.PreserveLocalRename
			hasPreserve = true
		}
		plans = append(plans, types.SyncPlan{Path: path, Action: action, Origin: "dir_event"})
	}

	fsPaths, err := walkLocalUnderPrefix(localDir, prefix)
	if err != nil {
		return nil, err
	}
	for _, path := range fsPaths {
		if _, already := seen[path]; already {
			continue
		}
		plans = append(plans, types.SyncPlan{
			Path:   path,
			Action: types.PreserveLocalRename,
			Origin: "dir_event",
		})
		hasPreserve = true
	}

	// Post-action: rmdir the directory shell once all per-file deletes
	// have run. Suppressed when (a) the prefix is the scope root, since
	// removing the local mount point would surprise the user and break
	// the next sync, or (b) any PreserveLocalRename plan exists — a
	// drifted or untracked file means we deliberately keep the shell so
	// the user notices something is there. The executor adds a final
	// non-recursive os.Remove(prefix), which fails harmlessly if a
	// stray file (e.g. .DS_Store) appears between expansion and
	// execution.
	if prefix != "" && !hasPreserve {
		plans = append(plans, types.SyncPlan{
			Path:   strings.TrimSuffix(prefix, "/"),
			Action: types.RmdirLocal,
			Origin: "dir_event",
		})
	}
	return plans, nil
}
