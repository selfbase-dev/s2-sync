package sync

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/selfbase-dev/s2-cli/internal/client"
	"github.com/selfbase-dev/s2-cli/internal/types"
)

// ExecuteResult tracks the outcome of sync execution.
type ExecuteResult struct {
	Pushed    int
	Pulled    int
	Deleted   int
	Conflicts int
	Errors    []error
}

// Execute applies the sync plans against local filesystem and remote storage.
// For conflict actions, local wins: local stays as-is, remote is saved as
// .sync-conflict-YYYYMMDD-HHMMSS file.
func Execute(
	plans []types.SyncPlan,
	localRoot string,
	remotePrefix string,
	c *client.Client,
	state *State,
	dryRun bool,
) (*ExecuteResult, error) {
	result := &ExecuteResult{}

	for _, plan := range plans {
		localPath := filepath.Join(localRoot, filepath.FromSlash(plan.Path))
		remoteKey := remotePrefix + plan.Path

		switch plan.Action {
		case types.Push:
			if dryRun {
				fmt.Printf("[dry-run] push: %s\n", plan.Path)
				result.Pushed++
				continue
			}
			if err := executePush(localPath, remoteKey, plan.Path, c, state); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("push %s: %w", plan.Path, err))
				continue
			}
			result.Pushed++
			fmt.Printf("pushed: %s\n", plan.Path)

		case types.Pull:
			if dryRun {
				fmt.Printf("[dry-run] pull: %s\n", plan.Path)
				result.Pulled++
				continue
			}
			if err := executePull(localPath, remoteKey, plan.Path, c, state); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("pull %s: %w", plan.Path, err))
				continue
			}
			result.Pulled++
			fmt.Printf("pulled: %s\n", plan.Path)

		case types.DeleteLocal:
			if dryRun {
				fmt.Printf("[dry-run] delete local: %s\n", plan.Path)
				result.Deleted++
				continue
			}
			if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
				result.Errors = append(result.Errors, fmt.Errorf("delete local %s: %w", plan.Path, err))
				continue
			}
			delete(state.Files, plan.Path)
			result.Deleted++
			fmt.Printf("deleted local: %s\n", plan.Path)

		case types.DeleteRemote:
			if dryRun {
				fmt.Printf("[dry-run] delete remote: %s\n", plan.Path)
				result.Deleted++
				continue
			}
			if err := c.DeleteObject(remoteKey); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("delete remote %s: %w", plan.Path, err))
				continue
			}
			delete(state.Files, plan.Path)
			result.Deleted++
			fmt.Printf("deleted remote: %s\n", plan.Path)

		case types.Conflict:
			if dryRun {
				fmt.Printf("[dry-run] conflict: %s\n", plan.Path)
				result.Conflicts++
				continue
			}
			if err := executeConflict(localPath, remoteKey, plan.Path, localRoot, c, state); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("conflict %s: %w", plan.Path, err))
				continue
			}
			result.Conflicts++
		}
	}

	return result, nil
}

func executePush(localPath, remoteKey, relPath string, c *client.Client, state *State) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	// Use If-Match for optimistic locking if we have a previous ETag
	var ifMatch string
	if prev, ok := state.Files[relPath]; ok && prev.RemoteETag != "" {
		ifMatch = prev.RemoteETag
	}

	newETag, err := c.PutObject(remoteKey, f, ifMatch)
	if err == client.ErrPreconditionFailed {
		// Remote was modified since last sync — treat as conflict
		fmt.Printf("conflict (push rejected): %s\n", relPath)
		return nil
	}
	if err != nil {
		return err
	}

	// Compute local hash for state
	hash, err := hashFile(localPath)
	if err != nil {
		return err
	}

	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}

	state.Files[relPath] = types.FileState{
		LocalHash:  hash,
		RemoteETag: newETag,
		Size:       info.Size(),
		SyncedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	return nil
}

func executePull(localPath, remoteKey, relPath string, c *client.Client, state *State) error {
	// Safety check: verify local hasn't changed since archive
	if prev, ok := state.Files[relPath]; ok {
		currentHash, err := hashFile(localPath)
		if err == nil && currentHash != prev.LocalHash {
			fmt.Printf("conflict (local changed during pull): %s\n", relPath)
			return nil
		}
	}

	body, etag, err := c.GetObject(remoteKey)
	if err != nil {
		return err
	}
	defer body.Close()

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return err
	}

	// Write to temp file, then rename
	tmpPath := localPath + ".s2tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	f.Close()

	if err := os.Rename(tmpPath, localPath); err != nil {
		os.Remove(tmpPath)
		return err
	}

	// Update state
	hash, err := hashFile(localPath)
	if err != nil {
		return err
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}

	state.Files[relPath] = types.FileState{
		LocalHash:  hash,
		RemoteETag: etag,
		Size:       info.Size(),
		SyncedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	return nil
}

// executeConflict handles conflict by keeping local and saving remote as
// .sync-conflict-YYYYMMDD-HHMMSS file (Syncthing style, extension preserved).
func executeConflict(localPath, remoteKey, relPath, localRoot string, c *client.Client, state *State) error {
	// Download remote version
	body, remoteETag, err := c.GetObject(remoteKey)
	if err != nil {
		// Remote might be deleted in delete-vs-change conflict
		// In that case, just push local
		fmt.Printf("conflict (remote unavailable, pushing local): %s\n", relPath)
		return nil
	}
	defer body.Close()

	// Build conflict file name: file.sync-conflict-YYYYMMDD-HHMMSS.ext
	conflictPath := conflictFileName(localPath)

	if err := os.MkdirAll(filepath.Dir(conflictPath), 0755); err != nil {
		return err
	}

	f, err := os.Create(conflictPath)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		return err
	}
	f.Close()

	conflictRel, _ := filepath.Rel(localRoot, conflictPath)
	conflictRel = filepath.ToSlash(conflictRel)
	fmt.Printf("conflict: %s (remote saved as %s)\n", relPath, conflictRel)

	// Push local version to remote (local wins)
	lf, err := os.Open(localPath)
	if err != nil {
		// Local might be deleted in delete-vs-change conflict
		// Remote version is already saved as conflict file
		fmt.Printf("conflict (local deleted, remote saved): %s\n", relPath)
		return nil
	}
	defer lf.Close()

	newETag, err := c.PutObject(remoteKey, lf, "")
	if err != nil {
		return err
	}

	// Update state
	hash, err := hashFile(localPath)
	if err != nil {
		return err
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}

	state.Files[relPath] = types.FileState{
		LocalHash:  hash,
		RemoteETag: newETag,
		Size:       info.Size(),
		SyncedAt:   time.Now().UTC().Format(time.RFC3339),
	}

	// Also track the conflict file in state
	conflictHash, _ := hashFile(conflictPath)
	conflictInfo, _ := os.Stat(conflictPath)
	if conflictInfo != nil {
		state.Files[conflictRel] = types.FileState{
			LocalHash:  conflictHash,
			RemoteETag: remoteETag,
			Size:       conflictInfo.Size(),
			SyncedAt:   time.Now().UTC().Format(time.RFC3339),
		}
	}

	return nil
}

// conflictFileName generates a Syncthing-style conflict name with extension preserved.
// "report.txt" → "report.sync-conflict-20260322-100000.txt"
// "Makefile" → "Makefile.sync-conflict-20260322-100000"
func conflictFileName(path string) string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	ts := time.Now().UTC().Format("20060102-150405")
	if ext != "" {
		return fmt.Sprintf("%s.sync-conflict-%s%s", base, ts, ext)
	}
	return fmt.Sprintf("%s.sync-conflict-%s", base, ts)
}
