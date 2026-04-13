package sync

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/selfbase-dev/s2-cli/internal/client"
	"github.com/selfbase-dev/s2-cli/internal/types"
)

// errPullAborted is returned by executePull when a concurrent local edit is
// detected after download. The caller treats this as a conflict, not an error.
var errPullAborted = errors.New("pull aborted: local file modified during download")

// downloadWithFallback fetches content using a revision-pinned request when
// revisionID is available (ADR 0040 §2段階fetchflow), falling back to a
// path-based download when the revision has been pruned (404).
// Returns the download result, the confirmed revision ID (empty if fallback
// was used), and any error.
func downloadWithFallback(c *client.Client, revisionID, remoteKey, relPath string) (*client.DownloadResult, string, error) {
	if revisionID != "" {
		dl, err := c.DownloadRevision(revisionID)
		if err == client.ErrNotFound {
			fmt.Printf("warning: revision %s pruned, falling back to path download: %s\n", revisionID, relPath)
			dl, err = c.Download(remoteKey)
			return dl, "", err // path-based download doesn't pin a revision
		}
		if err != nil {
			return nil, "", err
		}
		return dl, revisionID, nil
	}
	dl, err := c.Download(remoteKey)
	return dl, "", err
}

// ChunkedUploadThreshold is the file size above which chunked upload is used.
const ChunkedUploadThreshold = 10 * 1024 * 1024 // 10 MB

// ExecuteResult tracks the outcome of sync execution.
type ExecuteResult struct {
	Pushed    int
	Pulled    int
	Deleted   int
	Conflicts int
	Errors    []error
}

// executeDeps holds unexported seams for testing timing-dependent behavior.
// Production code always uses the zero value (all fields nil).
type executeDeps struct {
	// beforePullCommit is called after the remote file is downloaded to a temp
	// file but before it is renamed into place. Tests use this to simulate a
	// concurrent local write between download and commit.
	beforePullCommit func(localPath string)
}

// Execute applies the sync plans against local filesystem and remote storage.
func Execute(
	plans []types.SyncPlan,
	localRoot string,
	remotePrefix string,
	c *client.Client,
	state *State,
	dryRun bool,
) (*ExecuteResult, error) {
	return execute(plans, localRoot, remotePrefix, c, state, dryRun, executeDeps{})
}

func execute(
	plans []types.SyncPlan,
	localRoot string,
	remotePrefix string,
	c *client.Client,
	state *State,
	dryRun bool,
	deps executeDeps,
) (*ExecuteResult, error) {
	result := &ExecuteResult{}

	for _, plan := range plans {
		// Defence-in-depth: every plan path came from either Walk (local
		// filesystem, trusted) or from server data via Compare /
		// CompareIncremental / HandleIncrementalDirEvents. safeJoin
		// rejects `..`, null bytes and post-clean root escapes so a
		// malicious or buggy server cannot trick the executor into
		// writing outside localRoot.
		localPath, err := safeJoin(localRoot, plan.Path)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("unsafe plan path %s: %w", plan.Path, err))
			continue
		}
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
			if skipIdempotentPull(plan, state) {
				continue
			}
			if err := executePull(localPath, remoteKey, plan.Path, plan.RevisionID, c, state, deps.beforePullCommit); err != nil {
				if errors.Is(err, errPullAborted) {
					result.Conflicts++
					continue
				}
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
			delResult, err := c.Delete(remoteKey)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("delete remote %s: %w", plan.Path, err))
				continue
			}
			if delResult != nil && delResult.Seq != nil {
				state.AddPushedSeq(*delResult.Seq)
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
			if err := executeConflict(localPath, remoteKey, plan.Path, plan.RevisionID, localRoot, c, state); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("conflict %s: %w", plan.Path, err))
				continue
			}
			result.Conflicts++

		case types.PreserveLocalRename:
			// Used when the server has removed a file (dir delete,
			// scope-out move) but the local copy has edits we must
			// not destroy. We rename to .sync-conflict-* and drop the
			// archive entry — critically, we do NOT push back, which
			// would resurrect the subtree the server just removed.
			if dryRun {
				fmt.Printf("[dry-run] preserve-local-rename: %s\n", plan.Path)
				result.Conflicts++
				continue
			}
			if err := executePreserveLocalRename(localPath, plan.Path, state); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("preserve %s: %w", plan.Path, err))
				continue
			}
			result.Conflicts++

		}
	}

	return result, nil
}

func executePush(localPath, remoteKey, relPath string, c *client.Client, state *State) error {
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}

	// Use chunked upload for large files
	if info.Size() > ChunkedUploadThreshold {
		return executePushChunked(localPath, remoteKey, relPath, info.Size(), c, state)
	}

	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	// Determine conditional header
	var ifMatchVersion int64
	if prev, ok := state.Files[relPath]; ok && prev.ContentVersion > 0 {
		ifMatchVersion = prev.ContentVersion // If-Match for update
	} else {
		ifMatchVersion = 0 // If-None-Match: * for create
	}

	result, err := c.Upload(remoteKey, f, "", ifMatchVersion)
	if err == client.ErrPreconditionFailed || err == client.ErrConflict {
		return fmt.Errorf("conflict (push rejected, remote was modified): %s", relPath)
	}
	if err != nil {
		return err
	}

	// Record seq for self-change filtering (ADR 0033)
	if result.Seq != nil {
		state.AddPushedSeq(*result.Seq)
	}

	// Parse content_version from etag
	cv, _ := client.ParseContentVersion(result.ETag)

	hash, err := hashFile(localPath)
	if err != nil {
		return err
	}

	state.Files[relPath] = types.FileState{
		LocalHash:      hash,
		ContentVersion: cv,
		Size:           info.Size(),
		SyncedAt:       time.Now().UTC().Format(time.RFC3339),
	}
	return nil
}

func executePushChunked(localPath, remoteKey, relPath string, totalSize int64, c *client.Client, state *State) error {
	// NOTE: Chunked upload does not support If-Match CAS (the server's upload
	// session has base_content_version internally, but it's not exposed in the
	// public API yet). This means concurrent edits to large files won't be
	// detected as conflicts. See ADR 0033 for the planned API change.

	// Create upload session
	session, err := c.CreateUploadSession(remoteKey, totalSize, 0)
	if err != nil {
		return fmt.Errorf("create upload session: %w", err)
	}

	f, err := os.Open(localPath)
	if err != nil {
		_ = c.CancelUpload(session.SessionID)
		return err
	}
	defer f.Close()

	chunkSize := session.ChunkSize
	buf := make([]byte, chunkSize)
	chunkIndex := 0
	totalChunks := int((totalSize + int64(chunkSize) - 1) / int64(chunkSize))

	for {
		n, readErr := io.ReadFull(f, buf)
		if n > 0 {
			if err := c.UploadChunk(session.SessionID, chunkIndex, bytes.NewReader(buf[:n])); err != nil {
				_ = c.CancelUpload(session.SessionID)
				return fmt.Errorf("upload chunk %d: %w", chunkIndex, err)
			}
			fmt.Printf("  chunk %d/%d uploaded\n", chunkIndex+1, totalChunks)
			chunkIndex++
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			_ = c.CancelUpload(session.SessionID)
			return fmt.Errorf("read file: %w", readErr)
		}
	}

	result, err := c.CompleteUpload(session.SessionID)
	if err != nil {
		return fmt.Errorf("complete upload: %w", err)
	}

	// Record seq for self-change filtering (ADR 0033)
	if result.Seq != nil {
		state.AddPushedSeq(*result.Seq)
	}

	cv, _ := client.ParseContentVersion(result.ETag)
	hash, err := hashFile(localPath)
	if err != nil {
		return err
	}

	state.Files[relPath] = types.FileState{
		LocalHash:      hash,
		ContentVersion: cv,
		Size:           totalSize,
		SyncedAt:       time.Now().UTC().Format(time.RFC3339),
	}
	return nil
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

	if _, err := io.Copy(f, dl.Body); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	f.Close()

	// Allow tests to inject a concurrent local write before commit.
	if beforePullCommit != nil {
		beforePullCommit(localPath)
	}

	// Re-check local hash: abort if the file was modified during download.
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
		LocalHash:      hash,
		ContentVersion: dl.ContentVersion,
		RevisionID:     downloadedRevisionID,
		Size:           info.Size(),
		SyncedAt:       time.Now().UTC().Format(time.RFC3339),
	}
	return nil
}

// executeConflict handles conflict by keeping local and saving remote as
// .sync-conflict-YYYYMMDD-HHMMSS file (Syncthing style, extension preserved).
//
// Special case: on initial sync (no archive), both sides exist but we don't
// know if they're identical. We download remote, compare hashes, and if
// identical treat as no-op (just record state).
func executeConflict(localPath, remoteKey, relPath, revisionID, localRoot string, c *client.Client, state *State) error {
	dl, downloadedRevisionID, err := downloadWithFallback(c, revisionID, remoteKey, relPath)
	if err != nil {
		if err == client.ErrNotFound {
			// File itself deleted (not just revision pruned); push local.
			fmt.Printf("conflict (remote deleted, pushing local): %s\n", relPath)
			return conflictPushLocal(localPath, remoteKey, relPath, c, state)
		}
		return fmt.Errorf("download remote for conflict: %w", err)
	}

	// Write remote content to temp file so we can hash it
	tmpPath := localPath + ".s2conflict"
	if err := os.MkdirAll(filepath.Dir(tmpPath), 0755); err != nil {
		dl.Body.Close()
		return err
	}
	tmpF, err := os.Create(tmpPath)
	if err != nil {
		dl.Body.Close()
		return err
	}
	if _, err := io.Copy(tmpF, dl.Body); err != nil {
		tmpF.Close()
		dl.Body.Close()
		os.Remove(tmpPath)
		return err
	}
	tmpF.Close()
	dl.Body.Close()

	// Compare hashes: if identical, this isn't a real conflict
	localHash, err := hashFile(localPath)
	if err != nil {
		// Local might not exist (delete-vs-change conflict)
		localHash = ""
	}
	remoteHash, err := hashFile(tmpPath)
	if err != nil {
		// tmpPath was written moments ago — if we can't hash it,
		// something is seriously wrong; treat as error, not conflict.
		os.Remove(tmpPath)
		return fmt.Errorf("hash temp file for conflict comparison: %w", err)
	}

	if localHash != "" && localHash == remoteHash {
		// Identical content — not a real conflict, just record state
		os.Remove(tmpPath)
		info, _ := os.Stat(localPath)
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		state.Files[relPath] = types.FileState{
			LocalHash:      localHash,
			ContentVersion: dl.ContentVersion,
			RevisionID:     downloadedRevisionID,
			Size:           size,
			SyncedAt:       time.Now().UTC().Format(time.RFC3339),
		}
		fmt.Printf("verified: %s (identical)\n", relPath)
		return nil
	}

	// Real conflict: save remote as .sync-conflict-* file
	conflictPath := conflictFileName(localPath)
	if err := os.Rename(tmpPath, conflictPath); err != nil {
		os.Remove(tmpPath)
		return err
	}

	conflictRel, _ := filepath.Rel(localRoot, conflictPath)
	conflictRel = filepath.ToSlash(conflictRel)
	fmt.Printf("conflict: %s (remote saved as %s)\n", relPath, conflictRel)

	// Push local version to remote (local wins).
	return conflictPushLocal(localPath, remoteKey, relPath, c, state)
}

// conflictPushLocal uploads the local file to remote, overwriting whatever is there.
// Used to resolve conflicts where local wins and remote has been deleted or diverged.
func conflictPushLocal(localPath, remoteKey, relPath string, c *client.Client, state *State) error {
	lf, err := os.Open(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Local also gone — nothing to push; clean archive entry.
			fmt.Printf("conflict (local deleted, remote saved): %s\n", relPath)
			delete(state.Files, relPath)
			return nil
		}
		return err
	}
	defer lf.Close()

	result, err := c.Upload(remoteKey, lf, "", -1) // force overwrite
	if err != nil {
		return err
	}

	if result.Seq != nil {
		state.AddPushedSeq(*result.Seq)
	}

	cv, _ := client.ParseContentVersion(result.ETag)
	hash, err := hashFile(localPath)
	if err != nil {
		return err
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}

	state.Files[relPath] = types.FileState{
		LocalHash:      hash,
		ContentVersion: cv,
		Size:           info.Size(),
		SyncedAt:       time.Now().UTC().Format(time.RFC3339),
	}
	return nil
}

// executePreserveLocalRename renames the local file to a
// .sync-conflict-* copy and drops its archive entry. Used when the
// server has authoritatively removed the file (dir delete / scope-out
// move) but the local copy diverged and must not be destroyed.
// Crucially, NO network call — pushing the local copy back would
// resurrect the subtree the server just deleted.
func executePreserveLocalRename(localPath, relPath string, state *State) error {
	// If local is already gone there's nothing to preserve; just untrack.
	if _, err := os.Stat(localPath); err != nil {
		if os.IsNotExist(err) {
			delete(state.Files, relPath)
			fmt.Printf("preserved (already gone): %s\n", relPath)
			return nil
		}
		return err
	}
	conflictPath := conflictFileName(localPath)
	if err := os.Rename(localPath, conflictPath); err != nil {
		return err
	}
	delete(state.Files, relPath)
	fmt.Printf("preserved local as %s: %s\n", filepath.Base(conflictPath), relPath)
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

// skipIdempotentPull returns true when the archive already has the exact
// revision that the plan wants to pull, making the download redundant.
// Implements ADR 0040 §idempotent apply.
//
// Only RevisionID exact match is used — it guarantees identical immutable
// content (ADR 0021) and that the archive metadata (ContentVersion, etc.)
// is already correct from the previous download. Hash-based skip was
// rejected: it would leave ContentVersion stale (breaking CAS on next
// push) and bypass the local-change safety check in executePull.
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
