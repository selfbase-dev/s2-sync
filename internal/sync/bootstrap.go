// bootstrap.go — Large subtree bootstrap protocol (ADR 0046).
//
// When a scope root contains more items than the server's snapshot cap
// (~100k), GET /api/v1/snapshot returns 413 (ErrSubtreeCapExceeded).
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
	"sort"
	"strings"

	"github.com/selfbase-dev/s2-sync/internal/client"
	"github.com/selfbase-dev/s2-sync/internal/types"
)

const maxReplayIterations = 20

// FetchRemoteMap fetches the full remote file metadata map for the given path.
// It tries Snapshot first; on 413 (subtree cap exceeded), it falls back to
// ListDir and recursively fetches each subdirectory.
func FetchRemoteMap(c *client.Client, path string) (map[string]types.RemoteFile, error) {
	files, _, err := FetchRemoteMapWithDirs(c, path)
	return files, err
}

// FetchRemoteMapWithDirs is the wider sibling of FetchRemoteMap: it
// returns both the file map and the set of live directory paths under
// `path`. Used by the dir-lifecycle pipeline so empty folders the user
// created on the web UI get materialized locally even when no file
// pull would otherwise create them.
//
// The dir list is sorted and stripped of trailing slashes; empty path
// ("" = scope root) is filtered out — the local mount point is not
// re-created by sync.
func FetchRemoteMapWithDirs(c *client.Client, path string) (map[string]types.RemoteFile, []string, error) {
	files, dirs, _, err := fetchSnapshotSplit(c, path)
	if err == nil {
		return files, dirs, nil
	}
	if err != client.ErrSubtreeCapExceeded {
		return nil, nil, err
	}
	fmt.Printf("Subtree too large for atomic snapshot, splitting: %s\n", pathOrRoot(path))
	return fetchRemoteMapWithDirsViaListDir(c, path)
}

func fetchSnapshotSplit(c *client.Client, path string) (map[string]types.RemoteFile, []string, string, error) {
	return FetchSnapshotSplit(c, path)
}

func fetchRemoteMapWithDirsViaListDir(c *client.Client, path string) (map[string]types.RemoteFile, []string, error) {
	apiPath := path
	if apiPath == "" {
		apiPath = "/"
	}
	resp, err := c.ListDir(apiPath)
	if err != nil {
		return nil, nil, fmt.Errorf("listdir %s: %w", apiPath, err)
	}

	remoteFiles := make(map[string]types.RemoteFile)
	var remoteDirs []string
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
			remoteDirs = append(remoteDirs, fullPath)
			subFiles, subDirs, err := FetchRemoteMapWithDirs(c, "/"+fullPath)
			if err != nil {
				if errors.Is(err, client.ErrNotFound) {
					continue
				}
				return nil, nil, err
			}
			for k, v := range subFiles {
				remoteFiles[k] = v
			}
			remoteDirs = append(remoteDirs, subDirs...)
		} else {
			remoteFiles[fullPath] = remoteFileFromListItem(item)
		}
	}
	sort.Strings(remoteDirs)
	return remoteFiles, remoteDirs, nil
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
	files, _, cursor, err := BootstrapWithDirs(c)
	return files, cursor, err
}

// BootstrapWithDirs is Bootstrap plus the live directory list. Callers
// that drive the dir-lifecycle pipeline (materialize empty folders +
// rmdir shells) take this path; older callers stay on Bootstrap.
//
// The dir list is the post-replay set: directories deleted during the
// replay window are dropped, ones created during it are added.
func BootstrapWithDirs(c *client.Client) (map[string]types.RemoteFile, []string, string, error) {
	s0, err := c.LatestCursor()
	if err != nil {
		return nil, nil, "", fmt.Errorf("pin cursor: %w", err)
	}

	remoteFiles, remoteDirs, err := FetchRemoteMapWithDirs(c, "")
	if err != nil {
		return nil, nil, "", fmt.Errorf("fetch remote map: %w", err)
	}

	dirSet := make(map[string]struct{}, len(remoteDirs))
	for _, d := range remoteDirs {
		dirSet[d] = struct{}{}
	}
	cursor, err := replayUntilConvergedWithDirs(c, s0, remoteFiles, dirSet)
	if err != nil {
		return nil, nil, "", fmt.Errorf("delta replay: %w", err)
	}

	out := make([]string, 0, len(dirSet))
	for d := range dirSet {
		out = append(out, d)
	}
	sort.Strings(out)
	return remoteFiles, out, cursor, nil
}

// replayUntilConverged polls changes, applies them, drains dirty queues,
// and repeats until no more changes arrive after a drain.
func replayUntilConverged(c *client.Client, cursor string, remoteFiles map[string]types.RemoteFile) (string, error) {
	return replayUntilConvergedWithDirs(c, cursor, remoteFiles, nil)
}

// replayUntilConvergedWithDirs is the dir-aware variant: when dirSet is
// non-nil, dir-level changes (mkdir / delete / move) update the set in
// step with the file map so the bootstrap caller sees both projections
// converged to the same cursor. Passing nil keeps the file-only contract.
func replayUntilConvergedWithDirs(c *client.Client, cursor string, remoteFiles map[string]types.RemoteFile, dirSet map[string]struct{}) (string, error) {
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
		if dirSet != nil {
			applyDirChanges(resp.Changes, dirSet)
		}
		if len(dirtyQueue) > 0 {
			if err := drainDirtyQueueWithDirs(c, dirtyQueue, remoteFiles, dirSet); err != nil {
				return "", err
			}
		}
	}
	return "", fmt.Errorf("delta replay did not converge after %d iterations", maxReplayIterations)
}

// applyDirChanges keeps dirSet in step with the change feed for the
// directory rows themselves. It is intentionally narrower than
// applyChanges (which operates on the file map): a mkdir / delete /
// move on a directory updates dirSet, and dir puts are absorbed by
// drainDirtyQueueWithDirs via a follow-up FetchRemoteMapWithDirs.
func applyDirChanges(changes []types.ChangeEntry, dirSet map[string]struct{}) {
	for _, ch := range changes {
		if !ch.IsDir {
			continue
		}
		before := strings.TrimSuffix(strings.TrimPrefix(ch.PathBefore, "/"), "/")
		after := strings.TrimSuffix(strings.TrimPrefix(ch.PathAfter, "/"), "/")
		switch ch.Action {
		case "mkdir":
			if after != "" && isSafeRelativePath(after) {
				dirSet[after] = struct{}{}
			}
		case "delete":
			deleteDirPrefix(dirSet, before)
		case "move":
			renameDirPrefix(dirSet, before, after)
		}
	}
}

func deleteDirPrefix(dirSet map[string]struct{}, dir string) {
	if dir == "" {
		for k := range dirSet {
			delete(dirSet, k)
		}
		return
	}
	prefix := dir + "/"
	delete(dirSet, dir)
	for k := range dirSet {
		if strings.HasPrefix(k, prefix) {
			delete(dirSet, k)
		}
	}
}

func renameDirPrefix(dirSet map[string]struct{}, oldDir, newDir string) {
	if oldDir == "" {
		return
	}
	oldPrefix := oldDir + "/"
	newPrefix := newDir + "/"
	moved := make(map[string]struct{})
	for k := range dirSet {
		switch {
		case k == oldDir:
			moved[newDir] = struct{}{}
			delete(dirSet, k)
		case strings.HasPrefix(k, oldPrefix):
			moved[newPrefix+strings.TrimPrefix(k, oldPrefix)] = struct{}{}
			delete(dirSet, k)
		}
	}
	for k := range moved {
		if k != "" && isSafeRelativePath(k) {
			dirSet[k] = struct{}{}
		}
	}
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
	return drainDirtyQueueWithDirs(c, queue, remoteFiles, nil)
}

func drainDirtyQueueWithDirs(c *client.Client, queue []string, remoteFiles map[string]types.RemoteFile, dirSet map[string]struct{}) error {
	for _, path := range queue {
		subFiles, subDirs, err := FetchRemoteMapWithDirs(c, "/"+path)
		if err != nil {
			if errors.Is(err, client.ErrNotFound) {
				continue
			}
			return fmt.Errorf("drain %s: %w", path, err)
		}
		for k, v := range subFiles {
			remoteFiles[k] = v
		}
		if dirSet != nil {
			// The dir-put root itself is also live.
			if path != "" && isSafeRelativePath(path) {
				dirSet[path] = struct{}{}
			}
			for _, d := range subDirs {
				if d != "" && isSafeRelativePath(d) {
					dirSet[d] = struct{}{}
				}
			}
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
