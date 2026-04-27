package auth

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/selfbase-dev/s2-sync/internal/oauth"
)

// fakeKeyring lets us run session/source tests without poking the OS
// keychain. Tests swap in a closure-backed implementation via the
// keyringHooks override.
type fakeKeyring struct{ data string }

func (f *fakeKeyring) get() (string, error) { return f.data, nil }
func (f *fakeKeyring) set(v string) error   { f.data = v; return nil }
func (f *fakeKeyring) delete() error        { f.data = ""; return nil }

func swapKeyring(t *testing.T) *fakeKeyring {
	t.Helper()
	prev := keyringHooks
	fk := &fakeKeyring{}
	keyringHooks = fk
	t.Cleanup(func() { keyringHooks = prev })
	return fk
}

// TestSaveAndLoadSession_RoundTrip is the basic lifecycle: persist a
// fresh OAuth response, read it back. Bumps schema version, kind, and
// client_id implicitly so they're tested too.
func TestSaveAndLoadSession_RoundTrip(t *testing.T) {
	fk := swapKeyring(t)
	tr := &oauth.TokenResponse{
		AccessToken:  "s2_a",
		RefreshToken: "s2_r",
		ExpiresIn:    3600,
	}
	if err := SaveSession("https://scopeds.dev", "client-abc", tr); err != nil {
		t.Fatalf("save: %v", err)
	}
	if fk.data == "" {
		t.Fatal("keyring not written")
	}

	got, err := LoadSession()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.AccessToken != "s2_a" || got.RefreshToken != "s2_r" {
		t.Errorf("tokens: %+v", got)
	}
	if got.ClientID != "client-abc" {
		t.Errorf("client_id: %q", got.ClientID)
	}
	if got.Endpoint != "https://scopeds.dev" {
		t.Errorf("endpoint: %s", got.Endpoint)
	}
	if got.Version != sessionSchemaVersion || got.Kind != "oauth" {
		t.Errorf("metadata: %+v", got)
	}
	if time.Until(got.AccessExpiresAt) < 50*time.Minute || time.Until(got.AccessExpiresAt) > 70*time.Minute {
		t.Errorf("expires_at not ~3600s ahead: %v", got.AccessExpiresAt)
	}
}

// TestLoadSession_LegacyPlainStringIsWiped covers the migration from
// pre-OAuth builds: a raw `s2_xxx` is meaningless to refresh logic,
// so we delete it and tell the caller no session exists.
func TestLoadSession_LegacyPlainStringIsWiped(t *testing.T) {
	fk := swapKeyring(t)
	fk.data = "s2_legacyplaintext"

	_, err := LoadSession()
	if err != ErrNoSession {
		t.Fatalf("want ErrNoSession, got %v", err)
	}
	if fk.data != "" {
		t.Fatalf("legacy entry not wiped: %q", fk.data)
	}
}

func TestLoadSession_VersionMismatchIsWiped(t *testing.T) {
	fk := swapKeyring(t)
	bad, _ := json.Marshal(&Session{
		Version:      999,
		Kind:         "oauth",
		Endpoint:     "https://scopeds.dev",
		ClientID:     "client-abc",
		AccessToken:  "s2_a",
		RefreshToken: "s2_r",
	})
	fk.data = string(bad)

	if _, err := LoadSession(); err != ErrNoSession {
		t.Fatalf("want ErrNoSession, got %v", err)
	}
	if fk.data != "" {
		t.Fatalf("stale-version entry not wiped: %q", fk.data)
	}
}

// TestLoadSession_LegacyV1IsWiped is the upgrade path from the
// pre-DCR (hardcoded client_id, no per-install registration) format.
// A v1 payload has no client_id field; LoadSession must wipe it so
// the user is prompted to re-login cleanly with a freshly registered
// client.
func TestLoadSession_LegacyV1IsWiped(t *testing.T) {
	fk := swapKeyring(t)
	// Hand-crafted v1 shape — no client_id field. We don't reuse
	// Session{} so we don't accidentally encode the new field as ""
	// and slip past the version-mismatch wipe.
	v1 := map[string]any{
		"version":           1,
		"kind":              "oauth",
		"endpoint":          "https://scopeds.dev",
		"access_token":      "s2_a",
		"refresh_token":     "s2_r",
		"access_expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
	}
	b, _ := json.Marshal(v1)
	fk.data = string(b)

	if _, err := LoadSession(); err != ErrNoSession {
		t.Fatalf("want ErrNoSession, got %v", err)
	}
	if fk.data != "" {
		t.Fatalf("v1 entry not wiped: %q", fk.data)
	}
}

// TestLoadSession_MissingClientIDIsWiped guards against a corrupted
// v2 payload that for any reason carries no client_id. Refresh would
// fail server-side, so it's safer to force a clean re-login.
func TestLoadSession_MissingClientIDIsWiped(t *testing.T) {
	fk := swapKeyring(t)
	bad, _ := json.Marshal(&Session{
		Version:      sessionSchemaVersion,
		Kind:         "oauth",
		Endpoint:     "https://scopeds.dev",
		AccessToken:  "s2_a",
		RefreshToken: "s2_r",
	})
	fk.data = string(bad)

	if _, err := LoadSession(); err != ErrNoSession {
		t.Fatalf("want ErrNoSession, got %v", err)
	}
	if fk.data != "" {
		t.Fatalf("client_id-less entry not wiped: %q", fk.data)
	}
}

func TestLoadSession_Empty(t *testing.T) {
	swapKeyring(t)
	if _, err := LoadSession(); err != ErrNoSession {
		t.Fatalf("want ErrNoSession, got %v", err)
	}
}
