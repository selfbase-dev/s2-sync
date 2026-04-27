package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestBuildAuthorizeURL pins the parameter set the server expects.
// Drift here (e.g. dropping `state`, switching code_challenge_method)
// is the kind of silent break that only shows up server-side — keep
// this strict.
func TestBuildAuthorizeURL(t *testing.T) {
	got := buildAuthorizeURL(
		"https://scopeds.dev",
		"client-abc",
		"http://127.0.0.1:54321/callback",
		"STATE",
		"CC",
	)
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Scheme != "https" || u.Host != "scopeds.dev" || u.Path != "/oauth/authorize" {
		t.Fatalf("unexpected base: %s", got)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"response_type":         "code",
		"client_id":             "client-abc",
		"redirect_uri":          "http://127.0.0.1:54321/callback",
		"scope":                 Scope,
		"state":                 "STATE",
		"code_challenge":        "CC",
		"code_challenge_method": "S256",
	} {
		if got := q.Get(k); got != want {
			t.Errorf("%s: got %q want %q", k, got, want)
		}
	}
	// Per-install distinguisher fields no longer ride on the authorize
	// URL — the per-install client_id covers that.
	for _, k := range []string{"installation_id", "device_label"} {
		if q.Has(k) {
			t.Errorf("unexpected legacy param %q present", k)
		}
	}
}

// TestRegister_OK exercises the RFC 7591 Dynamic Client Registration
// happy path against a stubbed /oauth/register endpoint.
func TestRegister_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/register" || r.Method != "POST" {
			t.Errorf("unexpected req: %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("content-type: %s", ct)
		}
		var body registerRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if body.ClientName != ClientName {
			t.Errorf("client_name: %q", body.ClientName)
		}
		if body.TokenEndpointAuthMethod != "none" {
			t.Errorf("token_endpoint_auth_method: %q", body.TokenEndpointAuthMethod)
		}
		if len(body.RedirectURIs) != 1 || body.RedirectURIs[0] != loopbackTemplate {
			t.Errorf("redirect_uris: %v", body.RedirectURIs)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"client_id":"dcr-issued-id","client_id_issued_at":1700000000}`))
	}))
	defer srv.Close()

	id, err := Register(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if id != "dcr-issued-id" {
		t.Errorf("client_id: %q", id)
	}
}

func TestRegister_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_redirect_uri","error_description":"redirect_uri not allowed"}`))
	}))
	defer srv.Close()

	_, err := Register(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "invalid_redirect_uri") {
		t.Fatalf("expected invalid_redirect_uri error, got %v", err)
	}
}

func TestRegister_MissingClientID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	_, err := Register(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for missing client_id")
	}
}

// TestExchangeCode_OK runs the happy path against an httptest server
// that mimics /oauth/token. The point is to verify the request shape
// (form encoding, exact param names) — the server requires these.
func TestExchangeCode_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" || r.Method != "POST" {
			t.Errorf("unexpected req: %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			t.Errorf("content-type: %s", ct)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		for k, want := range map[string]string{
			"grant_type":    "authorization_code",
			"code":          "abc",
			"redirect_uri":  "http://127.0.0.1:1/callback",
			"code_verifier": "ver",
			"client_id":     "client-abc",
		} {
			if r.PostForm.Get(k) != want {
				t.Errorf("%s: got %q want %q", k, r.PostForm.Get(k), want)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"s2_a","refresh_token":"s2_r","token_type":"Bearer","expires_in":3600,"scope":"files"}`))
	}))
	defer srv.Close()

	tr, err := exchangeCode(context.Background(), srv.URL, "client-abc", "abc", "http://127.0.0.1:1/callback", "ver")
	if err != nil {
		t.Fatalf("exchangeCode: %v", err)
	}
	if tr.AccessToken != "s2_a" || tr.RefreshToken != "s2_r" || tr.ExpiresIn != 3600 {
		t.Errorf("unexpected response: %+v", tr)
	}
}

// TestExchangeCode_ServerError surfaces the OAuth error code/desc so
// the user sees what the server actually said, not just "400".
func TestExchangeCode_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"PKCE verification failed"}`))
	}))
	defer srv.Close()

	_, err := exchangeCode(context.Background(), srv.URL, "cid", "x", "y", "z")
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") || !strings.Contains(err.Error(), "PKCE") {
		t.Fatalf("expected invalid_grant + description, got %v", err)
	}
}

func TestExchangeCode_MissingTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"a"}`)) // no refresh_token
	}))
	defer srv.Close()

	_, err := exchangeCode(context.Background(), srv.URL, "cid", "x", "y", "z")
	if err == nil {
		t.Fatal("expected error for missing refresh_token")
	}
}

func TestRefresh_FormShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.PostForm.Get("grant_type") != "refresh_token" {
			t.Errorf("grant_type: %q", r.PostForm.Get("grant_type"))
		}
		if r.PostForm.Get("refresh_token") != "old" {
			t.Errorf("refresh_token: %q", r.PostForm.Get("refresh_token"))
		}
		if r.PostForm.Get("client_id") != "client-xyz" {
			t.Errorf("client_id: %q", r.PostForm.Get("client_id"))
		}
		_, _ = w.Write([]byte(`{"access_token":"s2_new_a","refresh_token":"s2_new_r","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tr, err := Refresh(ctx, srv.URL, "client-xyz", "old")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tr.AccessToken != "s2_new_a" || tr.RefreshToken != "s2_new_r" {
		t.Errorf("unexpected: %+v", tr)
	}
}
