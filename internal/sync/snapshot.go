package sync

import (
	"fmt"
	"strings"
	"time"

	"github.com/selfbase-dev/s2-cli/internal/client"
	"github.com/selfbase-dev/s2-cli/internal/types"
)

// SnapshotToRemoteFiles projects /api/snapshot items into the file-centric
// map used by Compare / CompareIncremental. Directory items (Type=="dir")
// are dropped — empty dirs are reconstructed implicitly by os.MkdirAll
// when pulling files into them (ADR 0040 §その他 mkdir).
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
	archive map[string]types.FileState,
	localFiles map[string]types.LocalFile,
	remoteFiles map[string]types.RemoteFile,
) int {
	if archive == nil {
		return 0
	}
	now := time.Now().UTC().Format(time.RFC3339)
	added := 0
	for path, l := range localFiles {
		if _, exists := archive[path]; exists {
			continue
		}
		r, ok := remoteFiles[path]
		if !ok {
			continue
		}
		if r.Hash == "" || l.Hash != r.Hash {
			continue
		}
		archive[path] = types.FileState{
			LocalHash:      l.Hash,
			ContentVersion: r.ContentVersion,
			RevisionID:     r.RevisionID,
			Size:           l.Size,
			SyncedAt:       now,
		}
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
