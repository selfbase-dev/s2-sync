package sync

import (
	"fmt"
	"sort"
	"strings"

	"github.com/selfbase-dev/s2-cli/internal/types"
)

func sortPlansByPath(plans []types.SyncPlan) {
	sort.Slice(plans, func(i, j int) bool { return plans[i].Path < plans[j].Path })
}

// Compare performs three-way comparison between local, remote, and archive states.
// Returns a list of sync plans sorted by path.
//
// For initial sync (archive is nil or empty), both sides are compared by hash
// (download required to get remote hash). Same hash → no-op, different → conflict.
func Compare(
	local map[string]types.LocalFile,
	remote map[string]types.RemoteFile,
	archive map[string]types.FileState,
) []types.SyncPlan {
	if archive == nil {
		archive = make(map[string]types.FileState)
	}

	// Collect all paths
	paths := make(map[string]struct{})
	for p := range local {
		paths[p] = struct{}{}
	}
	for p := range remote {
		paths[p] = struct{}{}
	}
	for p := range archive {
		paths[p] = struct{}{}
	}

	var plans []types.SyncPlan

	for path := range paths {
		l, hasLocal := local[path]
		r, hasRemote := remote[path]
		a, hasArchive := archive[path]

		action := classify(l, hasLocal, hasRemote, a, hasArchive)
		if action != types.NoOp {
			// Populate RevisionID so executor.executePull can use the
			// race-free /api/revisions/:id fetch path when it is available.
			// Only the snapshot-backed caller sets RemoteFile.RevisionID;
			// the legacy ListAllRecursive path leaves it empty and the
			// executor falls back to /api/files/:path.
			plans = append(plans, types.SyncPlan{
				Path:       path,
				Action:     action,
				RevisionID: r.RevisionID,
				Hash:       r.Hash,
			})
		}
	}

	sortPlansByPath(plans)

	return plans
}

// CompareIncremental builds sync plans from local changes + remote change entries.
// Used for incremental sync (cursor-based).
func CompareIncremental(
	local map[string]types.LocalFile,
	archive map[string]types.FileState,
	remoteChanges []types.ChangeEntry,
) []types.SyncPlan {
	if archive == nil {
		archive = make(map[string]types.FileState)
	}

	// Build set of remotely changed paths and their actions.
	// Skip directory events (mkdir) — we only sync files; directories are
	// created implicitly when pulling files into them.
	// normPath strips the leading "/" that the server's absolutePathToClient adds
	// so change paths match the slash-free keys used by Walk and ListAllRecursive.
	normPath := func(p string) string { return strings.TrimPrefix(p, "/") }

	// safeKey is normPath + defence-in-depth traversal filter. Unsafe
	// paths (traversal, null bytes, empty) are dropped with a warning —
	// the executor would refuse them via safeJoin anyway, and filtering
	// here keeps a single bad change entry from derailing the whole
	// sync batch.
	safeKey := func(p string) (string, bool) {
		k := normPath(p)
		if !isSafeRelativePath(k) {
			fmt.Printf("warning: skipping unsafe remote path: %s\n", p)
			return "", false
		}
		return k, true
	}

	remoteChanged := make(map[string]types.ChangeEntry)
	for _, ch := range remoteChanges {
		// Callers are expected to pre-split dir events out and route
		// them through HandleIncrementalDirEvents (ADR 0040 hybrid
		// strategy). Any is_dir entry that slips through here is
		// dropped so CompareIncremental stays file-level.
		if ch.IsDir {
			continue
		}
		switch ch.Action {
		case "put":
			if ch.PathAfter != "" {
				if k, ok := safeKey(ch.PathAfter); ok {
					remoteChanged[k] = ch
				}
			}
		case "delete":
			if ch.PathBefore != "" {
				if k, ok := safeKey(ch.PathBefore); ok {
					remoteChanged[k] = ch
				}
			}
		case "move":
			// move = delete at path_before + put at path_after
			if ch.PathBefore != "" {
				if k, ok := safeKey(ch.PathBefore); ok {
					remoteChanged[k] = types.ChangeEntry{Action: "delete", PathBefore: ch.PathBefore}
				}
			}
			if ch.PathAfter != "" {
				if k, ok := safeKey(ch.PathAfter); ok {
					remoteChanged[k] = ch
				}
			}
		}
	}

	// Collect all relevant paths
	paths := make(map[string]struct{})
	for p := range local {
		if a, ok := archive[p]; ok {
			l := local[p]
			if l.Hash != a.LocalHash {
				paths[p] = struct{}{} // local changed
			}
		} else {
			paths[p] = struct{}{} // local new
		}
	}
	for p := range archive {
		if _, ok := local[p]; !ok {
			paths[p] = struct{}{} // local deleted
		}
	}
	for p := range remoteChanged {
		paths[p] = struct{}{}
	}

	var plans []types.SyncPlan

	for path := range paths {
		l, hasLocal := local[path]
		a, hasArchive := archive[path]
		rch, hasRemoteChange := remoteChanged[path]

		localChanged := hasLocal && hasArchive && l.Hash != a.LocalHash
		localNew := hasLocal && !hasArchive
		localDeleted := !hasLocal && hasArchive
		remoteDeleted := hasRemoteChange && rch.Action == "delete"
		remotePut := hasRemoteChange && (rch.Action == "put" || rch.Action == "move")

		var action types.SyncAction

		switch {
		// Both sides changed/created
		case localChanged && remotePut:
			action = types.Conflict
		case localChanged && remoteDeleted:
			action = types.Conflict
		case localDeleted && remotePut:
			action = types.Conflict
		case localNew && remotePut:
			action = types.Conflict // both sides created same path independently

		// Only local changed
		case localChanged && !hasRemoteChange:
			action = types.Push
		case localNew && !hasRemoteChange:
			action = types.Push
		case localDeleted && !hasRemoteChange:
			action = types.DeleteRemote

		// Only remote changed
		case !localChanged && !localNew && !localDeleted && remotePut:
			action = types.Pull
		case !localChanged && !localNew && !localDeleted && remoteDeleted:
			action = types.DeleteLocal

		// Both deleted — must clean archive entry to prevent DeleteRemote re-firing.
		// DeleteLocal handler tolerates IsNotExist so this is safe when file is gone.
		case localDeleted && remoteDeleted:
			action = types.DeleteLocal

		default:
			action = types.NoOp
		}

		if action != types.NoOp {
			// For Pull / Conflict plans, thread the revision id from the
			// change entry so the executor can fetch it race-free. delete /
			// move events don't carry a new revision id and we leave it
			// empty — the executor won't use it for those actions.
			plans = append(plans, types.SyncPlan{
				Path:       path,
				Action:     action,
				RevisionID: rch.RevisionID,
				Hash:       rch.Hash,
			})
		}
	}

	sortPlansByPath(plans)

	return plans
}

func classify(
	l types.LocalFile, hasLocal bool,
	hasRemote bool,
	a types.FileState, hasArchive bool,
) types.SyncAction {
	if !hasArchive {
		return classifyNoArchive(hasLocal, hasRemote)
	}

	localChanged := hasLocal && l.Hash != a.LocalHash
	localDeleted := !hasLocal
	// For full sync compare, we don't have remote content_version.
	// Remote "changed" is detected by comparing content_version in archive vs current.
	// Since full sync doesn't have current content_version from list API,
	// we use this only for archive-based detection:
	// if remote is present but archive exists, we assume remote is unchanged
	// (incremental sync via changes API handles remote changes).
	remoteDeleted := !hasRemote

	switch {
	case hasLocal && hasRemote && !localChanged:
		return types.NoOp
	case localChanged && hasRemote:
		return types.Push
	case localDeleted && hasRemote:
		return types.DeleteRemote
	case hasLocal && remoteDeleted && !localChanged:
		return types.DeleteLocal
	case localChanged && remoteDeleted:
		return types.Conflict
	case localDeleted && remoteDeleted:
		// Clean archive entry; DeleteLocal handler tolerates IsNotExist.
		return types.DeleteLocal
	default:
		return types.NoOp
	}
}

func classifyNoArchive(hasLocal, hasRemote bool) types.SyncAction {
	switch {
	case hasLocal && !hasRemote:
		return types.Push
	case !hasLocal && hasRemote:
		return types.Pull
	case hasLocal && hasRemote:
		// Both exist but no archive: need to download and compare hash.
		// The executor handles this — here we signal "needs comparison".
		return types.Conflict
	default:
		return types.NoOp
	}
}

// HasLocalChanges reports whether any local file has changed (added,
// modified, or deleted) relative to the archive.
func HasLocalChanges(local map[string]types.LocalFile, archive map[string]types.FileState) bool {
	for path, l := range local {
		if a, ok := archive[path]; !ok || l.Hash != a.LocalHash {
			return true
		}
	}
	for path := range archive {
		if _, ok := local[path]; !ok {
			return true
		}
	}
	return false
}
