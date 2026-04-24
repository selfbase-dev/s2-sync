package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/selfbase-dev/s2-sync/internal/types"
)

// ListDir lists files in a directory.
func (c *Client) ListDir(path string) (*types.ListResponse, error) {
	if path == "" || path == "/" {
		path = ""
	} else if !strings.HasSuffix(path, "/") {
		path += "/"
	}

	req, err := http.NewRequestWithContext(c.reqContext(), "GET", c.filesURL(path), nil)
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
		return nil
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
	req, err := http.NewRequestWithContext(c.reqContext(), "GET", c.filesURL(path), nil)
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
	req, err := http.NewRequestWithContext(c.reqContext(), "HEAD", c.filesURL(path), nil)
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

// Upload uploads a file.
func (c *Client) Upload(path string, body io.Reader, contentType string, ifMatchVersion int64) (*types.UploadResult, error) {
	req, err := http.NewRequestWithContext(c.reqContext(), "PUT", c.filesURL(path), body)
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
	req, err := http.NewRequestWithContext(c.reqContext(), "PUT", c.filesURL(path), nil)
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
func (c *Client) Delete(path string) (*types.DeleteResult, error) {
	req, err := http.NewRequestWithContext(c.reqContext(), "DELETE", c.filesURL(path), nil)
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
	if resp.StatusCode == 200 {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return nil, fmt.Errorf("failed to parse delete response: %w", err)
		}
	} else if resp.StatusCode != 204 {
		return nil, fmt.Errorf("delete failed with status %d: %s", resp.StatusCode, readErrorBody(resp))
	}

	return result, nil
}

// ErrMoveConflict is returned when POST /api/file-moves returns 409
// (destination already exists or cycle detected). Callers should treat
// this as SkipCaseConflict per the collision policy — do NOT fall back to
// delete+push, which is not atomic and can lose data.
var ErrMoveConflict = fmt.Errorf("move conflict: destination exists or cycle")

// Move moves or renames a file/directory via POST /api/file-moves/{src}.
// On 409 the returned error wraps ErrMoveConflict so callers can detect
// the collision case without string-matching.
//
// The second return (*types.MoveResult) is populated on 200 with the
// new node's changelog seq and content_version — needed by the sync
// executor to self-filter the emitted change event and to update the
// archive row in place.
func (c *Client) Move(srcPath, dstPath string) (*types.MoveResult, error) {
	payload := struct {
		Destination string `json:"destination"`
	}{Destination: dstPath}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(c.reqContext(), "POST", c.endpoint+"/api/file-moves/"+srcPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("move failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		return nil, fmt.Errorf("%w: src=%q dst=%q", ErrMoveConflict, srcPath, dstPath)
	}
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("move failed with status %d: %s", resp.StatusCode, readErrorBody(resp))
	}

	var result types.MoveResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode move response: %w", err)
	}
	return &result, nil
}
