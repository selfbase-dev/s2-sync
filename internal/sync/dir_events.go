// dir_events.go — Hybrid strategy for is_dir change events (ADR 0040).
//
// The server emits directory-level events for bulk operations: delete a
// subtree, move a subtree, restore from trash, scope-external put, etc.
// The CLI handles each event in one of four ways:
//
//   1. Archive walk — delete / move within scope. Walk the archive +
//      local filesystem under the prefix, hash-check every touched
//      file against its current local copy, and emit DeleteLocal /
//      PreserveLocalRename plans (delete) or perform os.Rename in
//      place (move). No network traffic.
//
//   2. Full bootstrap — scope-root put (ancestor enter, restore, etc.).
//      Runs the ADR 0046 bootstrap protocol (pin S0, fetch via
//      Snapshot or ListDir on 413, converge). The converged cursor
//      replaces the primary cursor. Any earlier plans in the same
//      batch are cleared because the bootstrap result is authoritative
//      for the entire scope.
//
//   3. Subtree fetch — non-root put dir. Tries /api/snapshot first;
//      falls back to ListDir on 413 (weaker consistency, corrected
//      on next sync cycle). Does NOT replace the primary cursor.
//
//   4. os.MkdirAll — mkdir events. Pure local side effect, no plans.

package sync

import (
	"errors"
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

// SubtreeSnapshot carries a remote file map (from Snapshot, ListDir
// fallback, or Bootstrap) that the caller must Compare() AFTER
// re-walking the local tree. Prefix is the slash-terminated client-
// relative path, or "" for a scope-root fetch.
type SubtreeSnapshot struct {
	Prefix string
	Remote map[string]types.RemoteFile
}

// DirEventOutcome carries the results of hybrid-strategy processing.
type DirEventOutcome struct {
	// ArchiveWalkPlans are the file-level plans derived from delete /
	// move archive walks. They are already computed against the local
	// tree and are safe to merge directly with CompareIncremental
	// output.
	ArchiveWalkPlans []types.SyncPlan

	// SubtreeSnapshots carry snapshot responses that still need to be
	// compared against the LOCAL STATE. The caller runs Compare() on
	// each one AFTER the post-side-effects re-walk — see cmd/sync.go
	// for the canonical flow.
	SubtreeSnapshots []SubtreeSnapshot

	// NewPrimaryCursor, when non-empty, is the cursor returned by a
	// scope-root snapshot. The caller must use it as the new primary
	// cursor instead of advancing via /api/changes. Subtree snapshots
	// leave this empty (ADR 0040 §cursor semantics).
	NewPrimaryCursor string

	// LocalChanged is true when any mkdir / rename / archive mutation
	// happened, signalling the caller to re-Walk the local tree
	// before running CompareIncremental / the per-subtree Compare.
	LocalChanged bool
}

// HandleIncrementalDirEvents applies the hybrid strategy from ADR 0040
// §操作×スコープマトリクス to every is_dir entry in `changes`. It may
// mutate `archive` (rekey, delete) and the local file system (mkdir,
// rename). File-level changes are left untouched for CompareIncremental
// to process afterwards.
//
// Importantly this function NEVER calls Compare() on subtree snapshots;
// that's deferred to the caller so that Compare observes a post-side-
// effect, post-re-walk view of the local tree (codex blocker #2).
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
			// Empty-dir support. file-level pulls auto-create parents
			// via os.MkdirAll; this branch handles explicit mkdir
			// events for subtrees that have no files to pull.
			p, err := safeRelPrefix(ch.PathAfter)
			if err != nil {
				return nil, fmt.Errorf("mkdir %s: %w", ch.PathAfter, err)
			}
			if p == "" {
				continue // scope root always exists
			}
			// Reuse safeJoin by asking it to resolve a dummy file and
			// then Dir-ing it. Simpler: just trim the trailing slash
			// and pass through safeJoin.
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
				// Scope-root put = re-bootstrap (ADR 0040 §cursor
				// semantics). Bootstrap handles 413→ListDir fallback
				// and runs the convergence protocol (ADR 0046) so the
				// returned cursor is safe to adopt as primary.
				//
				// Clear earlier plans from this batch: the bootstrap
				// result is authoritative for the entire scope, so
				// delete/move plans that preceded this put are stale.
				// Note: preceding moves may have rekeyed archive and
				// renamed local files — those side effects remain, but
				// Compare against the bootstrap's remote map still
				// produces correct plans (moved files without remote
				// match get DeleteLocal, missing files get Pull).
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
				// Subtree put: FetchRemoteMap tries Snapshot first and
				// falls back to ListDir on 413 (ADR 0046 designed the
				// split-fetch for bootstrap; reuse here accepts weaker
				// consistency — ListDir reads are non-atomic across
				// directories). Correctness relies on:
				//   1. Primary cursor is NOT replaced (advances via
				//      resp.NextCursor), so missed files appear as
				//      change events on the next poll.
				//   2. Archive records what was applied, so duplicates
				//      from a slightly-ahead ListDir become NoOps.
				//   3. Compare + archive prevents overwriting local
				//      edits regardless of remote map staleness.
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

// SubtreeComparePlans runs Compare() against each snapshot in the
// outcome, using the POST-re-walk local state and archive. Must be
// called AFTER the caller re-walks the local tree on LocalChanged.
//
// Passing `localFiles` and `archive` explicitly (rather than keeping
// them on the outcome) forces the caller to re-provide them, making
// the "compare after re-walk" invariant hard to miss.
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

// MergePlansByPath returns the union of plan lists with last-writer-
// wins dedup by path. Caller order matters: pass authoritative plans
// LAST. For the incremental sync flow we use:
//
//	MergePlansByPath(fileLevelPlans, subtreeComparePlans, archiveWalkPlans)
//
// archive walk plans win the final tiebreak because a dir-delete that
// produced a DeleteLocal/PreserveLocalRename must NOT be overridden by
// a stale Push from a file-level change entry that referenced a path
// inside the deleted subtree.
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

// --- helpers ---------------------------------------------------------------

// expandArchiveDelete walks BOTH the archive and the local filesystem
// for entries under `prefix` and returns DeleteLocal plans for clean
// files (or PreserveLocalRename when the local hash has drifted from
// the archive record). An empty prefix means scope-wide.
//
// Walking the filesystem in addition to the archive fixes codex
// blocker #3: a local-only file under the deleted prefix (not tracked
// in archive) would otherwise be picked up by CompareIncremental as
// "local new" and pushed back, resurrecting the subtree the server
// just removed.
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
			// Local diverged — save it as a conflict copy instead of
			// deleting the user's edits.
			action = types.PreserveLocalRename
		}
		plans = append(plans, types.SyncPlan{Path: path, Action: action})
	}

	// Walk the local filesystem too to catch local-only files under
	// the deleted prefix.
	fsPaths, err := walkLocalUnderPrefix(localDir, prefix)
	if err != nil {
		return nil, err
	}
	for _, path := range fsPaths {
		if _, already := seen[path]; already {
			continue
		}
		// Local-only (not in archive). Untracked new work under a
		// directory the server has deleted. We preserve it as a
		// conflict copy so no user bytes are lost and the subtree
		// doesn't get resurrected on the next Push.
		plans = append(plans, types.SyncPlan{
			Path:   path,
			Action: types.PreserveLocalRename,
		})
	}
	return plans, nil
}

// expandArchiveMove renames archive keys and local files from `from`
// prefix to `to` prefix. Files whose local hash has drifted from the
// archive are left in place as PreserveLocalRename conflicts, while
// local-only descendants (not tracked in archive) are moved too — so
// they aren't re-pushed at the old path and don't resurrect the old
// subtree (codex blocker #3).
func expandArchiveMove(
	archive map[string]types.FileState,
	localDir, fromPrefix, toPrefix string,
) ([]types.SyncPlan, bool, error) {
	// Shouldn't happen: move with empty fromPrefix would mean moving
	// the scope root, which the server rewrites as delete / + put /.
	if fromPrefix == "" {
		return nil, false, nil
	}

	// Collect matches first so we don't mutate while iterating.
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
			// Divergent — preserve as conflict, keep archive as-is so
			// the old path no longer looks tracked next round.
			plans = append(plans, types.SyncPlan{
				Path:   m.oldKey,
				Action: types.PreserveLocalRename,
			})
			delete(archive, m.oldKey)
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

	// Now walk the local filesystem for UNTRACKED descendants under
	// fromPrefix — move them too so they land at the new location
	// instead of being pushed back under the old one next round.
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

// walkLocalUnderPrefix enumerates regular-file relative paths under
// the given prefix. Empty prefix means scope-wide (the entire localDir
// minus .s2/). Paths are returned in the slash-free / forward-slash
// convention used by archive keys and Walk().
func walkLocalUnderPrefix(localDir, prefix string) ([]string, error) {
	// Resolve the filesystem root of the walk.
	var walkRoot string
	if prefix == "" {
		walkRoot = localDir
	} else {
		// prefix already ends with "/"; strip for safeJoin
		var err error
		walkRoot, err = safeJoin(localDir, strings.TrimSuffix(prefix, "/"))
		if err != nil {
			return nil, err
		}
	}

	info, err := os.Stat(walkRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		// The prefix path exists but isn't a directory (e.g. a file
		// was created locally at the exact prefix path). Skip — there
		// are no descendants.
		return nil, nil
	}

	var out []string
	err = filepath.Walk(walkRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(localDir, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		// Skip .s2 state directory.
		if rel == ".s2" || strings.HasPrefix(rel, ".s2/") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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
