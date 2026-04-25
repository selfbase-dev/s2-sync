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
// fresh OAuth response, read it back. Bumps schema version and kind
// implicitly so they're tested too.
func TestSaveAndLoadSession_RoundTrip(t *testing.T) {
	fk := swapKeyring(t)
	tr := &oauth.TokenResponse{
		AccessToken:  "s2_a",
		RefreshToken: "s2_r",
		ExpiresIn:    3600,
	}
	if err := SaveSession("https://scopeds.dev", tr); err != nil {
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

func TestLoadSession_Empty(t *testing.T) {
	swapKeyring(t)
	if _, err := LoadSession(); err != ErrNoSession {
		t.Fatalf("want ErrNoSession, got %v", err)
	}
}
