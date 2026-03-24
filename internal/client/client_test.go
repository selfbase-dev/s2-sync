package client

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// fakeAPI creates an httptest server that mimics /api/files and /api/me.
// handlers is a map of "METHOD /path" → handler function.
func fakeAPI(t *testing.T, handlers map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		// Try exact match first
		if h, ok := handlers[key]; ok {
			h(w, r)
			return
		}
		// Try prefix match for catch-all routes
		for pattern, h := range handlers {
			parts := strings.SplitN(pattern, " ", 2)
			if len(parts) == 2 && r.Method == parts[0] && strings.HasPrefix(r.URL.Path, parts[1]) {
				h(w, r)
				return
			}
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
		http.Error(w, "not found", 404)
	}))
}

// requireAuth checks Authorization header and returns false (writing 401) if invalid.
func requireAuth(w http.ResponseWriter, r *http.Request, token string) bool {
	if r.Header.Get("Authorization") != "Bearer "+token {
		http.Error(w, "unauthorized", 401)
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Validate
// ---------------------------------------------------------------------------

func TestValidate_Success(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/me": func(w http.ResponseWriter, r *http.Request) {
			if !requireAuth(w, r, "s2_validtoken") {
				return
			}
			w.WriteHeader(200)
			fmt.Fprint(w, `{"user_id":"u1","email":"test@example.com"}`)
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_validtoken")
	if err := c.Validate(); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}

func TestValidate_InvalidToken(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/me": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":"Unauthorized"}`)
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_badtoken")
	err := c.Validate()
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
	if !strings.Contains(err.Error(), "invalid or expired token") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ListAll
// ---------------------------------------------------------------------------

func TestListAll_Basic(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/files/": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"key": "docs/a.txt", "size": 100, "uploaded": "2026-03-22T10:00:00Z", "hash": "abc123"},
					{"key": "docs/b.txt", "size": 200, "uploaded": "2026-03-22T11:00:00Z", "hash": "def456"},
				},
			})
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_test")
	objects, err := c.ListAll("docs/")
	if err != nil {
		t.Fatalf("ListAll failed: %v", err)
	}
	if len(objects) != 2 {
		t.Fatalf("expected 2 objects, got %d", len(objects))
	}
	if objects[0].Key != "docs/a.txt" {
		t.Errorf("expected key docs/a.txt, got %s", objects[0].Key)
	}
	if objects[0].ETag != "abc123" {
		t.Errorf("expected etag abc123, got %s", objects[0].ETag)
	}
	if objects[1].Size != 200 {
		t.Errorf("expected size 200, got %d", objects[1].Size)
	}
}

func TestListAll_EmptyDirectory(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/files/": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_test")
	objects, err := c.ListAll("")
	if err != nil {
		t.Fatalf("ListAll failed: %v", err)
	}
	if len(objects) != 0 {
		t.Fatalf("expected 0 objects, got %d", len(objects))
	}
}

func TestListAll_ManyFiles(t *testing.T) {
	// Simulate a project with 500 files (git repo, node_modules etc.)
	const fileCount = 500
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/files/": func(w http.ResponseWriter, r *http.Request) {
			items := make([]map[string]any, fileCount)
			for i := range items {
				items[i] = map[string]any{
					"key":      fmt.Sprintf("project/src/file_%04d.ts", i),
					"size":     int64(i * 100),
					"uploaded": "2026-03-22T10:00:00Z",
					"hash":     fmt.Sprintf("hash_%04d", i),
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"items": items})
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_test")
	objects, err := c.ListAll("project/")
	if err != nil {
		t.Fatalf("ListAll failed: %v", err)
	}
	if len(objects) != fileCount {
		t.Fatalf("expected %d objects, got %d", fileCount, len(objects))
	}
	// Spot check first and last
	if objects[0].Key != "project/src/file_0000.ts" {
		t.Errorf("first key: got %s", objects[0].Key)
	}
	if objects[fileCount-1].ETag != fmt.Sprintf("hash_%04d", fileCount-1) {
		t.Errorf("last etag: got %s", objects[fileCount-1].ETag)
	}
}

func TestListAll_SpecialCharacters(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/files/": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"key": "docs/日本語ファイル.txt", "size": 50, "uploaded": "2026-03-22T10:00:00Z", "hash": "jp1"},
					{"key": "docs/file with spaces.txt", "size": 60, "uploaded": "2026-03-22T10:00:00Z", "hash": "sp1"},
					{"key": "docs/special-chars_v2.0 (1).txt", "size": 70, "uploaded": "2026-03-22T10:00:00Z", "hash": "sc1"},
				},
			})
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_test")
	objects, err := c.ListAll("docs/")
	if err != nil {
		t.Fatalf("ListAll failed: %v", err)
	}
	if len(objects) != 3 {
		t.Fatalf("expected 3, got %d", len(objects))
	}
	if objects[0].Key != "docs/日本語ファイル.txt" {
		t.Errorf("japanese filename: got %s", objects[0].Key)
	}
}

func TestListAll_DeepNestedPaths(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/files/": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"key": "project/src/components/ui/buttons/primary/index.tsx", "size": 100, "uploaded": "2026-03-22T10:00:00Z", "hash": "deep1"},
					{"key": "project/.github/workflows/ci.yml", "size": 200, "uploaded": "2026-03-22T10:00:00Z", "hash": "deep2"},
				},
			})
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_test")
	objects, err := c.ListAll("project/")
	if err != nil {
		t.Fatalf("ListAll failed: %v", err)
	}
	if len(objects) != 2 {
		t.Fatalf("expected 2, got %d", len(objects))
	}
}

func TestListAll_ServerError(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/files/": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "internal server error", 500)
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_test")
	_, err := c.ListAll("docs/")
	if err == nil {
		t.Fatal("expected error on server error")
	}
}

func TestListAll_PrefixPassedAsQueryParam(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/files/": func(w http.ResponseWriter, r *http.Request) {
			// Verify the prefix is sent as path, not query
			path := r.URL.Path
			if !strings.HasSuffix(path, "/") {
				t.Errorf("expected path ending with /, got %s", path)
			}
			json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_test")
	_, _ = c.ListAll("myprefix/")
}

// ---------------------------------------------------------------------------
// GetObject
// ---------------------------------------------------------------------------

func TestGetObject_Success(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/files/": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("ETag", `"abc123"`)
			fmt.Fprint(w, "file content here")
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_test")
	body, etag, err := c.GetObject("docs/test.txt")
	if err != nil {
		t.Fatalf("GetObject failed: %v", err)
	}
	defer body.Close()

	data, _ := io.ReadAll(body)
	if string(data) != "file content here" {
		t.Errorf("expected 'file content here', got %q", string(data))
	}
	if etag != "abc123" {
		t.Errorf("expected etag abc123, got %s", etag)
	}
}

func TestGetObject_NotFound(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/files/": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Not Found", 404)
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_test")
	_, _, err := c.GetObject("nonexistent.txt")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

func TestGetObject_LargeFile(t *testing.T) {
	// 1MB file
	content := strings.Repeat("x", 1024*1024)
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/files/": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("ETag", `"largehash"`)
			fmt.Fprint(w, content)
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_test")
	body, _, err := c.GetObject("large.bin")
	if err != nil {
		t.Fatalf("GetObject failed: %v", err)
	}
	defer body.Close()

	data, _ := io.ReadAll(body)
	if len(data) != 1024*1024 {
		t.Errorf("expected 1MB, got %d bytes", len(data))
	}
}

// ---------------------------------------------------------------------------
// PutObject
// ---------------------------------------------------------------------------

func TestPutObject_Success(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"PUT /api/files/": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if string(body) != "hello world" {
				t.Errorf("unexpected body: %q", string(body))
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{
				"size": len(body),
				"hash": "sha256_abc",
				"etag": "md5_xyz",
			})
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_test")
	etag, err := c.PutObject("docs/test.txt", strings.NewReader("hello world"), "")
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}
	if etag != "md5_xyz" {
		t.Errorf("expected etag md5_xyz, got %s", etag)
	}
}

func TestPutObject_IfMatch_Conflict(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"PUT /api/files/": func(w http.ResponseWriter, r *http.Request) {
			ifMatch := r.Header.Get("If-Match")
			if ifMatch == "" {
				t.Error("expected If-Match header")
			}
			w.WriteHeader(412)
			fmt.Fprint(w, `{"error":"Precondition Failed"}`)
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_test")
	_, err := c.PutObject("test.txt", strings.NewReader("data"), "old_etag")
	if err != ErrPreconditionFailed {
		t.Errorf("expected ErrPreconditionFailed, got %v", err)
	}
}

func TestPutObject_IfMatch_Success(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"PUT /api/files/": func(w http.ResponseWriter, r *http.Request) {
			ifMatch := r.Header.Get("If-Match")
			if ifMatch != `"current_etag"` {
				t.Errorf("expected If-Match \"current_etag\", got %s", ifMatch)
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{
				"size": 4,
				"hash": "sha_new",
				"etag": "new_etag",
			})
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_test")
	etag, err := c.PutObject("test.txt", strings.NewReader("data"), "current_etag")
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}
	if etag != "new_etag" {
		t.Errorf("expected new_etag, got %s", etag)
	}
}

func TestPutObject_StorageLimitExceeded(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"PUT /api/files/": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(413)
			fmt.Fprint(w, `{"error":"Storage limit exceeded"}`)
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_test")
	_, err := c.PutObject("big.bin", strings.NewReader("data"), "")
	if err == nil {
		t.Fatal("expected error for 413")
	}
	if !strings.Contains(err.Error(), "413") {
		t.Errorf("expected 413 in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// DeleteObject
// ---------------------------------------------------------------------------

func TestDeleteObject_Success(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"DELETE /api/files/": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(204)
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_test")
	err := c.DeleteObject("docs/old.txt")
	if err != nil {
		t.Fatalf("DeleteObject failed: %v", err)
	}
}

func TestDeleteObject_NotFound(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"DELETE /api/files/": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Not Found", 404)
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_test")
	err := c.DeleteObject("nonexistent.txt")
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

// ---------------------------------------------------------------------------
// Change Log API (unchanged, but verify still works)
// ---------------------------------------------------------------------------

func TestPollChanges_Success(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/changes": func(w http.ResponseWriter, r *http.Request) {
			after := r.URL.Query().Get("after")
			if after != "5" {
				t.Errorf("expected after=5, got %s", after)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"changes": []map[string]any{
					{"seq": 6, "path": "/docs/new.txt", "action": "put", "size": 100},
					{"seq": 7, "path": "/docs/old.txt", "action": "delete"},
				},
			})
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_test")
	changes, err := c.PollChanges(5, 100)
	if err != nil {
		t.Fatalf("PollChanges failed: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("expected 2 changes, got %d", len(changes))
	}
	if changes[0].Seq != 6 {
		t.Errorf("expected seq 6, got %d", changes[0].Seq)
	}
}

func TestPollChanges_CursorInvalid(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/changes": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(410)
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_test")
	_, err := c.PollChanges(1, 100)
	if err != ErrCursorInvalid {
		t.Errorf("expected ErrCursorInvalid, got %v", err)
	}
}

func TestLatestCursor(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/changes/latest": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"latest": 42})
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_test")
	cursor, err := c.LatestCursor()
	if err != nil {
		t.Fatalf("LatestCursor failed: %v", err)
	}
	if cursor != 42 {
		t.Errorf("expected 42, got %d", cursor)
	}
}

// ---------------------------------------------------------------------------
// Edge cases & stress tests
// ---------------------------------------------------------------------------

func TestListAll_InvalidJSON(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/files/": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "this is not json{{{")
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_test")
	_, err := c.ListAll("")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestGetObject_EmptyETag(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/files/": func(w http.ResponseWriter, r *http.Request) {
			// No ETag header
			fmt.Fprint(w, "content")
		},
	})
	defer srv.Close()

	c := New(srv.URL, "s2_test")
	body, etag, err := c.GetObject("test.txt")
	if err != nil {
		t.Fatalf("GetObject failed: %v", err)
	}
	defer body.Close()
	io.ReadAll(body)

	if etag != "" {
		t.Errorf("expected empty etag, got %s", etag)
	}
}

func TestAuthHeaderSentOnAllRequests(t *testing.T) {
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer s2_mytoken" {
			t.Errorf("missing/wrong auth header on %s %s: got %q", r.Method, r.URL.Path, auth)
		}
		callCount.Add(1)

		switch {
		case r.Method == "GET" && r.URL.Path == "/api/me":
			w.WriteHeader(200)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/files/"):
			json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
		case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/api/files/"):
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"size": 0, "hash": "", "etag": ""})
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/api/files/"):
			w.WriteHeader(204)
		default:
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "s2_mytoken")
	_ = c.Validate()
	_, _ = c.ListAll("")
	c.PutObject("x.txt", strings.NewReader("x"), "")
	c.DeleteObject("x.txt")

	if callCount.Load() != 4 {
		t.Errorf("expected 4 calls, got %d", callCount.Load())
	}
}

func TestEndpointTrailingSlash(t *testing.T) {
	srv := fakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/me": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
		},
	})
	defer srv.Close()

	// Endpoint with trailing slash should still work
	c := New(srv.URL+"/", "s2_test")
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate with trailing slash failed: %v", err)
	}
}

func TestPutObject_URLEncoding(t *testing.T) {
	var receivedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"size": 4, "hash": "h", "etag": "e"})
	}))
	defer srv.Close()

	c := New(srv.URL, "s2_test")
	_, _ = c.PutObject("path/to/日本語.txt", strings.NewReader("data"), "")

	if !strings.Contains(receivedPath, "/api/files/path/to/") {
		t.Errorf("expected /api/files/path/to/... , got %s", receivedPath)
	}
}
