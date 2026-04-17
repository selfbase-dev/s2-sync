package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"

	"github.com/selfbase-dev/s2-sync/internal/types"
)

// PollChanges fetches change events after the given cursor.
func (c *Client) PollChanges(cursor string) (*types.ChangesResponse, error) {
	reqURL := c.url("/api/changes")
	if cursor != "" {
		reqURL += "?after=" + neturl.QueryEscape(cursor)
	}

	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("poll changes failed: %w", err)
	}
	defer resp.Body.Close()

	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("poll changes failed with status %d: %s", resp.StatusCode, readErrorBody(resp))
	}

	var result types.ChangesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse changes response: %w", err)
	}
	return &result, nil
}

// LatestCursor fetches the latest cursor without consuming events.
func (c *Client) LatestCursor() (string, error) {
	req, err := http.NewRequest("GET", c.url("/api/changes/latest"), nil)
	if err != nil {
		return "", err
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("latest cursor failed: %w", err)
	}
	defer resp.Body.Close()

	if err := checkStatus(resp); err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("latest cursor failed with status %d: %s", resp.StatusCode, readErrorBody(resp))
	}

	var result types.LatestCursorResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to parse latest cursor response: %w", err)
	}
	return result.Cursor, nil
}
