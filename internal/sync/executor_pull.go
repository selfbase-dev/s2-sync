package sync

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/selfbase-dev/s2-cli/internal/client"
	"github.com/selfbase-dev/s2-cli/internal/types"
)

// errPullAborted is returned by executePull when a concurrent local edit is
// detected after download. The caller treats this as a conflict, not an error.
var errPullAborted = errors.New("pull aborted: local file modified during download")

// downloadWithFallback fetches content using a revision-pinned request when
// revisionID is available (ADR 0040 §2段階fetchflow), falling back to a
// path-based download when the revision has been pruned (404).
func downloadWithFallback(c *client.Client, revisionID, remoteKey, relPath string) (*client.DownloadResult, string, error) {
	if revisionID != "" {
		dl, err := c.DownloadRevision(revisionID)
		if errors.Is(err, client.ErrNotFound) {
			fmt.Printf("warning: revision %s pruned, falling back to path download: %s\n", revisionID, relPath)
			dl, err = c.Download(remoteKey)
			return dl, "", err
		}
		if err != nil {
			return nil, "", err
		}
		return dl, revisionID, nil
	}
	dl, err := c.Download(remoteKey)
	return dl, "", err
}

func executePull(localPath, remoteKey, relPath, revisionID string, c *client.Client, state *State, beforePullCommit func(string)) error {
	// Safety check: verify local hasn't changed since archive
	var preHash string
	if prev, ok := state.Files[relPath]; ok {
		currentHash, err := hashFile(localPath)
		if err == nil && currentHash != prev.LocalHash {
			fmt.Printf("conflict (local changed during pull): %s\n", relPath)
			return errPullAborted
		}
		preHash = currentHash
	}

	dl, downloadedRevisionID, err := downloadWithFallback(c, revisionID, remoteKey, relPath)
	if err != nil {
		return err
	}
	defer dl.Body.Close()

	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return err
	}

	tmpPath := localPath + ".s2tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	if _, err := io.Copy(f, dl.Body); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	f.Close()

	if beforePullCommit != nil {
		beforePullCommit(localPath)
	}

	if preHash != "" {
		postHash, err := hashFile(localPath)
		if err == nil && postHash != preHash {
			os.Remove(tmpPath)
			fmt.Printf("conflict (local changed during pull): %s\n", relPath)
			return errPullAborted
		}
	}

	if err := os.Rename(tmpPath, localPath); err != nil {
		os.Remove(tmpPath)
		return err
	}

	hash, err := hashFile(localPath)
	if err != nil {
		return err
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}

	state.RecordFile(relPath, hash, dl.ContentVersion, info.Size(), downloadedRevisionID)
	return nil
}

// skipIdempotentPull returns true when the archive already has the exact
// revision that the plan wants to pull, making the download redundant.
func skipIdempotentPull(plan types.SyncPlan, state *State) bool {
	prev, ok := state.Files[plan.Path]
	if !ok {
		return false
	}

	if plan.RevisionID != "" && prev.RevisionID != "" && prev.RevisionID == plan.RevisionID {
		fmt.Printf("skipped (same revision): %s\n", plan.Path)
		return true
	}

	return false
}
