package sync_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/selfbase-dev/s2-sync/internal/auth"
	"github.com/selfbase-dev/s2-sync/internal/client"
	"github.com/selfbase-dev/s2-sync/internal/oauth"
)

// TestE2E_RefreshThenIntrospect wires the full credential pipeline
// end-to-end against a mocked S2 server: a near-expired session in
// the keyring triggers a transparent /oauth/token rotation on the
// first API call, the new bearer is injected via bearerTransport, and
// the rotated credentials are persisted for the next process. This
// is the load-bearing path for every long-running sync — without it
// the desktop service would fail silently after the first hour.
//
// The test is placed in `sync_test` rather than `auth` to keep
// internal/auth free of `internal/client` imports (which would create
// a cycle: client → auth).
func TestE2E_RefreshThenIntrospect(t *testing.T) {
	dataPtr, restore := auth.SwapKeyringForTesting()
	t.Cleanup(restore)

	var refreshHits, introspectHits int32
	var lastAuth atomic.Value // string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			atomic.AddInt32(&refreshHits, 1)
			body, _ := io.ReadAll(r.Body)
			form := string(body)
			if !strings.Contains(form, "grant_type=refresh_token") {
				t.Errorf("refresh body = %q, want grant_type=refresh_token", form)
			}
			if !strings.Contains(form, "refresh_token=ref_old") {
				t.Errorf("refresh body = %q, want refresh_token=ref_old", form)
			}
			if !strings.Contains(form, "client_id=client-abc") {
				t.Errorf("refresh body = %q, want client_id=client-abc", form)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(oauth.TokenResponse{
				AccessToken:  "s2_new_access",
				RefreshToken: "s2_new_refresh",
				ExpiresIn:    3600,
				TokenType:    "Bearer",
			})

		case "/api/v1/token":
			atomic.AddInt32(&introspectHits, 1)
			lastAuth.Store(r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"user_id": "user_42", "token_id": "tok_42",
				"can_delegate": false,
				"access_paths": []map[string]any{{"path": "/", "can_read": true, "can_write": true}},
			})

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)

	// Seed a near-expired session pointing at the mock — same shape
	// `s2 login` would have produced after a successful PKCE flow.
	seed := &auth.Session{
		Version:         auth.SessionSchemaVersion,
		Kind:            "oauth",
		Endpoint:        srv.URL,
		ClientID:        "client-abc",
		AccessToken:     "s2_old_access",
		RefreshToken:    "ref_old",
		AccessExpiresAt: time.Now().Add(5 * time.Second), // within refreshSkew
	}
	if err := auth.WriteSessionForTesting(seed); err != nil {
		t.Fatalf("WriteSessionForTesting: %v", err)
	}

	source, err := auth.NewSource(srv.URL)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}

	c := client.New(srv.URL, source)
	ti, err := c.Introspect()
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	if ti.UserID != "user_42" {
		t.Errorf("user_id = %q, want user_42", ti.UserID)
	}

	if got := atomic.LoadInt32(&refreshHits); got != 1 {
		t.Errorf("refresh hits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&introspectHits); got != 1 {
		t.Errorf("introspect hits = %d, want 1", got)
	}
	if got, _ := lastAuth.Load().(string); got != "Bearer s2_new_access" {
		t.Errorf("Authorization = %q, want \"Bearer s2_new_access\"", got)
	}

	// Rotated tokens must be persisted so a process restart starts
	// from the fresh credentials.
	var persisted auth.Session
	if err := json.Unmarshal([]byte(*dataPtr), &persisted); err != nil {
		t.Fatalf("unmarshal persisted: %v", err)
	}
	if persisted.AccessToken != "s2_new_access" || persisted.RefreshToken != "s2_new_refresh" {
		t.Errorf("persisted = %+v, want rotated tokens", persisted)
	}
	if persisted.ClientID != "client-abc" {
		t.Errorf("persisted client_id = %q, want client-abc", persisted.ClientID)
	}

	// A second API call inside the new skew window must NOT refresh
	// again — proves the rotated expiry is honoured in-memory.
	if _, err := c.Introspect(); err != nil {
		t.Fatalf("second Introspect: %v", err)
	}
	if got := atomic.LoadInt32(&refreshHits); got != 1 {
		t.Errorf("after second call, refresh hits = %d, want still 1", got)
	}
}

// TestE2E_InvalidGrantWipesSession is the failure path of the smoke:
// when the refresh token is permanently rejected (server returns
// invalid_grant), the session must be wiped so the next process boots
// into a clean re-login state instead of looping on a dead grant.
func TestE2E_InvalidGrantWipesSession(t *testing.T) {
	dataPtr, restore := auth.SwapKeyringForTesting()
	t.Cleanup(restore)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token revoked"}`))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(500)
	}))
	t.Cleanup(srv.Close)

	seed := &auth.Session{
		Version:         auth.SessionSchemaVersion,
		Kind:            "oauth",
		Endpoint:        srv.URL,
		ClientID:        "client-abc",
		AccessToken:     "old",
		RefreshToken:    "dead",
		AccessExpiresAt: time.Now().Add(-time.Minute), // already expired
	}
	if err := auth.WriteSessionForTesting(seed); err != nil {
		t.Fatalf("WriteSessionForTesting: %v", err)
	}

	source, err := auth.NewSource(srv.URL)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}

	c := client.New(srv.URL, source)
	if _, err := c.Introspect(); err == nil {
		t.Fatal("Introspect: want error after invalid_grant, got nil")
	}
	if *dataPtr != "" {
		t.Errorf("session not wiped after invalid_grant: %q", *dataPtr)
	}
}
