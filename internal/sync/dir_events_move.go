package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/selfbase-dev/s2-sync/internal/types"
)

// expandArchiveMove renames archive keys and local files from `from`
// prefix to `to` prefix. Files whose local hash has drifted from the
// archive are left in place as PreserveLocalRename conflicts. Archive
// rows are moved via state so dirty tracking flushes them at Save.
func expandArchiveMove(
	state *State,
	localDir, fromPrefix, toPrefix string,
) ([]types.SyncPlan, bool, error) {
	if fromPrefix == "" {
		return nil, false, nil
	}

	archive := state.Files

	type move struct {
		oldKey, newKey string
	}
	handled := make(map[string]struct{})
	var plans []types.SyncPlan
	var archiveMoves []move
	for path := range archive {
		if !strings.HasPrefix(path, fromPrefix) {
			continue
		}
		newKey := toPrefix + strings.TrimPrefix(path, fromPrefix)
		archiveMoves = append(archiveMoves, move{oldKey: path, newKey: newKey})
		handled[path] = struct{}{}
	}

	mutated := false
	for _, m := range archiveMoves {
		fs := archive[m.oldKey]
		oldLocal, err := safeJoin(localDir, m.oldKey)
		if err != nil {
			return nil, mutated, err
		}
		newLocal, err := safeJoin(localDir, m.newKey)
		if err != nil {
			return nil, mutated, err
		}

		localHash, hashErr := hashFile(oldLocal)
		if hashErr != nil && !os.IsNotExist(hashErr) {
			return nil, mutated, fmt.Errorf("hash %s: %w", m.oldKey, hashErr)
		}
		if hashErr == nil && localHash != fs.LocalHash {
			// Drift detected: defer archive mutation until executePreserveLocalRename
			// succeeds. If we delete the archive entry here and the executor's
			// os.Rename later fails, the local file at oldKey would resurface
			// next sync as a brand-new file and get re-pushed under the old
			// path — silent data drift. Keeping the archive entry means a
			// failed rename leaves us in a recoverable state (the next cycle
			// retries the preserve, or eventually delete-local lets the
			// server's new-path copy flow back via change log).
			plans = append(plans, types.SyncPlan{
				Path:   m.oldKey,
				Action: types.PreserveLocalRename,
			})
			continue
		}

		if hashErr == nil {
			if err := os.MkdirAll(filepath.Dir(newLocal), 0755); err != nil {
				return nil, mutated, fmt.Errorf("mkdirall %s: %w", m.newKey, err)
			}
			if err := os.Rename(oldLocal, newLocal); err != nil && !os.IsNotExist(err) {
				return nil, mutated, fmt.Errorf("rename %s → %s: %w", m.oldKey, m.newKey, err)
			}
			mutated = true
		}
		state.MoveFile(m.oldKey, m.newKey)
	}

	fsPaths, err := walkLocalUnderPrefix(localDir, fromPrefix)
	if err != nil {
		return nil, mutated, err
	}
	for _, path := range fsPaths {
		if _, already := handled[path]; already {
			continue
		}
		newPath := toPrefix + strings.TrimPrefix(path, fromPrefix)
		oldLocal, err := safeJoin(localDir, path)
		if err != nil {
			return nil, mutated, err
		}
		newLocal, err := safeJoin(localDir, newPath)
		if err != nil {
			return nil, mutated, err
		}
		if err := os.MkdirAll(filepath.Dir(newLocal), 0755); err != nil {
			return nil, mutated, fmt.Errorf("mkdirall %s: %w", newPath, err)
		}
		if err := os.Rename(oldLocal, newLocal); err != nil && !os.IsNotExist(err) {
			return nil, mutated, fmt.Errorf("rename untracked %s → %s: %w", path, newPath, err)
		}
		mutated = true
	}

	return plans, mutated, nil
}
