package sync

import (
	"fmt"
	"sort"
	"strings"

	"github.com/selfbase-dev/s2-sync/internal/client"
	"github.com/selfbase-dev/s2-sync/internal/types"
)

// SnapshotToRemoteFiles projects /api/v1/snapshot items into the
// file-centric map used by Compare / CompareIncremental. Directory
// items are filtered out — the dir lifecycle is owned by
// SnapshotToRemoteDirs so the file pipeline stays single-purpose.
//
// Paths are normalised to the slash-free keys used throughout the sync
// pipeline (stripping the leading "/" that the server adds via
// absolutePathToClient). Entries with traversal components, null bytes
// or absolute-path indicators are dropped with a warning — the executor
// would refuse to write them anyway, and we don't want a malicious
// snapshot to abort the whole sync.
func SnapshotToRemoteFiles(items []types.SnapshotItem) map[string]types.RemoteFile {
	out := make(map[string]types.RemoteFile, len(items))
	for _, it := range items {
		if it.Type != "file" {
			continue
		}
		key := strings.TrimPrefix(it.Path, "/")
		if !isSafeRelativePath(key) {
			fmt.Printf("warning: skipping unsafe snapshot path: %s\n", it.Path)
			continue
		}
		size := int64(0)
		if it.Size != nil {
			size = *it.Size
		}
		// Name is the final segment — used by a couple of display paths;
		// not semantically load-bearing because Compare keys on the map key.
		name := key
		if idx := strings.LastIndex(key, "/"); idx >= 0 {
			name = key[idx+1:]
		}
		out[key] = types.RemoteFile{
			Name:           name,
			Size:           size,
			Hash:           it.Hash,
			RevisionID:     it.RevisionID,
			ContentVersion: it.ContentVersion,
		}
	}
	return out
}

// SnapshotToRemoteDirs returns the set of directory paths present in
// a snapshot response. Used to materialize empty folders locally that
// would otherwise vanish: the file-centric pipeline implicitly creates
// dirs whose path is the parent of a file via os.MkdirAll, but a live
// directory row with no file children (the user just clicked
// "new folder" on the web UI) has no file to ride into existence on.
//
// Paths use the same canonical form as SnapshotToRemoteFiles: leading
// "/" stripped, NFC, traversal-unsafe entries dropped. Returns a sorted
// slice so callers (executor plans) get deterministic order; parents
// come before children which lets a single MkdirAll create the chain.
func SnapshotToRemoteDirs(items []types.SnapshotItem) []string {
	var out []string
	for _, it := range items {
		if it.Type != "dir" {
			continue
		}
		key := strings.TrimPrefix(it.Path, "/")
		key = strings.TrimSuffix(key, "/")
		if key == "" {
			// Scope root — never emit MkdirLocal for it. The sync root
			// already exists; the user wouldn't expect us to recreate
			// it if it's missing.
			continue
		}
		if !isSafeRelativePath(key) {
			fmt.Printf("warning: skipping unsafe snapshot dir path: %s\n", it.Path)
			continue
		}
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// PrefillArchiveForIdempotentApply populates `archive` with FileState
// entries for files whose local hash already matches the server hash
// from a fresh snapshot. Those entries let Compare short-circuit to
// NoOp instead of producing a Conflict plan that would force a
// download-and-compare round-trip in the executor (ADR 0040 §conflict
// 検出 — idempotent apply via hash).
//
// The function only ADDS entries. It never overwrites a pre-existing
// archive entry, so a stale archive row from a previous sync still
// triggers the normal compare logic and lets the executor resolve the
// stale state. Returns the number of entries added for observability.
func PrefillArchiveForIdempotentApply(
	state *State,
	localFiles map[string]types.LocalFile,
	remoteFiles map[string]types.RemoteFile,
) int {
	if state == nil {
		return 0
	}
	added := 0
	for path, l := range localFiles {
		if _, exists := state.Files[path]; exists {
			continue
		}
		r, ok := remoteFiles[path]
		if !ok {
			continue
		}
		if r.Hash == "" || l.Hash != r.Hash {
			continue
		}
		state.RecordFile(path, l.Hash, r.ContentVersion, r.RevisionID)
		added++
	}
	return added
}

// FetchSnapshotAsRemoteFiles is a convenience wrapper that runs a
// scope-root snapshot and returns the projection plus the paired
// cursor. The cursor is atomic with the items and must be used as the
// primary cursor after a successful bootstrap (ADR 0040 §cursor
// semantics: bootstrap / 410 recovery → 主 cursor 差し替え).
func FetchSnapshotAsRemoteFiles(c *client.Client, path string) (map[string]types.RemoteFile, string, error) {
	resp, err := c.Snapshot(path)
	if err != nil {
		return nil, "", err
	}
	return SnapshotToRemoteFiles(resp.Items), resp.Cursor, nil
}

// FetchSnapshotSplit returns both the file map and the live directory
// list from a snapshot in a single round-trip. The split lets the
// caller drive os.MkdirAll for dir-only nodes (empty folders the user
// created on the web UI) without conflating them with file pulls.
//
// Returns a zero-value cursor only when the server response itself
// carries one — the file-only sibling (FetchSnapshotAsRemoteFiles)
// keeps its existing contract for callers that don't yet need dir
// materialization.
func FetchSnapshotSplit(c *client.Client, path string) (map[string]types.RemoteFile, []string, string, error) {
	resp, err := c.Snapshot(path)
	if err != nil {
		return nil, nil, "", err
	}
	return SnapshotToRemoteFiles(resp.Items), SnapshotToRemoteDirs(resp.Items), resp.Cursor, nil
}
