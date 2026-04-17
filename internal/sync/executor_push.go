package sync

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/selfbase-dev/s2-sync/internal/client"
)

// ChunkedUploadThreshold is the file size above which chunked upload is used.
const ChunkedUploadThreshold = 10 * 1024 * 1024 // 10 MB

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

	cv := result.ContentVersion

	hash, err := hashFile(localPath)
	if err != nil {
		return err
	}

	state.RecordFile(relPath, hash, cv, "")
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

	cv := result.ContentVersion
	hash, err := hashFile(localPath)
	if err != nil {
		return err
	}

	state.RecordFile(relPath, hash, cv, "")
	return nil
}
