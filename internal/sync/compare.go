package sync

import (
	"sort"

	"github.com/selfbase-dev/s2-cli/internal/types"
)

// Compare performs three-way comparison between local, remote, and archive states.
// Returns a list of sync plans sorted by path.
//
// For initial sync (archive is nil or empty), local wins on conflicts:
// - Same hash → no-op
// - Different hash → conflict (local wins, remote retired as .sync-conflict-*)
// - Only local → push
// - Only remote → pull
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

		action := classify(l, hasLocal, r, hasRemote, a, hasArchive)
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
	r types.RemoteFile, hasRemote bool,
	a types.FileState, hasArchive bool,
) types.SyncAction {
	if !hasArchive {
		// Initial sync or new file
		return classifyNoArchive(l, hasLocal, r, hasRemote)
	}

	localChanged := hasLocal && l.Hash != a.LocalHash
	localDeleted := !hasLocal
	remoteChanged := hasRemote && r.ETag != a.RemoteETag
	remoteDeleted := !hasRemote

	switch {
	// Both present, neither changed
	case hasLocal && hasRemote && !localChanged && !remoteChanged:
		return types.NoOp

	// Only local changed
	case localChanged && !remoteChanged && !remoteDeleted:
		return types.Push

	// Only remote changed
	case !localChanged && !localDeleted && remoteChanged:
		return types.Pull

	// Both changed
	case localChanged && remoteChanged:
		return types.Conflict

	// Local new (shouldn't happen with archive, but handle gracefully)
	case hasLocal && !hasRemote && !hasArchive:
		return types.Push

	// Local deleted, remote unchanged
	case localDeleted && !remoteChanged && hasRemote:
		return types.DeleteRemote

	// Remote deleted, local unchanged
	case !localChanged && remoteDeleted && hasLocal:
		return types.DeleteLocal

	// Local deleted, remote changed
	case localDeleted && remoteChanged:
		return types.Conflict

	// Remote deleted, local changed
	case localChanged && remoteDeleted:
		return types.Conflict

	// Both deleted
	case localDeleted && remoteDeleted:
		return types.NoOp

	default:
		return types.NoOp
	}
}

func classifyNoArchive(
	l types.LocalFile, hasLocal bool,
	r types.RemoteFile, hasRemote bool,
) types.SyncAction {
	switch {
	case hasLocal && !hasRemote:
		return types.Push
	case !hasLocal && hasRemote:
		return types.Pull
	case hasLocal && hasRemote:
		// Both exist: compare hash
		if l.Hash == r.ETag {
			return types.NoOp
		}
		// Local wins (ADR 0009: initial sync = local priority)
		return types.Conflict
	default:
		return types.NoOp
	}
}
