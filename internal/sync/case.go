// Package sync: case-sensitivity and Unicode normalization.
//
// All logic for absorbing OS-level differences in filename handling
// (macOS NFD vs NFC, case-insensitive vs case-sensitive filesystems,
// case-only renames) is isolated in this file. The main sync pipeline
// (walk, compare, executor) treats case/Unicode as a black box:
// normalize at ingest, detect collisions/renames at analysis, execute
// via existing I/O layers.
//
// Functions here are pure — no I/O, no state. Side effects live in the
// walk / executor layers — keep detection and I/O separate.
package sync

import (
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/selfbase-dev/s2-sync/internal/types"
)

// NormalizePath returns the canonical form used for archive keys,
// comparisons, and sorting: TrimPrefix("/") → forward slashes → NFC.
//
// This is idempotent: NormalizePath(NormalizePath(x)) == NormalizePath(x).
// Idempotence is critical — the archive stores NormalizePath results and
// walk re-normalizes on every sync; if this weren't stable, repeated
// syncs could oscillate.
func NormalizePath(p string) string {
	p = strings.TrimPrefix(p, "/")
	p = filepath.ToSlash(p)
	return norm.NFC.String(p)
}

// FoldKey returns a case-insensitive key for collision detection:
// NormalizePath + Unicode-aware lowercase.
//
// Paths with the same FoldKey are indistinguishable on case-insensitive
// filesystems (Mac, Windows). Used to group collisions (DetectCollisions)
// and detect case-only renames (DetectMovePairs).
//
// Limitation: strings.ToLower uses simple Unicode case mapping.
// Locale-specific folds (Turkish dotless-I, Greek final sigma) may miss
// detection — acceptable tradeoff (fallback is delete+create
// which is correct, just wasteful).
func FoldKey(p string) string {
	return strings.ToLower(NormalizePath(p))
}

// DetectCollisions groups paths by FoldKey. Groups with >1 member are
// collisions that the caller must handle (typically skip + warning
//, not stop the whole sync).
//
// Returned paths are NormalizePath-canonical and sorted UTF-8 bytewise
// lexicographic within each group — this determinism is what makes
// tie-break stable across devices and invocations (deterministic tie-break).
func DetectCollisions(paths []string) map[string][]string {
	groups := make(map[string][]string)
	for _, p := range paths {
		canonical := NormalizePath(p)
		key := FoldKey(p)
		groups[key] = append(groups[key], canonical)
	}
	for k := range groups {
		sort.Strings(groups[k])
	}
	return groups
}

// MoveCandidate describes a detected case-only rename: archive had From,
// current walk has To, they fold to the same key, and the content hash
// matches. Emitted as SyncAction.Move and executed via server MOVE API.
type MoveCandidate struct {
	From string // source path (archive side)
	To   string // destination path (walk side)
	Hash string // shared content hash
}

// DetectMovePairs finds case-only renames by pairing disappeared archive
// entries with new local entries sharing the same FoldKey and content hash.
//
// Only unambiguous 1:1 pairs are emitted — if multiple archive entries
// or multiple new local entries fold to the same key, the situation is
// ambiguous and we let it decompose to delete+create (the pipeline
// already handles that correctly).
//
// Arguments map path → hash. Callers extract hashes from their domain
// types (types.LocalFile, types.FileState) before invoking.
//
// Returned pairs are sorted by From (UTF-8 bytewise) for deterministic
// execution order.
func DetectMovePairs(local, archive map[string]string) []MoveCandidate {
	archiveLost := make(map[string][]string) // foldKey → archive paths absent from local
	for p := range archive {
		if _, ok := local[p]; ok {
			continue
		}
		archiveLost[FoldKey(p)] = append(archiveLost[FoldKey(p)], p)
	}

	localNew := make(map[string][]string) // foldKey → local paths absent from archive
	for p := range local {
		if _, ok := archive[p]; ok {
			continue
		}
		localNew[FoldKey(p)] = append(localNew[FoldKey(p)], p)
	}

	var pairs []MoveCandidate
	for key, fromList := range archiveLost {
		toList, ok := localNew[key]
		if !ok || len(fromList) != 1 || len(toList) != 1 {
			continue
		}
		from, to := fromList[0], toList[0]
		if from == to {
			continue
		}
		if local[to] != archive[from] {
			continue
		}
		pairs = append(pairs, MoveCandidate{From: from, To: to, Hash: local[to]})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].From < pairs[j].From })
	return pairs
}

// NormalizeRemoteMap canonicalizes remote map keys (NFC) and, on
// case-insensitive local FS, collapses case collisions — keeping the
// lexicographic-first path (deterministic tie-break key
// concept 5). The losers are returned as CollisionGroups for reporting
// and emission as SkipCaseConflict plans.
//
// On case-sensitive local FS, no filtering happens — all paths pass
// through (Linux users keep both `File.txt` and `file.txt`).
func NormalizeRemoteMap(remote map[string]types.RemoteFile, caseInsensitive bool) (
	filtered map[string]types.RemoteFile,
	collisions []CollisionGroup,
) {
	filtered = make(map[string]types.RemoteFile, len(remote))
	if !caseInsensitive {
		for k, v := range remote {
			filtered[NormalizePath(k)] = v
		}
		return filtered, nil
	}

	// Case-insensitive: bucket by FoldKey, keep lex-first.
	type entry struct {
		canonical string
		file      types.RemoteFile
	}
	buckets := make(map[string][]entry)
	for k, v := range remote {
		canonical := NormalizePath(k)
		key := FoldKey(k)
		buckets[key] = append(buckets[key], entry{canonical: canonical, file: v})
	}
	for key, es := range buckets {
		sort.Slice(es, func(i, j int) bool { return es[i].canonical < es[j].canonical })
		// If duplicates exist under the exact same canonical path (same
		// remote path appearing twice in input, shouldn't happen but
		// defensive) collapse silently — all carry the same content.
		filtered[es[0].canonical] = es[0].file
		if len(es) > 1 {
			paths := make([]string, 0, len(es))
			seen := make(map[string]struct{}, len(es))
			for _, e := range es {
				if _, ok := seen[e.canonical]; ok {
					continue
				}
				seen[e.canonical] = struct{}{}
				paths = append(paths, e.canonical)
			}
			if len(paths) > 1 {
				collisions = append(collisions, CollisionGroup{Key: key, Paths: paths})
			}
		}
	}
	sort.Slice(collisions, func(i, j int) bool { return collisions[i].Key < collisions[j].Key })
	return filtered, collisions
}

// encodeCollisionKeys serializes a sorted set of FoldKeys into the
// newline-separated form stored in state_meta.collision_keys.
func encodeCollisionKeys(keys []string) string {
	return strings.Join(uniqSorted(keys), "\n")
}

// parseCollisionKeys reverses encodeCollisionKeys. Blank storage → nil.
func parseCollisionKeys(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// uniqSorted returns a deduplicated, sorted copy of keys (UTF-8
// bytewise, stable tie-break rule).
func uniqSorted(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	cp := make([]string, len(keys))
	copy(cp, keys)
	sort.Strings(cp)
	out := cp[:0]
	prev := ""
	for i, k := range cp {
		if i > 0 && k == prev {
			continue
		}
		out = append(out, k)
		prev = k
	}
	return out
}

// diffSortedStrings returns elements in b not in a (added) and in a not
// in b (resolved). Both a and b must be sorted and deduplicated.
func diffSortedStrings(a, b []string) (added, resolved []string) {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			i++
			j++
		case a[i] < b[j]:
			resolved = append(resolved, a[i])
			i++
		default:
			added = append(added, b[j])
			j++
		}
	}
	resolved = append(resolved, a[i:]...)
	added = append(added, b[j:]...)
	return added, resolved
}

// MergeCaseOnlyRenames rewrites plans to collapse case-only rename
// pairs (DeleteRemote(P1) + Push(P2) where P1 was the archive name
// and P2 is the new walk name, sharing the same content hash and
// FoldKey) into a single Move plan executed via server MOVE API.
//
// This is the push-side counterpart of the pull-side move handling
// in CompareIncremental (which consumes changelog "move" events
// directly). Running this after Compare lets the pipeline detect
// renames even when the user ran a plain rename on disk (where no
// changelog event exists to decompose).
//
// Plans carrying paths that are part of a detected pair are removed
// and replaced by Move{Path: To, From: From}.
func MergeCaseOnlyRenames(
	plans []types.SyncPlan,
	local map[string]types.LocalFile,
	archive map[string]types.FileState,
) []types.SyncPlan {
	localHashes := make(map[string]string, len(local))
	for p, l := range local {
		localHashes[p] = l.Hash
	}
	archiveHashes := make(map[string]string, len(archive))
	for p, a := range archive {
		archiveHashes[p] = a.LocalHash
	}
	pairs := DetectMovePairs(localHashes, archiveHashes)
	if len(pairs) == 0 {
		return plans
	}

	consumed := make(map[string]bool, 2*len(pairs))
	for _, p := range pairs {
		consumed[p.From] = true
		consumed[p.To] = true
	}

	out := make([]types.SyncPlan, 0, len(plans)+len(pairs))
	for _, plan := range plans {
		if consumed[plan.Path] {
			continue
		}
		out = append(out, plan)
	}
	for _, p := range pairs {
		out = append(out, types.SyncPlan{
			Path:   p.To,
			From:   p.From,
			Action: types.Move,
			Hash:   p.Hash,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// NeutralizeLocalRemoteCaseCollisions replaces plans whose paths
// fold-collide with a different existing local/archive path (or with
// another plan) by SkipCaseConflict. This protects integrity on
// case-insensitive filesystems where pulling `File.txt` would
// silently overwrite an existing `file.txt` (same inode).
//
// A Push for an exact existing path is NOT a collision — we check
// the FoldKey bucket only when the exact paths differ.
//
// Only active when caseInsensitive is true. On case-sensitive FS,
// distinct exact paths can coexist.
func NeutralizeLocalRemoteCaseCollisions(
	plans []types.SyncPlan,
	local map[string]types.LocalFile,
	archive map[string]types.FileState,
	caseInsensitive bool,
) []types.SyncPlan {
	if !caseInsensitive {
		return plans
	}
	// foldKey → set of exact paths that already exist on disk or in
	// the archive (pre-sync state). Plans that target a different
	// exact path within the same bucket are unsafe.
	existing := make(map[string]map[string]struct{})
	add := func(p string) {
		key := FoldKey(p)
		if _, ok := existing[key]; !ok {
			existing[key] = make(map[string]struct{})
		}
		existing[key][NormalizePath(p)] = struct{}{}
	}
	for p := range local {
		add(p)
	}
	for p := range archive {
		add(p)
	}

	// Move / MoveApply are the resolution to a case collision, not
	// a new one — don't second-guess them. Same for SkipCaseConflict.
	// We neutralize only actions that would touch a case-insensitive
	// slot currently owned by a different exact path.
	vulnerable := func(a types.SyncAction) bool {
		switch a {
		case types.Push, types.Pull, types.DeleteLocal, types.DeleteRemote, types.Conflict:
			return true
		}
		return false
	}

	// Also track which exact paths the current plans intend to
	// remove, so a Push into a slot being freed by the same batch
	// is allowed through.
	removedByPlans := make(map[string]struct{})
	for _, p := range plans {
		switch p.Action {
		case types.DeleteLocal, types.DeleteRemote:
			removedByPlans[NormalizePath(p.Path)] = struct{}{}
		case types.Move, types.MoveApply:
			removedByPlans[NormalizePath(p.From)] = struct{}{}
		}
	}

	byFold := make(map[string][]int)
	for i, p := range plans {
		byFold[FoldKey(p.Path)] = append(byFold[FoldKey(p.Path)], i)
	}
	toSkip := make(map[int]bool)
	for key, idxs := range byFold {
		// Plan-vs-plan: >1 vulnerable plans targeting the same slot.
		vulnIdx := make([]int, 0, len(idxs))
		for _, idx := range idxs {
			if vulnerable(plans[idx].Action) {
				vulnIdx = append(vulnIdx, idx)
			}
		}
		if len(vulnIdx) > 1 {
			for _, idx := range vulnIdx {
				toSkip[idx] = true
			}
		}

		// Plan-vs-existing: vulnerable plan targets a slot already
		// occupied by a different exact path that is NOT being
		// removed by this batch.
		others, has := existing[key]
		if !has {
			continue
		}
		for _, idx := range vulnIdx {
			planPath := NormalizePath(plans[idx].Path)
			conflict := false
			for other := range others {
				if other == planPath {
					continue
				}
				if _, removed := removedByPlans[other]; removed {
					continue
				}
				conflict = true
				break
			}
			if conflict {
				toSkip[idx] = true
			}
		}
	}
	if len(toSkip) == 0 {
		return plans
	}
	out := make([]types.SyncPlan, 0, len(plans))
	for i, p := range plans {
		if toSkip[i] {
			p.Action = types.SkipCaseConflict
		}
		out = append(out, p)
	}
	return out
}
