package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/selfbase-dev/s2-sync/internal/types"
)

// CreateUploadSession creates a chunked upload session.
func (c *Client) CreateUploadSession(path string, totalSize int64, expectedChunks int) (*types.UploadSession, error) {
	payload := struct {
		Path           string `json:"path"`
		TotalSize      *int64 `json:"totalSize,omitempty"`
		ExpectedChunks *int   `json:"expectedChunks,omitempty"`
	}{
		Path: path,
	}
	if totalSize > 0 {
		payload.TotalSize = &totalSize
	}
	if expectedChunks > 0 {
		payload.ExpectedChunks = &expectedChunks
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(c.reqContext(), "POST", c.url("/api/uploads"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create upload session failed: %w", err)
	}
	defer resp.Body.Close()

	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	if resp.StatusCode != 201 {
		return nil, fmt.Errorf("create upload session failed with status %d: %s", resp.StatusCode, readErrorBody(resp))
	}

	var session types.UploadSession
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return nil, fmt.Errorf("failed to parse upload session response: %w", err)
	}
	return &session, nil
}

// UploadChunk uploads a single chunk.
func (c *Client) UploadChunk(sessionID string, chunkIndex int, body io.Reader) error {
	url := fmt.Sprintf("%s/api/uploads/%s/%d", c.endpoint, sessionID, chunkIndex)
	req, err := http.NewRequestWithContext(c.reqContext(), "PUT", url, body)
	if err != nil {
		return err
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload chunk %d failed: %w", chunkIndex, err)
	}
	defer resp.Body.Close()

	if err := checkStatus(resp); err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("upload chunk %d failed with status %d: %s", chunkIndex, resp.StatusCode, readErrorBody(resp))
	}
	return nil
}

// CompleteUpload finalizes a chunked upload session.
func (c *Client) CompleteUpload(sessionID string) (*types.UploadResult, error) {
	url := fmt.Sprintf("%s/api/uploads/%s/complete", c.endpoint, sessionID)
	req, err := http.NewRequestWithContext(c.reqContext(), "POST", url, nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("complete upload failed: %w", err)
	}
	defer resp.Body.Close()

	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("complete upload failed with status %d: %s", resp.StatusCode, readErrorBody(resp))
	}

	var result types.UploadResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse complete upload response: %w", err)
	}
	return &result, nil
}

// CancelUpload cancels a chunked upload session.
func (c *Client) CancelUpload(sessionID string) error {
	url := fmt.Sprintf("%s/api/uploads/%s", c.endpoint, sessionID)
	req, err := http.NewRequestWithContext(c.reqContext(), "DELETE", url, nil)
	if err != nil {
		return err
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("cancel upload failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 204 {
		return fmt.Errorf("cancel upload failed with status %d", resp.StatusCode)
	}
	return nil
}
