package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/selfbase-dev/s2-sync/internal/auth"
)

// TestClient_RetriesOn5xx_ThenSuccess verifies the retryable transport
// retries transient 5xx responses and eventually returns the successful
// payload. Without this the desktop service would surface every
// momentary edge / gateway blip as a sync failure.
func TestClient_RetriesOn5xx_ThenSuccess(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte("upstream unavailable"))
			return
		}
		jsonResponse(w, 200, map[string]any{
			"user_id": "user_1", "token_id": "tok_1",
			"can_delegate": false,
			"access_paths": []map[string]any{{"path": "/", "can_read": true, "can_write": true}},
		})
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, auth.NewStaticSource("s2_test"))
	ti, err := c.Introspect()
	if err != nil {
		t.Fatalf("Introspect after retry: %v", err)
	}
	if ti.UserID != "user_1" {
		t.Errorf("user_id = %q, want user_1", ti.UserID)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("hits = %d, want 2 (1 retry after 503)", got)
	}
}

// TestClient_DoesNotRetryOn4xx confirms the CheckRetry override turns
// off retries for client-side errors. Retrying a 401/403/404 is wasted
// time and can mask the real cause.
func TestClient_DoesNotRetryOn4xx(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(401)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, auth.NewStaticSource("s2_test"))
	_, err := c.Introspect()
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Introspect on 401: %v, want ErrUnauthorized", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("hits = %d, want 1 (4xx must NOT retry)", got)
	}
}

// TestClient_RetriesOnConnectionError verifies that a server that
// closes the connection mid-flight is retried (per
// retryablehttp.DefaultRetryPolicy). This is the common laptop-sleep
// scenario: network reappears after a brief outage.
func TestClient_RetriesOnConnectionError(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			// Hijack and close to simulate connection drop.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("hijacker not supported")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatalf("hijack: %v", err)
			}
			conn.Close()
			return
		}
		jsonResponse(w, 200, map[string]any{
			"user_id": "user_2", "token_id": "tok_2",
			"can_delegate": false,
			"access_paths": []map[string]any{{"path": "/", "can_read": true, "can_write": true}},
		})
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, auth.NewStaticSource("s2_test"))
	ti, err := c.Introspect()
	if err != nil {
		t.Fatalf("Introspect after connection drop: %v", err)
	}
	if ti.UserID != "user_2" {
		t.Errorf("user_id = %q, want user_2", ti.UserID)
	}
	if got := atomic.LoadInt32(&hits); got < 2 {
		t.Errorf("hits = %d, want >= 2 (connection drop must retry)", got)
	}
}

// TestClient_ContextCancelAbortsRequest covers the desktop Stop path:
// cancelling the per-sync context must interrupt in-flight downloads
// instead of waiting for the body to finish streaming. Without this,
// "Stop sync" would block on a slow remote response.
func TestClient_ContextCancelAbortsRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("ETag", `"1"`)
		w.(http.Flusher).Flush()
		close(started)
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	c := New(srv.URL, auth.NewStaticSource("s2_test")).WithContext(ctx)

	done := make(chan error, 1)
	go func() {
		_, err := c.Download("slow.bin")
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("server never received request")
	}
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Download returned nil after cancel; want context error")
		}
		if !strings.Contains(err.Error(), "context") {
			t.Errorf("err = %v, want context error", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Download did not return after context cancel")
	}
}

// TestClient_CancelUpload_Success ensures the chunked-upload abort path
// hits the right URL and treats 204 as success. Used by the runner when
// the user stops a sync mid-upload.
func TestClient_CancelUpload_Success(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("method = %q, want DELETE", r.Method)
		}
		gotPath = r.URL.Path
		w.WriteHeader(204)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, auth.NewStaticSource("s2_test"))
	if err := c.CancelUpload("sess_xyz"); err != nil {
		t.Fatalf("CancelUpload: %v", err)
	}
	if gotPath != "/api/v1/uploads/sess_xyz" {
		t.Errorf("path = %q, want /api/v1/uploads/sess_xyz", gotPath)
	}
}

// TestClient_CancelUpload_UnexpectedStatus surfaces a bad server
// response instead of silently treating it as success. Otherwise a
// 200/500 response would leave the session believed-cancelled but still
// alive server-side.
func TestClient_CancelUpload_UnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, auth.NewStaticSource("s2_test"))
	if err := c.CancelUpload("sess_1"); err == nil {
		t.Fatal("CancelUpload on 200: want error, got nil")
	}
}

// TestClient_CreateUploadSession_4xx returns an error from the server's
// own status without retrying (4xx are non-retryable client errors).
func TestClient_CreateUploadSession_4xx(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"invalid_path"}`))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, auth.NewStaticSource("s2_test"))
	_, err := c.CreateUploadSession("../../etc/passwd", 100, 1)
	if err == nil {
		t.Fatal("CreateUploadSession on 400: want error, got nil")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("hits = %d, want 1 (4xx must not retry)", got)
	}
}

// TestClient_UploadChunk_5xxRetriesThenSucceeds confirms the same
// retry policy applies to chunk PUTs — a single 502 mid-upload must
// not abort the whole multi-GB transfer.
func TestClient_UploadChunk_5xxRetriesThenSucceeds(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		// Drain the body so retryablehttp can rewind via the Seeker.
		_, _ = io.Copy(io.Discard, r.Body)
		if n == 1 {
			w.WriteHeader(502)
			return
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, auth.NewStaticSource("s2_test"))
	body := strings.NewReader("chunk-data")
	if err := c.UploadChunk("sess_1", 0, body); err != nil {
		t.Fatalf("UploadChunk after retry: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("hits = %d, want 2", got)
	}
}

// TestClient_CompleteUpload_4xxNonRetryable ensures a 422 (e.g. chunk
// hash mismatch on the server) surfaces immediately so the runner can
// recreate the session, not retry into the same wall.
func TestClient_CompleteUpload_4xxNonRetryable(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(422)
		_, _ = w.Write([]byte(`{"error":"chunk_hash_mismatch"}`))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, auth.NewStaticSource("s2_test"))
	_, err := c.CompleteUpload("sess_1")
	if err == nil {
		t.Fatal("CompleteUpload on 422: want error, got nil")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("hits = %d, want 1 (4xx must not retry)", got)
	}
}
