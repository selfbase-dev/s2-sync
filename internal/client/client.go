// Package client provides the S2 REST API client.
//
// All file operations use the /api/files/ REST endpoint.
// Change log operations use /api/changes.
// Authentication is via Bearer token in the Authorization header.
package client

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/go-retryablehttp"
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

func (c *Client) filesURL(key string) string {
	return c.endpoint + "/api/files/" + key
}

// --- Errors ----------------------------------------------------------------

// ErrPreconditionFailed is returned when If-Match fails (412).
var ErrPreconditionFailed = fmt.Errorf("precondition failed: object was modified")

// ErrCursorInvalid is returned when the cursor has been pruned (410 Gone).
var ErrCursorInvalid = fmt.Errorf("cursor invalid: pruned by server, full resync required")

// --- Validate --------------------------------------------------------------

// Validate checks that the token is valid by calling /api/me.
func (c *Client) Validate() error {
	req, err := http.NewRequest("GET", c.url("/api/me"), nil)
	if err != nil {
		return err
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return fmt.Errorf("invalid or expired token")
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}
	return nil
}

// --- File Operations -------------------------------------------------------

// ListObject represents a remote file.
type ListObject struct {
	Key          string
	Size         int64
	LastModified string
	ETag         string // R2 native ETag (used for change detection)
}

// listResponse is the JSON shape returned by GET /api/files/{prefix}/.
type listResponse struct {
	Items []listItem `json:"items"`
}

type listItem struct {
	Key      string `json:"key"`
	Size     int64  `json:"size"`
	Uploaded string `json:"uploaded"`
	Hash     string `json:"hash"`
}

// ListAll lists all objects under the given prefix.
func (c *Client) ListAll(prefix string) ([]ListObject, error) {
	// Ensure prefix ends with / for directory listing
	p := prefix
	if p != "" && !strings.HasSuffix(p, "/") {
		p += "/"
	}

	req, err := http.NewRequest("GET", c.filesURL(p), nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list failed with status %d: %s", resp.StatusCode, string(body))
	}

	var result listResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse list response: %w", err)
	}

	objects := make([]ListObject, len(result.Items))
	for i, item := range result.Items {
		objects[i] = ListObject{
			Key:          item.Key,
			Size:         item.Size,
			LastModified: item.Uploaded,
			ETag:         item.Hash,
		}
	}
	return objects, nil
}

// GetObject downloads a file. Returns the body, ETag, and any error.
// Caller must close the body.
func (c *Client) GetObject(key string) (io.ReadCloser, string, error) {
	req, err := http.NewRequest("GET", c.filesURL(key), nil)
	if err != nil {
		return nil, "", err
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("get failed: %w", err)
	}

	if resp.StatusCode == 404 {
		resp.Body.Close()
		return nil, "", fmt.Errorf("not found: %s", key)
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, "", fmt.Errorf("get failed with status %d: %s", resp.StatusCode, string(body))
	}

	etag := strings.Trim(resp.Header.Get("ETag"), "\"")
	return resp.Body, etag, nil
}

// putResponse is the JSON shape returned by PUT /api/files/*.
type putResponse struct {
	Size int64  `json:"size"`
	Hash string `json:"hash"`
	ETag string `json:"etag"`
}

// PutObject uploads a file. If ifMatch is non-empty, sends If-Match header
// for optimistic locking. Returns the new ETag (R2 native).
func (c *Client) PutObject(key string, body io.Reader, ifMatch string) (string, error) {
	req, err := http.NewRequest("PUT", c.filesURL(key), body)
	if err != nil {
		return "", err
	}
	c.setAuth(req)
	if ifMatch != "" {
		if !strings.HasPrefix(ifMatch, "\"") {
			ifMatch = "\"" + ifMatch + "\""
		}
		req.Header.Set("If-Match", ifMatch)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("put failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 412 {
		return "", ErrPreconditionFailed
	}
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("put failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var result putResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to parse put response: %w", err)
	}

	return result.ETag, nil
}

// DeleteObject deletes a file.
func (c *Client) DeleteObject(key string) error {
	req, err := http.NewRequest("DELETE", c.filesURL(key), nil)
	if err != nil {
		return err
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 204 && resp.StatusCode != 200 {
		return fmt.Errorf("delete failed with status %d", resp.StatusCode)
	}
	return nil
}

// --- Change Log API --------------------------------------------------------

// ChangeEvent represents a single change from the change_log.
type ChangeEvent struct {
	Seq       int64  `json:"seq"`
	ClientID  string `json:"client_id"`
	Path      string `json:"path"`
	Action    string `json:"action"`
	Size      *int64 `json:"size"`
	Hash      string `json:"hash"`
	CreatedAt string `json:"created_at"`
}

// PollChanges fetches change_log events after the given cursor.
func (c *Client) PollChanges(after int64, limit int) ([]ChangeEvent, error) {
	url := fmt.Sprintf("%s?after=%d&limit=%d", c.url("/api/changes"), after, limit)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("poll changes failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 410 {
		return nil, ErrCursorInvalid
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("poll changes failed with status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Changes []ChangeEvent `json:"changes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse changes response: %w", err)
	}
	return result.Changes, nil
}

// LatestCursor fetches the latest change_log cursor.
func (c *Client) LatestCursor() (int64, error) {
	req, err := http.NewRequest("GET", c.url("/api/changes/latest"), nil)
	if err != nil {
		return 0, err
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("latest cursor failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("latest cursor failed with status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Latest int64 `json:"latest"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("failed to parse latest cursor response: %w", err)
	}
	return result.Latest, nil
}
