package sync

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/selfbase-dev/s2-cli/internal/client"
)

// executeConflict handles conflict by keeping local and saving remote as
// .sync-conflict-YYYYMMDD-HHMMSS file (Syncthing style, extension preserved).
//
// Special case: on initial sync (no archive), both sides exist but we don't
// know if they're identical. We download remote, compare hashes, and if
// identical treat as no-op (just record state).
func executeConflict(localPath, remoteKey, relPath, revisionID, localRoot string, c *client.Client, state *State) error {
	dl, downloadedRevisionID, err := downloadWithFallback(c, revisionID, remoteKey, relPath)
	if err != nil {
		if errors.Is(err, client.ErrNotFound) {
			fmt.Printf("conflict (remote deleted, pushing local): %s\n", relPath)
			return conflictPushLocal(localPath, remoteKey, relPath, c, state)
		}
		return fmt.Errorf("download remote for conflict: %w", err)
	}

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

	localHash, err := hashFile(localPath)
	if err != nil {
		localHash = ""
	}
	remoteHash, err := hashFile(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("hash temp file for conflict comparison: %w", err)
	}

	if localHash != "" && localHash == remoteHash {
		os.Remove(tmpPath)
		info, _ := os.Stat(localPath)
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		state.RecordFile(relPath, localHash, dl.ContentVersion, size, downloadedRevisionID)
		fmt.Printf("verified: %s (identical)\n", relPath)
		return nil
	}

	conflictPath := conflictFileName(localPath)
	if err := os.Rename(tmpPath, conflictPath); err != nil {
		os.Remove(tmpPath)
		return err
	}

	conflictRel, _ := filepath.Rel(localRoot, conflictPath)
	conflictRel = filepath.ToSlash(conflictRel)
	fmt.Printf("conflict: %s (remote saved as %s)\n", relPath, conflictRel)

	return conflictPushLocal(localPath, remoteKey, relPath, c, state)
}

func conflictPushLocal(localPath, remoteKey, relPath string, c *client.Client, state *State) error {
	lf, err := os.Open(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("conflict (local deleted, remote saved): %s\n", relPath)
			delete(state.Files, relPath)
			return nil
		}
		return err
	}
	defer lf.Close()

	result, err := c.Upload(remoteKey, lf, "", -1)
	if err != nil {
		return err
	}

	if result.Seq != nil {
		state.AddPushedSeq(*result.Seq)
	}

	cv := result.ContentVersion
	hash, err := hashFile(localPath)
	if err != nil {
		return err
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}

	state.RecordFile(relPath, hash, cv, info.Size(), "")
	return nil
}

// executePreserveLocalRename renames the local file to a .sync-conflict-*
// copy and drops its archive entry.
func executePreserveLocalRename(localPath, relPath string, state *State) error {
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
func conflictFileName(path string) string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	ts := time.Now().UTC().Format("20060102-150405")
	if ext != "" {
		return fmt.Sprintf("%s.sync-conflict-%s%s", base, ts, ext)
	}
	return fmt.Sprintf("%s.sync-conflict-%s", base, ts)
}
