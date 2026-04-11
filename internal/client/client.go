// Package client provides the S2 REST API client.
//
// All methods correspond to the public OpenAPI spec (GET /api/openapi.yaml).
// Authentication is via Bearer token in the Authorization header.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	"github.com/selfbase-dev/s2-cli/internal/types"
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
	// ErrSubtreeCapExceeded is returned when /api/snapshot rejects a subtree
	// larger than the server's configured cap (ADR 0038 OQ2 / ADR 0039 §Errors).
	ErrSubtreeCapExceeded = fmt.Errorf("subtree cap exceeded: server refused atomic snapshot")
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
	// Don't retry on 4xx errors (they won't succeed on retry)
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

// checkStatus maps common HTTP status codes to sentinel errors.
//
// Note: 413 on /api/files/* means "storage quota exceeded"; 413 on
// /api/snapshot means "subtree cap exceeded". Callers that need to
// distinguish the two should inspect the endpoint; this helper returns
// the storage-quota error by default and `Snapshot()` overrides it.
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

// readErrorBody reads the response body for error context.
func readErrorBody(resp *http.Response) string {
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// --- Auth ---

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

// --- File Operations ---

// ListDir lists files in a directory. Path should end with "/" for directories.
// Empty path lists the root directory.
func (c *Client) ListDir(path string) (*types.ListResponse, error) {
	if path == "" || path == "/" {
		path = "" // root listing: /api/files/ (not /api/files//)
	} else if !strings.HasSuffix(path, "/") {
		path += "/"
	}

	req, err := http.NewRequest("GET", c.filesURL(path), nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list failed: %w", err)
	}
	defer resp.Body.Close()

	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("list failed with status %d: %s", resp.StatusCode, readErrorBody(resp))
	}

	var result types.ListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse list response: %w", err)
	}
	return &result, nil
}

// ListAllRecursive lists all files under a prefix recursively.
// Returns a map of relative path → RemoteFile.
func (c *Client) ListAllRecursive(prefix string) (map[string]types.RemoteFile, error) {
	result := make(map[string]types.RemoteFile)
	if err := c.listRecursive(prefix, "", result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) listRecursive(prefix, relDir string, result map[string]types.RemoteFile) error {
	path := prefix
	if relDir != "" {
		path = prefix + relDir
	}

	listing, err := c.ListDir(path)
	if err == ErrNotFound {
		return nil // directory doesn't exist yet — treat as empty
	}
	if err != nil {
		return err
	}

	for _, item := range listing.Items {
		relPath := relDir + item.Name
		if item.Type == "directory" {
			if err := c.listRecursive(prefix, relPath+"/", result); err != nil {
				return err
			}
		} else {
			size := int64(0)
			if item.Size != nil {
				size = *item.Size
			}
			result[relPath] = types.RemoteFile{
				Name:       item.Name,
				Size:       size,
				ModifiedAt: item.ModifiedAt,
			}
		}
	}
	return nil
}

// DownloadResult contains the result of a file download.
type DownloadResult struct {
	Body           io.ReadCloser
	ContentVersion int64
	Size           int64
	ContentType    string
}

// Download downloads a file. Caller must close Body.
func (c *Client) Download(path string) (*DownloadResult, error) {
	req, err := http.NewRequest("GET", c.filesURL(path), nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}

	if err := checkStatus(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	if resp.StatusCode != 200 {
		body := readErrorBody(resp)
		resp.Body.Close()
		return nil, fmt.Errorf("download failed with status %d: %s", resp.StatusCode, body)
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

// HeadFile returns the content version and size without downloading.
func (c *Client) HeadFile(path string) (contentVersion int64, size int64, err error) {
	req, err := http.NewRequest("HEAD", c.filesURL(path), nil)
	if err != nil {
		return 0, 0, err
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("head failed: %w", err)
	}
	defer resp.Body.Close()

	if err := checkStatus(resp); err != nil {
		return 0, 0, err
	}
	if resp.StatusCode != 200 {
		return 0, 0, fmt.Errorf("head failed with status %d", resp.StatusCode)
	}

	cv, _ := ParseContentVersion(resp.Header.Get("ETag"))
	sz, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	return cv, sz, nil
}

// Upload uploads a file. Returns the upload result with content version.
// ifMatchVersion > 0 sends If-Match for CAS update.
// ifMatchVersion == 0 sends If-None-Match: * for create-only.
// ifMatchVersion < 0 sends no conditional headers (force overwrite).
func (c *Client) Upload(path string, body io.Reader, contentType string, ifMatchVersion int64) (*types.UploadResult, error) {
	req, err := http.NewRequest("PUT", c.filesURL(path), body)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	if ifMatchVersion > 0 {
		req.Header.Set("If-Match", FormatETag(ifMatchVersion))
	} else if ifMatchVersion == 0 {
		req.Header.Set("If-None-Match", "*")
	}
	// ifMatchVersion < 0: no conditional headers

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload failed: %w", err)
	}
	defer resp.Body.Close()

	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	if resp.StatusCode != 201 && resp.StatusCode != 200 {
		return nil, fmt.Errorf("upload failed with status %d: %s", resp.StatusCode, readErrorBody(resp))
	}

	var result types.UploadResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse upload response: %w", err)
	}
	return &result, nil
}

// Mkdir creates a directory.
func (c *Client) Mkdir(path string) error {
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	req, err := http.NewRequest("PUT", c.filesURL(path), nil)
	if err != nil {
		return err
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("mkdir failed: %w", err)
	}
	defer resp.Body.Close()

	if err := checkStatus(resp); err != nil {
		return err
	}
	if resp.StatusCode != 201 && resp.StatusCode != 200 {
		return fmt.Errorf("mkdir failed with status %d: %s", resp.StatusCode, readErrorBody(resp))
	}
	return nil
}

// Delete deletes a file (soft delete to trash).
// Returns DeleteResult with optional Seq for self-change filtering.
func (c *Client) Delete(path string) (*types.DeleteResult, error) {
	req, err := http.NewRequest("DELETE", c.filesURL(path), nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("delete failed: %w", err)
	}
	defer resp.Body.Close()

	if err := checkStatus(resp); err != nil {
		return nil, err
	}

	result := &types.DeleteResult{}

	// Current server returns 204 (no body). Future server may return 200 with seq.
	if resp.StatusCode == 200 {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return nil, fmt.Errorf("failed to parse delete response: %w", err)
		}
	} else if resp.StatusCode != 204 {
		return nil, fmt.Errorf("delete failed with status %d: %s", resp.StatusCode, readErrorBody(resp))
	}

	return result, nil
}

// Move moves or renames a file/directory.
func (c *Client) Move(srcPath, dstPath string, overwrite bool) error {
	payload := struct {
		Destination string `json:"destination"`
		Overwrite   bool   `json:"overwrite,omitempty"`
	}{
		Destination: dstPath,
		Overwrite:   overwrite,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", c.endpoint+"/api/file-moves/"+srcPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("move failed: %w", err)
	}
	defer resp.Body.Close()

	if err := checkStatus(resp); err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("move failed with status %d: %s", resp.StatusCode, readErrorBody(resp))
	}
	return nil
}

// --- Tokens ---

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

// --- Chunked Upload ---

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

	req, err := http.NewRequest("POST", c.url("/api/uploads"), bytes.NewReader(body))
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
	req, err := http.NewRequest("PUT", url, body)
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
	req, err := http.NewRequest("POST", url, nil)
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
	req, err := http.NewRequest("DELETE", url, nil)
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

// --- Snapshot primitive (ADR 0039) ---

// Snapshot fetches an atomic subtree snapshot from GET /api/snapshot.
// An empty `path` (or "/") snapshots the token's scope root — used for
// bootstrap and 410 recovery. A non-empty `path` snapshots a subtree —
// used for scope-crossing `put` events and restore operations in the
// hybrid strategy (ADR 0040 §hybrid 戦略の分岐).
//
// The returned `Cursor` is atomic with `Items`: no writes between the
// snapshot and the cursor can be missed or double-counted.
//
// 413 / `ErrSubtreeCapExceeded` means the subtree is larger than the
// server's configured cap. Callers must either request a smaller
// subtree, fall back to error reporting, or wait for a future streaming
// mode (out of scope for v1 — ADR 0038 §non-goals).
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

	// 413 on /api/snapshot means "subtree cap exceeded" — distinct from
	// the storage-quota meaning of 413 on /api/files/*.
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

// DownloadRevision downloads file content by revision id — the race-free
// counterpart to Download(path). See ADR 0040 §2段階fetchflow: sync
// clients pair metadata from Snapshot() with this endpoint to avoid
// racing with concurrent writes. Callers must close Body.
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

// --- Change Log ---

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

// --- ETag helpers ---

// ParseContentVersion extracts the integer content_version from an ETag header.
// ETag format: "42" (quoted integer string).
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
