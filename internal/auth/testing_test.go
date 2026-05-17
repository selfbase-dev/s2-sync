package auth

import (
	"strings"
	"testing"
	"time"
)

// TestSwapKeyringForTesting_RoundTrip locks in the behavior of the
// exported test seam: install fake → reads/writes route through it
// → restore replaces the production hook. Without this regression
// guard, a future refactor could quietly break every external smoke
// (sync_test/oauth_e2e_smoke_test.go) by changing what restore returns.
func TestSwapKeyringForTesting_RoundTrip(t *testing.T) {
	data, restore := SwapKeyringForTesting()
	t.Cleanup(restore)

	if err := SetKeyring("hello"); err != nil {
		t.Fatalf("SetKeyring: %v", err)
	}
	if *data != "hello" {
		t.Errorf("data = %q, want hello", *data)
	}
	got, err := GetKeyring()
	if err != nil || got != "hello" {
		t.Errorf("GetKeyring = %q %v", got, err)
	}
	if err := DeleteKeyring(); err != nil {
		t.Fatalf("DeleteKeyring: %v", err)
	}
	if *data != "" {
		t.Errorf("after delete, data = %q, want empty", *data)
	}
}

// TestWriteSessionForTesting_PersistsThroughHelper proves the helper
// stores a payload that LoadSession can read back round-trip.
func TestWriteSessionForTesting_PersistsThroughHelper(t *testing.T) {
	_, restore := SwapKeyringForTesting()
	t.Cleanup(restore)

	seed := &Session{
		Version:         SessionSchemaVersion,
		Kind:            "oauth",
		Endpoint:        "https://example.test",
		ClientID:        "client-zzz",
		AccessToken:     "s2_a",
		RefreshToken:    "s2_r",
		AccessExpiresAt: time.Now().Add(time.Hour),
	}
	if err := WriteSessionForTesting(seed); err != nil {
		t.Fatalf("WriteSessionForTesting: %v", err)
	}
	got, err := LoadSession()
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if got.ClientID != "client-zzz" || got.AccessToken != "s2_a" {
		t.Errorf("round-trip lost data: %+v", got)
	}

	// Sanity check: SessionSchemaVersion is the same constant
	// LoadSession compares against — otherwise the helper would
	// silently mint payloads the loader wipes as legacy.
	if SessionSchemaVersion != sessionSchemaVersion {
		t.Errorf("exported SessionSchemaVersion = %d, internal = %d",
			SessionSchemaVersion, sessionSchemaVersion)
	}

	// inMemoryKeyring's underlying store accepts/returns strings,
	// not bytes — guard against silent encoding regressions.
	raw, _ := GetKeyring()
	if !strings.Contains(raw, `"client_id":"client-zzz"`) {
		t.Errorf("persisted blob shape unexpected: %s", raw)
	}
}
