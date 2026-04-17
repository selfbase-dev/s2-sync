package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"strconv"

	"github.com/selfbase-dev/s2-sync/internal/types"
)

// Snapshot fetches an atomic subtree snapshot from GET /api/snapshot.
func (c *Client) Snapshot(path string) (*types.SnapshotResponse, error) {
	reqURL := c.url("/api/snapshot")
	if path != "" && path != "/" {
		reqURL += "?path=" + neturl.QueryEscape(path)
	}

	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("snapshot failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 413 {
		return nil, ErrSubtreeCapExceeded
	}
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("snapshot failed with status %d: %s", resp.StatusCode, readErrorBody(resp))
	}

	var result types.SnapshotResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse snapshot response: %w", err)
	}
	return &result, nil
}

// DownloadRevision downloads file content by revision id.
func (c *Client) DownloadRevision(revisionID string) (*DownloadResult, error) {
	req, err := http.NewRequest("GET", c.url("/api/revisions/"+neturl.PathEscape(revisionID)), nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download revision failed: %w", err)
	}

	if err := checkStatus(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	if resp.StatusCode != 200 {
		body := readErrorBody(resp)
		resp.Body.Close()
		return nil, fmt.Errorf("download revision failed with status %d: %s", resp.StatusCode, body)
	}

	cv, _ := ParseContentVersion(resp.Header.Get("ETag"))
	size, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)

	return &DownloadResult{
		Body:           resp.Body,
		ContentVersion: cv,
		Size:           size,
		ContentType:    resp.Header.Get("Content-Type"),
	}, nil
}
