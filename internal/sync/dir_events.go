// dir_events.go — Hybrid strategy for is_dir change events (ADR 0040).
//
// The server emits directory-level events for bulk operations: delete a
// subtree, move a subtree, restore from trash, scope-external put, etc.
// The CLI handles each event in one of three ways:
//
//   1. Archive walk — delete / move within scope. Walk the archive by
//      path prefix, hash-check every touched file against its current
//      local copy, and emit DeleteLocal / Conflict plans (delete) or
//      perform os.Rename in place (move). No network traffic.
//
//   2. Snapshot fetch — put dir / restore / ancestor enter / scope-root
//      operations. Call /api/snapshot?path=X to get the fresh atomic
//      state, then Compare local + snapshot + archive scoped to that
//      subtree to produce Pull / Conflict / NoOp plans.
//
//   3. os.MkdirAll — mkdir events. Pure local side effect, no plans.
//
// Scope-root put (ADR 0040 §cursor semantics) uses the snapshot's cursor
// as the new primary cursor, effectively re-bootstrapping. Subtree put
// does NOT replace the primary cursor — it's a hint for that subtree.

package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/selfbase-dev/s2-cli/internal/client"
	"github.com/selfbase-dev/s2-cli/internal/types"
)

func sortPlansByPath(plans []types.SyncPlan) {
	sort.Slice(plans, func(i, j int) bool { return plans[i].Path < plans[j].Path })
}

// DirEventOutcome carries the results of hybrid-strategy processing.
type DirEventOutcome struct {
	// Plans to merge with the file-level CompareIncremental plans.
	// Includes archive-walk DeleteLocal / Conflict entries and per-subtree
	// Compare results from snapshot fetches.
	ExtraPlans []types.SyncPlan

	// NewPrimaryCursor, when non-empty, is the cursor returned by a
	// scope-root snapshot. The caller must use it as the new primary
	// cursor instead of advancing the existing one via /api/changes.
	// For subtree snapshots this remains "" (ADR 0040 §cursor semantics).
	NewPrimaryCursor string

	// LocalChanged is true when any mkdir / rename / archive mutation
	// happened, hinting that the caller should re-Walk the local tree
	// before running CompareIncremental on file-level events.
	LocalChanged bool
}

// HandleIncrementalDirEvents applies the hybrid strategy from ADR 0040
// §操作×スコープマトリクス to every is_dir entry in `changes`. It may
// mutate `archive` (rename keys, delete entries) and the local file
// system (os.MkdirAll, os.Rename). File-level changes are left untouched
// for CompareIncremental to process afterwards.
//
// The returned plans are in addition to whatever CompareIncremental
// produces. The caller is responsible for merging them — see
// MergePlansByPath for the dedup rule.
func HandleIncrementalDirEvents(
	c *client.Client,
	localDir string,
	localFiles map[string]types.LocalFile,
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
			// Empty-dir support. file-level pulls auto-create parents
			// via os.MkdirAll; this branch handles explicit mkdir events
			// for subtrees that have no files to pull.
			p := normalizeDirPath(ch.PathAfter)
			if p == "" {
				continue // scope root already exists
			}
			local := filepath.Join(localDir, filepath.FromSlash(p))
			if err := os.MkdirAll(local, 0755); err != nil {
				return nil, fmt.Errorf("mkdir %s: %w", p, err)
			}
			outcome.LocalChanged = true

		case "delete":
			// Archive prefix walk. `delete /` (scope-wide) uses an empty
			// prefix which matches every archive entry.
			prefix := normalizeDirPrefix(ch.PathBefore)
			plans := expandArchiveDelete(archive, localDir, prefix)
			outcome.ExtraPlans = append(outcome.ExtraPlans, plans...)

		case "move":
			fromPrefix := normalizeDirPrefix(ch.PathBefore)
			toPrefix := normalizeDirPrefix(ch.PathAfter)
			plans, localMutated, err := expandArchiveMove(
				archive, localDir, fromPrefix, toPrefix,
			)
			if err != nil {
				return nil, fmt.Errorf("move %s → %s: %w", fromPrefix, toPrefix, err)
			}
			outcome.ExtraPlans = append(outcome.ExtraPlans, plans...)
			if localMutated {
				outcome.LocalChanged = true
			}

		case "put":
			// Snapshot fetch + Compare() scoped to the put path. Empty /
			// "/" paths trigger a scope-root snapshot that also replaces
			// the primary cursor.
			isRoot := ch.PathAfter == "" || ch.PathAfter == "/"
			snapshotPath := ""
			if !isRoot {
				snapshotPath = ch.PathAfter
			}
			remote, cursor, err := FetchSnapshotAsRemoteFiles(c, snapshotPath)
			if err != nil {
				if err == client.ErrNotFound {
					// Race: the put subtree was gone by the time we
					// asked for it. Next poll will deliver a delete for
					// the same prefix and the archive walk will clean up.
					continue
				}
				return nil, fmt.Errorf("snapshot %s: %w", snapshotPath, err)
			}

			prefix := normalizeDirPrefix(ch.PathAfter)
			localSub := filterByPrefix(localFiles, prefix)
			archiveSub := filterByPrefix(archive, prefix)
			plans := Compare(localSub, remote, archiveSub)
			outcome.ExtraPlans = append(outcome.ExtraPlans, plans...)
			if isRoot {
				outcome.NewPrimaryCursor = cursor
			}
		}
	}
	return outcome, nil
}

// MergePlansByPath returns the union of the two plan lists with last-
// writer-wins dedup by path. Caller order matters: pass dir-event plans
// LAST so that snapshot-derived decisions override the incremental
// CompareIncremental plan for the same path (the snapshot reflects a
// more recent atomic state).
func MergePlansByPath(incremental, dirEvents []types.SyncPlan) []types.SyncPlan {
	byPath := make(map[string]types.SyncPlan, len(incremental)+len(dirEvents))
	for _, p := range incremental {
		byPath[p.Path] = p
	}
	for _, p := range dirEvents {
		byPath[p.Path] = p
	}
	out := make([]types.SyncPlan, 0, len(byPath))
	for _, p := range byPath {
		out = append(out, p)
	}
	// Deterministic order for humans.
	sortPlansByPath(out)
	return out
}

// --- helpers ---------------------------------------------------------------

// normalizeDirPath strips a leading "/" and any trailing "/" so the
// result matches the slash-free, no-trailing-slash convention used by
// archive keys and Walk output.
func normalizeDirPath(p string) string {
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, "/")
	return p
}

// normalizeDirPrefix returns a prefix that can be used with
// strings.HasPrefix against slash-free archive keys. Empty input
// (scope-root) returns "", which matches everything.
func normalizeDirPrefix(p string) string {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return ""
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// expandArchiveDelete walks the archive for entries under `prefix` and
// returns DeleteLocal plans for each one, upgrading to Conflict when
// the local hash no longer matches the archive (user edited since the
// last sync). An empty prefix is scope-wide (delete-all).
func expandArchiveDelete(
	archive map[string]types.FileState,
	localDir, prefix string,
) []types.SyncPlan {
	var plans []types.SyncPlan
	for path, fs := range archive {
		if prefix != "" && !strings.HasPrefix(path, prefix) {
			continue
		}
		localPath := filepath.Join(localDir, filepath.FromSlash(path))
		localHash, err := hashFile(localPath)
		action := types.DeleteLocal
		if err == nil && localHash != fs.LocalHash {
			action = types.Conflict
		}
		plans = append(plans, types.SyncPlan{Path: path, Action: action})
	}
	return plans
}

// expandArchiveMove renames archive keys and local files from `from`
// prefix to `to` prefix. Files whose local hash has drifted from the
// archive are left in place and surfaced as Conflict plans for the
// executor to handle. Returns whether the local tree was mutated so
// the caller knows to re-Walk.
func expandArchiveMove(
	archive map[string]types.FileState,
	localDir, fromPrefix, toPrefix string,
) ([]types.SyncPlan, bool, error) {
	// Collect matches first so we don't mutate while iterating.
	type move struct {
		oldKey string
		newKey string
	}
	var moves []move
	for path := range archive {
		if fromPrefix != "" && !strings.HasPrefix(path, fromPrefix) {
			continue
		}
		if fromPrefix == "" {
			// Shouldn't happen: move with empty fromPrefix would mean
			// moving the scope root itself, which the server rewrites
			// as delete / + put / (see ADR 0040 §操作×スコープマトリクス).
			continue
		}
		newKey := toPrefix + strings.TrimPrefix(path, fromPrefix)
		moves = append(moves, move{oldKey: path, newKey: newKey})
	}

	var plans []types.SyncPlan
	mutated := false
	for _, m := range moves {
		fs := archive[m.oldKey]
		oldLocal := filepath.Join(localDir, filepath.FromSlash(m.oldKey))
		newLocal := filepath.Join(localDir, filepath.FromSlash(m.newKey))

		// If the file is missing locally we can still rekey the archive
		// so a future local re-create is treated correctly. Treat as a
		// normal move (no rename needed).
		localHash, hashErr := hashFile(oldLocal)
		if hashErr != nil && !os.IsNotExist(hashErr) {
			return nil, mutated, fmt.Errorf("hash %s: %w", m.oldKey, hashErr)
		}
		if hashErr == nil && localHash != fs.LocalHash {
			// Local diverged from archive since last sync — don't rename,
			// let the executor surface a conflict on the old path.
			plans = append(plans, types.SyncPlan{
				Path:   m.oldKey,
				Action: types.Conflict,
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
		delete(archive, m.oldKey)
		archive[m.newKey] = fs
	}
	return plans, mutated, nil
}

// filterByPrefix returns a new map limited to keys matching `prefix`.
// Empty prefix returns a shallow copy (matches everything).
func filterByPrefix[T any](m map[string]T, prefix string) map[string]T {
	out := make(map[string]T)
	for k, v := range m {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			out[k] = v
		}
	}
	return out
}
