package client

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/selfbase-dev/s2-cli/internal/types"
)

// --- Helpers ---

// newTestServer creates an httptest server with a route handler map.
// Routes are keyed as "METHOD /path" (exact match) or "METHOD /prefix/" (prefix match).
func newTestServer(t *testing.T, routes map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		if h, ok := routes[key]; ok {
			h(w, r)
			return
		}
		// Try prefix match
		for k, h := range routes {
			parts := strings.SplitN(k, " ", 2)
			if len(parts) == 2 && parts[0] == r.Method && strings.HasSuffix(parts[1], "/") && strings.HasPrefix(r.URL.Path, parts[1]) {
				h(w, r)
				return
			}
		}
		t.Errorf("unhandled request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(500)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func requireAuth(t *testing.T, r *http.Request, expectedToken string) bool {
	t.Helper()
	got := r.Header.Get("Authorization")
	want := "Bearer " + expectedToken
	if got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
		return false
	}
	return true
}

func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// --- ETag helpers ---

func TestParseContentVersion(t *testing.T) {
	tests := []struct {
		name    string
		etag    string
		want    int64
		wantErr bool
	}{
		{"quoted integer", `"42"`, 42, false},
		{"unquoted integer", "42", 42, false},
		{"zero", `"0"`, 0, false},
		{"large number", `"999999"`, 999999, false},
		{"empty", "", 0, true},
		{"empty quotes", `""`, 0, true},
		{"non-numeric", `"abc"`, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseContentVersion(tt.etag)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseContentVersion(%q) error = %v, wantErr %v", tt.etag, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ParseContentVersion(%q) = %d, want %d", tt.etag, got, tt.want)
			}
		})
	}
}

func TestFormatETag(t *testing.T) {
	tests := []struct {
		cv   int64
		want string
	}{
		{42, `"42"`},
		{0, `"0"`},
		{999999, `"999999"`},
	}
	for _, tt := range tests {
		got := FormatETag(tt.cv)
		if got != tt.want {
			t.Errorf("FormatETag(%d) = %q, want %q", tt.cv, got, tt.want)
		}
	}
}

// --- /api/me ---

func TestMe_Success(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/me": func(w http.ResponseWriter, r *http.Request) {
			requireAuth(t, r, "s2_testtoken")
			jsonResponse(w, 200, map[string]any{
				"type": "token", "user_id": "user_1", "token_id": "tok_1",
				"can_delegate": false,
				"access_paths": []map[string]any{{"path": "/", "can_read": true, "can_write": true}},
			})
		},
	})

	c := New(srv.URL, "s2_testtoken")
	me, err := c.Me()
	if err != nil {
		t.Fatalf("Me() error: %v", err)
	}
	if me.TokenID != "tok_1" {
		t.Errorf("TokenID = %q, want %q", me.TokenID, "tok_1")
	}
	if me.UserID != "user_1" {
		t.Errorf("UserID = %q, want %q", me.UserID, "user_1")
	}
	if len(me.AccessPaths) != 1 {
		t.Errorf("AccessPaths len = %d, want 1", len(me.AccessPaths))
	}
}

func TestMe_Unauthorized(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/me": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(401)
		},
	})

	c := New(srv.URL, "s2_bad")
	_, err := c.Me()
	if err != ErrUnauthorized {
		t.Errorf("Me() error = %v, want ErrUnauthorized", err)
	}
}

// --- /api/files (list) ---

func TestListDir_Success(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/files/": func(w http.ResponseWriter, r *http.Request) {
			size := int64(1024)
			jsonResponse(w, 200, map[string]any{
				"items": []map[string]any{
					{"id": "n1", "name": "readme.md", "type": "file", "size": size, "modified_at": "2026-04-01T00:00:00Z"},
					{"id": "n2", "name": "docs", "type": "directory"},
				},
			})
		},
	})

	c := New(srv.URL, "s2_test")
	resp, err := c.ListDir("prefix/")
	if err != nil {
		t.Fatalf("ListDir() error: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("ListDir() got %d items, want 2", len(resp.Items))
	}
	if resp.Items[0].Type != "file" || resp.Items[0].Name != "readme.md" {
		t.Errorf("item[0] = %+v", resp.Items[0])
	}
	if resp.Items[1].Type != "directory" || resp.Items[1].Name != "docs" {
		t.Errorf("item[1] = %+v", resp.Items[1])
	}
}

func TestListDir_AddsTrailingSlash(t *testing.T) {
	var gotPath string
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/files/": func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			jsonResponse(w, 200, map[string]any{"items": []any{}})
		},
	})

	c := New(srv.URL, "s2_test")
	_, err := c.ListDir("docs") // no trailing slash
	if err != nil {
		t.Fatalf("ListDir() error: %v", err)
	}
	if !strings.HasSuffix(gotPath, "/") {
		t.Errorf("path = %q, should end with /", gotPath)
	}
}

func TestListAllRecursive_404_ReturnsEmpty(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/files/": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(404)
		},
	})

	c := New(srv.URL, "s2_test")
	result, err := c.ListAllRecursive("nonexistent/prefix/")
	if err != nil {
		t.Fatalf("ListAllRecursive() error: %v (want nil for 404)", err)
	}
	if len(result) != 0 {
		t.Errorf("got %d files, want 0 for nonexistent prefix", len(result))
	}
}

// --- /api/files (download) ---

func TestDownload_Success(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/files/": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("ETag", `"5"`)
			w.Header().Set("Content-Length", "13")
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("hello, world!"))
		},
	})

	c := New(srv.URL, "s2_test")
	dl, err := c.Download("test.txt")
	if err != nil {
		t.Fatalf("Download() error: %v", err)
	}
	defer dl.Body.Close()

	body, _ := io.ReadAll(dl.Body)
	if string(body) != "hello, world!" {
		t.Errorf("body = %q", string(body))
	}
	if dl.ContentVersion != 5 {
		t.Errorf("ContentVersion = %d, want 5", dl.ContentVersion)
	}
	if dl.Size != 13 {
		t.Errorf("Size = %d, want 13", dl.Size)
	}
}

func TestDownload_NotFound(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/files/": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(404)
		},
	})

	c := New(srv.URL, "s2_test")
	_, err := c.Download("missing.txt")
	if err != ErrNotFound {
		t.Errorf("Download() error = %v, want ErrNotFound", err)
	}
}

// --- /api/files (upload) ---

func TestUpload_IfMatch_Success(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"PUT /api/files/": func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("If-Match"); got != `"3"` {
				t.Errorf("If-Match = %q, want %q", got, `"3"`)
			}
			jsonResponse(w, 201, map[string]any{
				"id": "n1", "name": "test.txt", "size": 5, "hash": "abc", "etag": `"4"`,
			})
		},
	})

	c := New(srv.URL, "s2_test")
	result, err := c.Upload("test.txt", strings.NewReader("hello"), "", 3)
	if err != nil {
		t.Fatalf("Upload() error: %v", err)
	}
	cv, _ := ParseContentVersion(result.ETag)
	if cv != 4 {
		t.Errorf("content_version = %d, want 4", cv)
	}
}

func TestUpload_SeqInResponse(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"PUT /api/files/": func(w http.ResponseWriter, r *http.Request) {
			jsonResponse(w, 201, map[string]any{
				"id": "n1", "name": "test.txt", "size": 5, "hash": "abc", "etag": `"1"`,
				"seq": 42,
			})
		},
	})

	c := New(srv.URL, "s2_test")
	result, err := c.Upload("test.txt", strings.NewReader("hello"), "", -1)
	if err != nil {
		t.Fatalf("Upload() error: %v", err)
	}
	if result.Seq == nil {
		t.Fatal("Seq should not be nil when server returns seq")
	}
	if *result.Seq != 42 {
		t.Errorf("Seq = %d, want 42", *result.Seq)
	}
}

func TestUpload_SeqAbsentInResponse(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"PUT /api/files/": func(w http.ResponseWriter, r *http.Request) {
			jsonResponse(w, 201, map[string]any{
				"id": "n1", "name": "test.txt", "size": 5, "hash": "abc", "etag": `"1"`,
			})
		},
	})

	c := New(srv.URL, "s2_test")
	result, err := c.Upload("test.txt", strings.NewReader("hello"), "", -1)
	if err != nil {
		t.Fatalf("Upload() error: %v", err)
	}
	if result.Seq != nil {
		t.Errorf("Seq = %d, want nil (server doesn't return seq yet)", *result.Seq)
	}
}

func TestUpload_IfNoneMatch_CreateOnly(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"PUT /api/files/": func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("If-None-Match"); got != "*" {
				t.Errorf("If-None-Match = %q, want %q", got, "*")
			}
			jsonResponse(w, 201, map[string]any{
				"id": "n1", "name": "new.txt", "size": 5, "hash": "abc", "etag": `"1"`,
			})
		},
	})

	c := New(srv.URL, "s2_test")
	_, err := c.Upload("new.txt", strings.NewReader("hello"), "", 0) // 0 = If-None-Match: *
	if err != nil {
		t.Fatalf("Upload() error: %v", err)
	}
}

func TestUpload_ForceOverwrite_NoConditionalHeaders(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"PUT /api/files/": func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("If-Match") != "" || r.Header.Get("If-None-Match") != "" {
				t.Errorf("expected no conditional headers, got If-Match=%q If-None-Match=%q",
					r.Header.Get("If-Match"), r.Header.Get("If-None-Match"))
			}
			jsonResponse(w, 201, map[string]any{
				"id": "n1", "name": "f.txt", "size": 5, "hash": "abc", "etag": `"1"`,
			})
		},
	})

	c := New(srv.URL, "s2_test")
	_, err := c.Upload("f.txt", strings.NewReader("hello"), "", -1) // -1 = force
	if err != nil {
		t.Fatalf("Upload() error: %v", err)
	}
}

func TestUpload_PreconditionFailed(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"PUT /api/files/": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(412) },
	})
	c := New(srv.URL, "s2_test")
	_, err := c.Upload("test.txt", strings.NewReader("x"), "", 3)
	if err != ErrPreconditionFailed {
		t.Errorf("error = %v, want ErrPreconditionFailed", err)
	}
}

func TestUpload_Conflict(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"PUT /api/files/": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(409) },
	})
	c := New(srv.URL, "s2_test")
	_, err := c.Upload("test.txt", strings.NewReader("x"), "", 0)
	if err != ErrConflict {
		t.Errorf("error = %v, want ErrConflict", err)
	}
}

func TestUpload_StorageLimitExceeded(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"PUT /api/files/": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(413) },
	})
	c := New(srv.URL, "s2_test")
	_, err := c.Upload("test.txt", strings.NewReader("x"), "", -1)
	if err != ErrStorageLimitExceeded {
		t.Errorf("error = %v, want ErrStorageLimitExceeded", err)
	}
}

// --- /api/files (delete) ---

func TestDelete_Success_204(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"DELETE /api/files/": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) },
	})
	c := New(srv.URL, "s2_test")
	result, err := c.Delete("test.txt")
	if err != nil {
		t.Fatalf("Delete() error: %v", err)
	}
	if result.Seq != nil {
		t.Errorf("Seq should be nil for 204 response")
	}
}

func TestDelete_Success_WithSeq(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"DELETE /api/files/": func(w http.ResponseWriter, r *http.Request) {
			jsonResponse(w, 200, map[string]any{"seq": 55})
		},
	})
	c := New(srv.URL, "s2_test")
	result, err := c.Delete("test.txt")
	if err != nil {
		t.Fatalf("Delete() error: %v", err)
	}
	if result.Seq == nil {
		t.Fatal("Seq should not be nil when server returns seq")
	}
	if *result.Seq != 55 {
		t.Errorf("Seq = %d, want 55", *result.Seq)
	}
}

func TestDelete_NotFound(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"DELETE /api/files/": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) },
	})
	c := New(srv.URL, "s2_test")
	_, err := c.Delete("missing.txt")
	if err != ErrNotFound {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

// --- /api/files (head) ---

func TestHeadFile_Success(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"HEAD /api/files/": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("ETag", `"7"`)
			w.Header().Set("Content-Length", "2048")
			w.WriteHeader(200)
		},
	})
	c := New(srv.URL, "s2_test")
	cv, sz, err := c.HeadFile("test.txt")
	if err != nil {
		t.Fatalf("HeadFile() error: %v", err)
	}
	if cv != 7 {
		t.Errorf("content_version = %d, want 7", cv)
	}
	if sz != 2048 {
		t.Errorf("size = %d, want 2048", sz)
	}
}

// --- /api/changes ---

func TestPollChanges_Success(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/changes": func(w http.ResponseWriter, r *http.Request) {
			after := r.URL.Query().Get("after")
			if after != "cursor_abc" {
				t.Errorf("after = %q, want %q", after, "cursor_abc")
			}
			jsonResponse(w, 200, map[string]any{
				"changes": []map[string]any{{
					"seq": 42, "action": "put",
					"path_before": "/docs/readme.md", "path_after": "/docs/readme.md",
					"is_dir": false, "size": 100, "hash": "sha256abc",
					"created_at": "2026-04-01T00:00:00Z",
				}},
				"next_cursor":     "cursor_def",
				"resync_required": false,
			})
		},
	})

	c := New(srv.URL, "s2_test")
	resp, err := c.PollChanges("cursor_abc")
	if err != nil {
		t.Fatalf("PollChanges() error: %v", err)
	}
	if len(resp.Changes) != 1 {
		t.Fatalf("got %d changes, want 1", len(resp.Changes))
	}
	if resp.Changes[0].Seq != 42 {
		t.Errorf("seq = %d, want 42", resp.Changes[0].Seq)
	}
	if resp.NextCursor != "cursor_def" {
		t.Errorf("next_cursor = %q", resp.NextCursor)
	}
}

func TestPollChanges_CursorGone(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/changes": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(410) },
	})
	c := New(srv.URL, "s2_test")
	_, err := c.PollChanges("old_cursor")
	if err != ErrCursorGone {
		t.Errorf("error = %v, want ErrCursorGone", err)
	}
}

func TestPollChanges_NoCursor(t *testing.T) {
	var gotQuery string
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/changes": func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.RawQuery
			jsonResponse(w, 200, map[string]any{
				"changes": []any{}, "next_cursor": "cursor_init", "resync_required": false,
			})
		},
	})

	c := New(srv.URL, "s2_test")
	_, err := c.PollChanges("") // empty cursor
	if err != nil {
		t.Fatalf("PollChanges() error: %v", err)
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want empty (no after param)", gotQuery)
	}
}

// --- /api/changes/latest ---

func TestLatestCursor_Success(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/changes/latest": func(w http.ResponseWriter, r *http.Request) {
			jsonResponse(w, 200, map[string]string{"cursor": "cursor_xyz"})
		},
	})

	c := New(srv.URL, "s2_test")
	cursor, err := c.LatestCursor()
	if err != nil {
		t.Fatalf("LatestCursor() error: %v", err)
	}
	if cursor != "cursor_xyz" {
		t.Errorf("cursor = %q, want %q", cursor, "cursor_xyz")
	}
}

// Bug fix: LatestCursor was missing checkStatus, so 401/403/etc. returned
// generic errors instead of sentinel errors.
func TestLatestCursor_Unauthorized(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/changes/latest": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(401)
		},
	})
	c := New(srv.URL, "s2_bad")
	_, err := c.LatestCursor()
	if err != ErrUnauthorized {
		t.Errorf("LatestCursor() error = %v, want ErrUnauthorized", err)
	}
}

func TestLatestCursor_Forbidden(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/changes/latest": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(403)
		},
	})
	c := New(srv.URL, "s2_test")
	_, err := c.LatestCursor()
	if err != ErrForbidden {
		t.Errorf("LatestCursor() error = %v, want ErrForbidden", err)
	}
}

// --- Chunked upload ---

func TestChunkedUpload_FullFlow(t *testing.T) {
	var steps []string

	srv := newTestServer(t, map[string]http.HandlerFunc{
		"POST /api/uploads": func(w http.ResponseWriter, r *http.Request) {
			steps = append(steps, "create")
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["path"] != "big.bin" {
				t.Errorf("path = %v", body["path"])
			}
			jsonResponse(w, 201, map[string]any{
				"sessionId": "sess_1", "nodeId": "n1",
				"chunkSize": 4194304, "expiresAt": "2026-04-10T00:00:00Z",
			})
		},
		"PUT /api/uploads/": func(w http.ResponseWriter, r *http.Request) {
			steps = append(steps, "chunk")
			w.WriteHeader(200)
		},
		"POST /api/uploads/": func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/complete") {
				steps = append(steps, "complete")
				jsonResponse(w, 200, map[string]any{
					"id": "n1", "name": "big.bin", "size": 1000, "hash": "abc", "etag": `"1"`,
				})
			}
		},
	})

	c := New(srv.URL, "s2_test")

	session, err := c.CreateUploadSession("big.bin", 1000, 1)
	if err != nil {
		t.Fatalf("CreateUploadSession() error: %v", err)
	}
	if session.SessionID != "sess_1" {
		t.Errorf("SessionID = %q", session.SessionID)
	}
	if session.ChunkSize != 4194304 {
		t.Errorf("ChunkSize = %d", session.ChunkSize)
	}

	if err := c.UploadChunk("sess_1", 0, strings.NewReader("data")); err != nil {
		t.Fatalf("UploadChunk() error: %v", err)
	}

	result, err := c.CompleteUpload("sess_1")
	if err != nil {
		t.Fatalf("CompleteUpload() error: %v", err)
	}
	if result.Size != 1000 {
		t.Errorf("size = %d", result.Size)
	}

	if len(steps) != 3 || steps[0] != "create" || steps[1] != "chunk" || steps[2] != "complete" {
		t.Errorf("steps = %v, want [create, chunk, complete]", steps)
	}
}

func TestCompleteUpload_SeqInResponse(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"POST /api/uploads/": func(w http.ResponseWriter, r *http.Request) {
			jsonResponse(w, 200, map[string]any{
				"id": "n1", "name": "big.bin", "size": 1000, "hash": "abc", "etag": `"1"`,
				"seq": 99,
			})
		},
	})

	c := New(srv.URL, "s2_test")
	result, err := c.CompleteUpload("sess_1")
	if err != nil {
		t.Fatalf("CompleteUpload() error: %v", err)
	}
	if result.Seq == nil {
		t.Fatal("Seq should not be nil")
	}
	if *result.Seq != 99 {
		t.Errorf("Seq = %d, want 99", *result.Seq)
	}
}

// --- Move ---

// --- /api/tokens ---

func TestCreateToken_Success(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"POST /api/tokens": func(w http.ResponseWriter, r *http.Request) {
			requireAuth(t, r, "s2_parent")
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["name"] != "child" {
				t.Errorf("name = %v", body["name"])
			}
			jsonResponse(w, 201, map[string]any{
				"token": map[string]any{
					"id": "tok_child", "name": "child", "base_path": "/",
					"can_delegate": false, "origin": "delegation",
					"origin_id": "tok_parent", "created_at": "2026-04-04T00:00:00Z",
					"access_paths": []map[string]any{
						{"path": "/", "can_read": true, "can_write": true},
					},
				},
				"raw_token": "s2_childtoken123",
			})
		},
	})

	c := New(srv.URL, "s2_parent")
	resp, err := c.CreateToken("child", "/", false, []types.AccessPath{
		{Path: "/", CanRead: true, CanWrite: true},
	})
	if err != nil {
		t.Fatalf("CreateToken() error: %v", err)
	}
	if resp.RawToken != "s2_childtoken123" {
		t.Errorf("raw_token = %q", resp.RawToken)
	}
	if resp.Token.ID != "tok_child" {
		t.Errorf("token.id = %q", resp.Token.ID)
	}
	if resp.Token.Origin != "delegation" {
		t.Errorf("origin = %q", resp.Token.Origin)
	}
}

func TestCreateToken_Forbidden(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"POST /api/tokens": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(403)
		},
	})
	c := New(srv.URL, "s2_test")
	_, err := c.CreateToken("child", "/", false, nil)
	if err != ErrForbidden {
		t.Errorf("error = %v, want ErrForbidden", err)
	}
}

// --- /api/file-moves ---

func TestMove_Success(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"POST /api/file-moves/": func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["destination"] != "new/path.txt" {
				t.Errorf("destination = %v", body["destination"])
			}
			jsonResponse(w, 200, map[string]any{"id": "n1"})
		},
	})

	c := New(srv.URL, "s2_test")
	if err := c.Move("old/path.txt", "new/path.txt", false); err != nil {
		t.Fatalf("Move() error: %v", err)
	}
}

// --- /api/snapshot (ADR 0039) ---

func TestSnapshot_ScopeRoot(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/snapshot": func(w http.ResponseWriter, r *http.Request) {
			requireAuth(t, r, "s2_test")
			if r.URL.RawQuery != "" {
				t.Errorf("expected empty query, got %q", r.URL.RawQuery)
			}
			jsonResponse(w, 200, map[string]any{
				"items": []any{
					map[string]any{
						"path":            "/docs/",
						"type":            "dir",
						"content_version": 0,
						"revision_id":     nil,
						"size":            nil,
						"hash":            nil,
						"content_type":    "inode/directory",
					},
					map[string]any{
						"path":            "/docs/a.txt",
						"type":            "file",
						"content_version": 1,
						"revision_id":     "rev_01",
						"size":            11,
						"hash":            "h-a",
						"content_type":    "text/plain",
					},
				},
				"cursor": "cursor_abc",
			})
		},
	})

	c := New(srv.URL, "s2_test")
	resp, err := c.Snapshot("")
	if err != nil {
		t.Fatalf("Snapshot() error: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(resp.Items))
	}
	if resp.Cursor != "cursor_abc" {
		t.Errorf("cursor = %q", resp.Cursor)
	}
	if resp.Items[1].Type != "file" || resp.Items[1].RevisionID != "rev_01" {
		t.Errorf("unexpected item[1]: %+v", resp.Items[1])
	}
	if resp.Items[1].Size == nil || *resp.Items[1].Size != 11 {
		t.Errorf("size = %v, want 11", resp.Items[1].Size)
	}
}

func TestSnapshot_WithPath(t *testing.T) {
	var capturedQuery string
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/snapshot": func(w http.ResponseWriter, r *http.Request) {
			capturedQuery = r.URL.RawQuery
			jsonResponse(w, 200, map[string]any{
				"items":  []any{},
				"cursor": "cursor_sub",
			})
		},
	})

	c := New(srv.URL, "s2_test")
	if _, err := c.Snapshot("/vacation/2024"); err != nil {
		t.Fatalf("Snapshot() error: %v", err)
	}
	if want := "path=%2Fvacation%2F2024"; capturedQuery != want {
		t.Errorf("query = %q, want %q", capturedQuery, want)
	}
}

func TestSnapshot_NotFound(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/snapshot": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) },
	})
	c := New(srv.URL, "s2_test")
	_, err := c.Snapshot("/missing")
	if err != ErrNotFound {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestSnapshot_SubtreeCapExceeded(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/snapshot": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(413)
			w.Write([]byte(`{"error":{"code":"subtree_cap_exceeded","message":"too big"}}`))
		},
	})
	c := New(srv.URL, "s2_test")
	_, err := c.Snapshot("/huge")
	if err != ErrSubtreeCapExceeded {
		t.Errorf("error = %v, want ErrSubtreeCapExceeded", err)
	}
}

func TestSnapshot_Unauthorized(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/snapshot": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) },
	})
	c := New(srv.URL, "s2_test")
	_, err := c.Snapshot("")
	if err != ErrUnauthorized {
		t.Errorf("error = %v, want ErrUnauthorized", err)
	}
}

// --- /api/revisions/:id (GET, ADR 0040) ---

func TestDownloadRevision_Success(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/revisions/": func(w http.ResponseWriter, r *http.Request) {
			requireAuth(t, r, "s2_test")
			if r.URL.Path != "/api/revisions/rev_42" {
				t.Errorf("path = %q", r.URL.Path)
			}
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("ETag", `"7"`)
			w.Header().Set("Content-Length", "5")
			w.WriteHeader(200)
			w.Write([]byte("hello"))
		},
	})

	c := New(srv.URL, "s2_test")
	dl, err := c.DownloadRevision("rev_42")
	if err != nil {
		t.Fatalf("DownloadRevision() error: %v", err)
	}
	defer dl.Body.Close()
	data, _ := io.ReadAll(dl.Body)
	if string(data) != "hello" {
		t.Errorf("body = %q, want %q", data, "hello")
	}
	if dl.ContentVersion != 7 {
		t.Errorf("content_version = %d, want 7", dl.ContentVersion)
	}
	if dl.ContentType != "text/plain" {
		t.Errorf("content_type = %q", dl.ContentType)
	}
	if dl.Size != 5 {
		t.Errorf("size = %d, want 5", dl.Size)
	}
}

func TestDownloadRevision_NotFound(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/revisions/": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) },
	})
	c := New(srv.URL, "s2_test")
	_, err := c.DownloadRevision("rev_missing")
	if err != ErrNotFound {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

// Ensure the unused types import keeps the compiler happy if new tests
// reference no types.* symbols directly.
var _ = types.SnapshotItem{}
