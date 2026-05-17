package sync

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/selfbase-dev/s2-sync/internal/client"
	slog2 "github.com/selfbase-dev/s2-sync/internal/log"
	"github.com/selfbase-dev/s2-sync/internal/types"
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
			slog.Default().Warn(slog2.FileSkip,
				"path", relPath,
				"revision_id", revisionID,
				"reason", "revision_pruned_falling_back_to_path_download",
			)
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
			slog.Default().Warn(slog2.FileConflict,
				"path", relPath,
				"reason", "local_changed_during_pull",
			)
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
			slog.Default().Warn(slog2.FileConflict,
				"path", relPath,
				"reason", "local_changed_during_pull",
			)
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

	state.RecordFile(relPath, hash, dl.ContentVersion, downloadedRevisionID)
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
		slog.Default().Info(slog2.FileSkip,
			"path", plan.Path,
			"reason", "same_revision",
		)
		return true
	}

	return false
}
