package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/selfbase-dev/s2-sync/internal/types"
)

// CreateToken creates a child token via delegation.
func (c *Client) CreateToken(name, basePath string, canDelegate bool, accessPaths []types.AccessPath) (*types.CreateTokenResponse, error) {
	if accessPaths == nil {
		accessPaths = []types.AccessPath{}
	}
	req := types.CreateTokenRequest{
		Name:        name,
		BasePath:    basePath,
		CanDelegate: canDelegate,
		AccessPaths: accessPaths,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequest("POST", c.url("/api/tokens"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setAuth(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("create token failed: %w", err)
	}
	defer resp.Body.Close()

	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	if resp.StatusCode != 201 {
		return nil, fmt.Errorf("create token failed with status %d: %s", resp.StatusCode, readErrorBody(resp))
	}

	var result types.CreateTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse create token response: %w", err)
	}
	return &result, nil
}
