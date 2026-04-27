package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/selfbase-dev/s2-sync/internal/oauth"
)

// fakeTokenServer mimics POST /oauth/token. Counts hits so refresh
// tests can assert the server was contacted exactly once across
// concurrent goroutines.
func fakeTokenServer(t *testing.T, status int, body string, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

// TestSessionSource_TokenWithinSkew_NoRefresh: an access token whose
// expiry is comfortably ahead of refreshSkew should be returned as-is.
// Touching /oauth/token here would burn a refresh-token rotation for
// no reason.
func TestSessionSource_TokenWithinSkew_NoRefresh(t *testing.T) {
	swapKeyring(t)
	var hits int32
	srv := fakeTokenServer(t, 200, `{"access_token":"X","refresh_token":"Y","expires_in":3600}`, &hits)
	defer srv.Close()

	s := &sessionSource{
		endpoint: srv.URL,
		sess: Session{
			Version:         sessionSchemaVersion,
			Kind:            "oauth",
			Endpoint:        srv.URL,
			AccessToken:     "current",
			RefreshToken:    "ref",
			AccessExpiresAt: time.Now().Add(10 * time.Minute),
		},
	}
	tok, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "current" {
		t.Errorf("got %q, want 'current'", tok)
	}
	if hits != 0 {
		t.Errorf("unexpected refresh hits: %d", hits)
	}
}

// TestSessionSource_RefreshNearExpiry: when the access is within
// refreshSkew, Token() must rotate and persist new credentials.
func TestSessionSource_RefreshNearExpiry(t *testing.T) {
	fk := swapKeyring(t)
	var hits int32
	srv := fakeTokenServer(t, 200, `{"access_token":"new_a","refresh_token":"new_r","expires_in":3600}`, &hits)
	defer srv.Close()

	// Seed the keyring so writeSession's persisted state can be
	// observed (sessionSource updates its in-memory copy + keyring).
	seed := &Session{
		Version:         sessionSchemaVersion,
		Kind:            "oauth",
		Endpoint:        srv.URL,
		AccessToken:     "old_a",
		RefreshToken:    "old_r",
		AccessExpiresAt: time.Now().Add(10 * time.Second), // within skew
	}
	_ = writeSession(seed)

	s := &sessionSource{endpoint: srv.URL, sess: *seed}
	tok, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "new_a" {
		t.Errorf("got %q, want new_a", tok)
	}
	if hits != 1 {
		t.Errorf("hits: %d, want 1", hits)
	}

	// Persisted copy reflects rotation.
	var persisted Session
	if err := json.Unmarshal([]byte(fk.data), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.AccessToken != "new_a" || persisted.RefreshToken != "new_r" {
		t.Errorf("persisted not updated: %+v", persisted)
	}
}

// TestSessionSource_ConcurrentRefresh is the load-bearing test for
// the mutex: many goroutines hitting an expired token must produce
// exactly one refresh call. Without serialization the server would
// trip refresh-token reuse detection (RFC 9700 §4.14) and revoke the
// grant, locking the user out.
func TestSessionSource_ConcurrentRefresh(t *testing.T) {
	swapKeyring(t)
	var hits int32
	srv := fakeTokenServer(t, 200, `{"access_token":"new_a","refresh_token":"new_r","expires_in":3600}`, &hits)
	defer srv.Close()

	seed := &Session{
		Version:         sessionSchemaVersion,
		Endpoint:        srv.URL,
		AccessToken:     "old",
		RefreshToken:    "ref",
		AccessExpiresAt: time.Now().Add(-time.Minute), // already expired
	}
	_ = writeSession(seed)
	s := &sessionSource{endpoint: srv.URL, sess: *seed}

	const N = 16
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _ = s.Token(context.Background())
		}()
	}
	wg.Wait()
	if hits != 1 {
		t.Errorf("refresh fired %d times across %d goroutines; want 1", hits, N)
	}
}

// TestSessionSource_InvalidGrantWipes: a refresh that comes back as
// invalid_grant means the server permanently rejected the credential.
// Keep the session and the user is in a stuck state forever — wipe
// it so the next `s2 login` starts clean.
func TestSessionSource_InvalidGrantWipes(t *testing.T) {
	fk := swapKeyring(t)
	var hits int32
	srv := fakeTokenServer(t, 400, `{"error":"invalid_grant","error_description":"refresh token expired"}`, &hits)
	defer srv.Close()

	seed := &Session{
		Version:         sessionSchemaVersion,
		Endpoint:        srv.URL,
		AccessToken:     "old",
		RefreshToken:    "ref",
		AccessExpiresAt: time.Now().Add(-time.Minute),
	}
	_ = writeSession(seed)
	s := &sessionSource{endpoint: srv.URL, sess: *seed}

	_, err := s.Token(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if fk.data != "" {
		t.Errorf("session not wiped on invalid_grant: %q", fk.data)
	}
}

// TestSessionSource_NetworkErrorPreservesSession: a 5xx / network
// blip is transient. Wiping the session would force a re-login on
// every server hiccup, which is hostile and unnecessary.
func TestSessionSource_NetworkErrorPreservesSession(t *testing.T) {
	fk := swapKeyring(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":"server_error"}`))
	}))
	defer srv.Close()

	seed := &Session{
		Version:         sessionSchemaVersion,
		Endpoint:        srv.URL,
		AccessToken:     "old",
		RefreshToken:    "ref",
		AccessExpiresAt: time.Now().Add(-time.Minute),
	}
	_ = writeSession(seed)
	s := &sessionSource{endpoint: srv.URL, sess: *seed}

	_, err := s.Token(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if fk.data == "" {
		t.Error("session wiped on transient error; should be preserved")
	}
}

func TestStaticSource(t *testing.T) {
	s := NewStaticSource("s2_static")
	tok, err := s.Token(context.Background())
	if err != nil || tok != "s2_static" {
		t.Errorf("Token: %q %v", tok, err)
	}
	r, err := s.Refresh(context.Background())
	if err != nil || r != "s2_static" {
		t.Errorf("Refresh on static should return same token: %q %v", r, err)
	}
}

// TestRefresh_TimeoutPropagates verifies cancelling context cancels
// the in-flight refresh; otherwise Stop() on a syncing service would
// hang waiting for /oauth/token.
func TestRefresh_TimeoutPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := oauth.Refresh(ctx, srv.URL, "client-id", "ref")
	if err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("want context error, got %v", err)
	}
}
