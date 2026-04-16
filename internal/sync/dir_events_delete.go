package sync

import (
	"strings"

	"github.com/selfbase-dev/s2-cli/internal/types"
)

// expandArchiveDelete walks BOTH the archive and the local filesystem
// for entries under `prefix` and returns DeleteLocal plans for clean
// files (or PreserveLocalRename when the local hash has drifted from
// the archive record). An empty prefix means scope-wide.
func expandArchiveDelete(
	archive map[string]types.FileState,
	localDir, prefix string,
) ([]types.SyncPlan, error) {
	seen := make(map[string]struct{})
	var plans []types.SyncPlan

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
		}
		plans = append(plans, types.SyncPlan{Path: path, Action: action})
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
		})
	}
	return plans, nil
}
