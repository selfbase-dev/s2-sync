package sync

import (
	"sort"

	"github.com/selfbase-dev/s2-cli/internal/types"
)

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
		_, hasRemote := remote[path]
		a, hasArchive := archive[path]

		action := classify(l, hasLocal, hasRemote, a, hasArchive)
		if action != types.NoOp {
			plans = append(plans, types.SyncPlan{Path: path, Action: action})
		}
	}

	sort.Slice(plans, func(i, j int) bool {
		return plans[i].Path < plans[j].Path
	})

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
	remoteChanged := make(map[string]types.ChangeEntry)
	for _, ch := range remoteChanges {
		if ch.IsDir {
			continue // skip directory events
		}
		switch ch.Action {
		case "put":
			if ch.PathAfter != "" {
				remoteChanged[ch.PathAfter] = ch
			}
		case "delete":
			if ch.PathBefore != "" {
				remoteChanged[ch.PathBefore] = ch
			}
		case "move":
			// move = delete at path_before + put at path_after
			if ch.PathBefore != "" {
				remoteChanged[ch.PathBefore] = types.ChangeEntry{Action: "delete", PathBefore: ch.PathBefore}
			}
			if ch.PathAfter != "" {
				remoteChanged[ch.PathAfter] = ch
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

		// Both deleted
		case localDeleted && remoteDeleted:
			action = types.NoOp

		default:
			action = types.NoOp
		}

		if action != types.NoOp {
			plans = append(plans, types.SyncPlan{Path: path, Action: action})
		}
	}

	sort.Slice(plans, func(i, j int) bool {
		return plans[i].Path < plans[j].Path
	})

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
		return types.NoOp
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
