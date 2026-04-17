// Package client provides the S2 REST API client.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	"github.com/selfbase-dev/s2-sync/internal/types"
)

// Sentinel errors.
var (
	ErrPreconditionFailed   = fmt.Errorf("precondition failed: resource was modified")
	ErrConflict             = fmt.Errorf("conflict: resource already exists")
	ErrCursorGone           = fmt.Errorf("cursor gone: full resync required")
	ErrNotFound             = fmt.Errorf("not found")
	ErrForbidden            = fmt.Errorf("forbidden")
	ErrUnauthorized         = fmt.Errorf("unauthorized: invalid or expired token")
	ErrStorageLimitExceeded = fmt.Errorf("storage limit exceeded")
	ErrSubtreeCapExceeded   = fmt.Errorf("subtree cap exceeded: server refused atomic snapshot")
)

// Client talks to the S2 REST API.
type Client struct {
	endpoint   string
	token      string
	httpClient *http.Client
}

// New creates a new S2 client.
func New(endpoint, token string) *Client {
	retryClient := retryablehttp.NewClient()
	retryClient.RetryMax = 3
	retryClient.RetryWaitMin = 500 * time.Millisecond
	retryClient.RetryWaitMax = 5 * time.Second
	retryClient.Logger = nil
	retryClient.CheckRetry = func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		if err != nil {
			return retryablehttp.DefaultRetryPolicy(ctx, resp, err)
		}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return false, nil
		}
		return retryablehttp.DefaultRetryPolicy(ctx, resp, err)
	}

	return &Client{
		endpoint:   strings.TrimRight(endpoint, "/"),
		token:      token,
		httpClient: retryClient.StandardClient(),
	}
}

func (c *Client) setAuth(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
}

func (c *Client) url(path string) string {
	return c.endpoint + path
}

func (c *Client) filesURL(path string) string {
	return c.endpoint + "/api/files/" + path
}

func checkStatus(resp *http.Response) error {
	switch resp.StatusCode {
	case 401:
		return ErrUnauthorized
	case 403:
		return ErrForbidden
	case 404:
		return ErrNotFound
	case 409:
		return ErrConflict
	case 410:
		return ErrCursorGone
	case 412:
		return ErrPreconditionFailed
	case 413:
		return ErrStorageLimitExceeded
	}
	return nil
}

func readErrorBody(resp *http.Response) string {
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// Me returns the auth context for the current token.
func (c *Client) Me() (*types.MeTokenResponse, error) {
	req, err := http.NewRequest("GET", c.url("/api/me"), nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()

	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, readErrorBody(resp))
	}

	var me types.MeTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		return nil, fmt.Errorf("failed to parse /api/me response: %w", err)
	}
	return &me, nil
}

// --- ETag helpers ---

// ParseContentVersion extracts the integer content_version from an ETag header.
func ParseContentVersion(etag string) (int64, error) {
	s := strings.Trim(etag, "\"")
	if s == "" {
		return 0, fmt.Errorf("empty ETag")
	}
	return strconv.ParseInt(s, 10, 64)
}

// FormatETag formats a content_version as an ETag header value.
func FormatETag(contentVersion int64) string {
	return fmt.Sprintf(`"%d"`, contentVersion)
}
