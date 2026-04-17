// bootstrap.go — Large subtree bootstrap protocol (ADR 0046).
//
// When a scope root contains more items than the server's snapshot cap
// (~100k), GET /api/snapshot returns 413 (ErrSubtreeCapExceeded).
// The bootstrap protocol handles this by:
//
//  1. Pinning S0 via LatestCursor before any listing work begins.
//  2. Fetching the remote map — first trying Snapshot, falling back
//     to ListDir + recursive descent on 413.
//  3. Running delta replay from S0 to correct for writes that
//     occurred during the (possibly slow) fetch window.

package sync

import (
	"errors"
	"fmt"
	"strings"

	"github.com/selfbase-dev/s2-sync/internal/client"
	"github.com/selfbase-dev/s2-sync/internal/types"
)

const maxReplayIterations = 20

// FetchRemoteMap fetches the full remote file metadata map for the given path.
// It tries Snapshot first; on 413 (subtree cap exceeded), it falls back to
// ListDir and recursively fetches each subdirectory.
func FetchRemoteMap(c *client.Client, path string) (map[string]types.RemoteFile, error) {
	remoteFiles, _, err := FetchSnapshotAsRemoteFiles(c, path)
	if err == nil {
		return remoteFiles, nil
	}
	if err != client.ErrSubtreeCapExceeded {
		return nil, err
	}

	fmt.Printf("Subtree too large for atomic snapshot, splitting: %s\n", pathOrRoot(path))
	return fetchRemoteMapViaListDir(c, path)
}

func fetchRemoteMapViaListDir(c *client.Client, path string) (map[string]types.RemoteFile, error) {
	apiPath := path
	if apiPath == "" {
		apiPath = "/"
	}
	resp, err := c.ListDir(apiPath)
	if err != nil {
		return nil, fmt.Errorf("listdir %s: %w", apiPath, err)
	}

	remoteFiles := make(map[string]types.RemoteFile)
	prefix := strings.TrimPrefix(path, "/")
	for _, item := range resp.Items {
		var fullPath string
		if prefix == "" {
			fullPath = item.Name
		} else {
			fullPath = prefix + "/" + item.Name
		}

		if !isSafeRelativePath(fullPath) {
			fmt.Printf("warning: skipping unsafe path: %s\n", fullPath)
			continue
		}

		if item.Type == "directory" {
			subFiles, err := FetchRemoteMap(c, "/"+fullPath)
			if err != nil {
				if errors.Is(err, client.ErrNotFound) {
					continue
				}
				return nil, err
			}
			for k, v := range subFiles {
				remoteFiles[k] = v
			}
		} else {
			remoteFiles[fullPath] = remoteFileFromListItem(item)
		}
	}
	return remoteFiles, nil
}

func remoteFileFromListItem(item types.FileItem) types.RemoteFile {
	rf := types.RemoteFile{Name: item.Name}
	if item.Size != nil {
		rf.Size = *item.Size
	}
	if item.Hash != nil {
		rf.Hash = *item.Hash
	}
	if item.RevisionID != nil {
		rf.RevisionID = *item.RevisionID
	}
	if item.ContentVersion != nil {
		rf.ContentVersion = *item.ContentVersion
	}
	return rf
}

func remoteFileFromChangeEntry(ch types.ChangeEntry, path string) types.RemoteFile {
	name := path
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		name = path[idx+1:]
	}
	rf := types.RemoteFile{
		Name:       name,
		Hash:       ch.Hash,
		RevisionID: ch.RevisionID,
	}
	if ch.Size != nil {
		rf.Size = *ch.Size
	}
	if ch.ContentVersion != nil {
		rf.ContentVersion = *ch.ContentVersion
	}
	return rf
}

// Bootstrap implements the full bootstrap protocol (ADR 0046).
// It pins S0, fetches the remote map (with 413 fallback), runs delta
// replay to correct for changes during fetch, and returns the final
// remote map + cursor.
func Bootstrap(c *client.Client) (map[string]types.RemoteFile, string, error) {
	s0, err := c.LatestCursor()
	if err != nil {
		return nil, "", fmt.Errorf("pin cursor: %w", err)
	}

	remoteFiles, err := FetchRemoteMap(c, "")
	if err != nil {
		return nil, "", fmt.Errorf("fetch remote map: %w", err)
	}

	cursor, err := replayUntilConverged(c, s0, remoteFiles)
	if err != nil {
		return nil, "", fmt.Errorf("delta replay: %w", err)
	}

	return remoteFiles, cursor, nil
}

// replayUntilConverged polls changes, applies them, drains dirty queues,
// and repeats until no more changes arrive after a drain.
func replayUntilConverged(c *client.Client, cursor string, remoteFiles map[string]types.RemoteFile) (string, error) {
	for i := 0; i < maxReplayIterations; i++ {
		resp, err := c.PollChanges(cursor)
		if err != nil {
			return "", err
		}
		if resp.NextCursor != "" {
			cursor = resp.NextCursor
		}
		if len(resp.Changes) == 0 {
			return cursor, nil
		}

		dirtyQueue := applyChanges(resp.Changes, remoteFiles)
		if len(dirtyQueue) > 0 {
			if err := drainDirtyQueue(c, dirtyQueue, remoteFiles); err != nil {
				return "", err
			}
		}
	}
	return "", fmt.Errorf("delta replay did not converge after %d iterations", maxReplayIterations)
}

// applyChanges applies a batch of change events to the remote map.
// Returns a dirty prefix queue (paths that need to be fetched via FetchRemoteMap).
func applyChanges(changes []types.ChangeEntry, remoteFiles map[string]types.RemoteFile) []string {
	var dirtyQueue []string

	for _, ch := range changes {
		pathAfter := strings.TrimPrefix(ch.PathAfter, "/")
		pathBefore := strings.TrimPrefix(ch.PathBefore, "/")

		switch {
		case ch.IsDir && ch.Action == "put":
			dirtyQueue = append(dirtyQueue, pathAfter)

		case ch.IsDir && ch.Action == "delete":
			deletePrefixFromMap(remoteFiles, pathBefore)
			dirtyQueue = coalesceDirDelete(dirtyQueue, pathBefore)

		case ch.IsDir && ch.Action == "move":
			renamePrefixInMap(remoteFiles, pathBefore, pathAfter)
			dirtyQueue = coalesceDirMove(dirtyQueue, pathBefore, pathAfter)

		case ch.IsDir && ch.Action == "mkdir":
			// no-op: file-only map

		case !ch.IsDir && ch.Action == "put":
			if pathAfter != "" && isSafeRelativePath(pathAfter) {
				remoteFiles[pathAfter] = remoteFileFromChangeEntry(ch, pathAfter)
			}

		case !ch.IsDir && ch.Action == "delete":
			if pathBefore != "" {
				delete(remoteFiles, pathBefore)
			}

		case !ch.IsDir && ch.Action == "move":
			if pathBefore != "" {
				delete(remoteFiles, pathBefore)
			}
			if pathAfter != "" && isSafeRelativePath(pathAfter) {
				remoteFiles[pathAfter] = remoteFileFromChangeEntry(ch, pathAfter)
			}
		}
	}

	return dirtyQueue
}

func deletePrefixFromMap(m map[string]types.RemoteFile, dir string) {
	prefix := dir + "/"
	if dir == "" {
		prefix = ""
	}
	for k := range m {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			delete(m, k)
		}
	}
}

func renamePrefixInMap(m map[string]types.RemoteFile, oldDir, newDir string) {
	oldPrefix := oldDir + "/"
	newPrefix := newDir + "/"
	if oldDir == "" {
		oldPrefix = ""
	}
	if newDir == "" {
		newPrefix = ""
	}
	var toDelete []string
	toAdd := make(map[string]types.RemoteFile)
	for k, v := range m {
		if strings.HasPrefix(k, oldPrefix) {
			toDelete = append(toDelete, k)
			toAdd[newPrefix+strings.TrimPrefix(k, oldPrefix)] = v
		}
	}
	for _, k := range toDelete {
		delete(m, k)
	}
	for k, v := range toAdd {
		m[k] = v
	}
}

// coalesceDirDelete removes entries from the dirty queue that are under
// the deleted directory prefix. Required for correctness (ADR 0046).
func coalesceDirDelete(queue []string, deletedPath string) []string {
	prefix := deletedPath + "/"
	result := queue[:0]
	for _, p := range queue {
		if p != deletedPath && !strings.HasPrefix(p, prefix) {
			result = append(result, p)
		}
	}
	return result
}

// coalesceDirMove updates entries in the dirty queue whose prefix matches
// the moved directory, rewriting them to the new location. Required for
// correctness (ADR 0046).
func coalesceDirMove(queue []string, oldPath, newPath string) []string {
	oldPrefix := oldPath + "/"
	for i, p := range queue {
		if p == oldPath {
			queue[i] = newPath
		} else if strings.HasPrefix(p, oldPrefix) {
			queue[i] = newPath + "/" + strings.TrimPrefix(p, oldPrefix)
		}
	}
	return queue
}

func drainDirtyQueue(c *client.Client, queue []string, remoteFiles map[string]types.RemoteFile) error {
	for _, path := range queue {
		subFiles, err := FetchRemoteMap(c, "/"+path)
		if err != nil {
			if errors.Is(err, client.ErrNotFound) {
				continue
			}
			return fmt.Errorf("drain %s: %w", path, err)
		}
		for k, v := range subFiles {
			remoteFiles[k] = v
		}
	}
	return nil
}

func pathOrRoot(p string) string {
	if p == "" || p == "/" {
		return "/"
	}
	return p
}
