// Package auth holds the s2-sync side of the OAuth 2.1 + PKCE flow.
// Three layers:
//
//   - keyring.go — low-level secure-storage primitive (single string).
//   - session.go — the structured payload we keep there (this file):
//     client_id, access_token, refresh_token, expiry, endpoint.
//   - source.go  — the live token source the client consults at request
//     time, with refresh-on-expiry serialized through a mutex.
//
// Legacy entries (pre-OAuth plain `s2_…` tokens or older v1 sessions
// without a client_id) are wiped on load and surface as ErrNoSession so
// the user is prompted to re-login cleanly.
package auth

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/selfbase-dev/s2-sync/internal/oauth"
)

// sessionSchemaVersion bumps when the JSON shape changes incompatibly.
// On mismatch the stored session is wiped and the user re-logs in —
// the keyring is a cache of credentials, not a source of truth, so
// throwing it away is always safe.
//
// v2: added ClientID for RFC 7591 Dynamic Client Registration. v1
// sessions had no per-install client_id and are not migratable.
const sessionSchemaVersion = 2

// ErrNoSession is returned by LoadSession / NewSource when there is no
// usable session in the keyring (missing, legacy, schema-bumped, or
// endpoint-mismatched). Callers prompt the user to run `s2 login`.
var ErrNoSession = errors.New("no session: please run `s2 login` to authenticate")

// Session is the JSON shape stored in the OS secure storage.
type Session struct {
	Version         int       `json:"version"`
	Kind            string    `json:"kind"` // currently always "oauth"
	Endpoint        string    `json:"endpoint"`
	ClientID        string    `json:"client_id"`
	AccessToken     string    `json:"access_token"`
	RefreshToken    string    `json:"refresh_token"`
	AccessExpiresAt time.Time `json:"access_expires_at"`
}

// LoadSession reads the keyring and returns the structured session.
// Legacy plain-string entries and out-of-date schema versions are
// deleted to force a clean re-login.
func LoadSession() (*Session, error) {
	raw, err := GetKeyring()
	if err != nil || raw == "" {
		return nil, ErrNoSession
	}
	var s Session
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		// Legacy plain `s2_xxx` token (or otherwise corrupt). Wipe so
		// the user re-logs in via OAuth.
		_ = DeleteKeyring()
		return nil, ErrNoSession
	}
	if s.Version != sessionSchemaVersion {
		// Older schema (e.g. v1 without client_id). Not migratable —
		// the user needs a fresh registration + login.
		_ = DeleteKeyring()
		return nil, ErrNoSession
	}
	if s.ClientID == "" {
		// Defensive: a v2 session must carry a client_id. Treat a
		// missing one as corrupt.
		_ = DeleteKeyring()
		return nil, ErrNoSession
	}
	return &s, nil
}

// SaveSession persists the structured session, stamping the current
// schema version + kind. Caller supplies endpoint, the per-install
// client_id, and the OAuth token response from oauth.Login or
// oauth.Refresh.
func SaveSession(endpoint, clientID string, tr *oauth.TokenResponse) error {
	s := &Session{
		Version:         sessionSchemaVersion,
		Kind:            "oauth",
		Endpoint:        strings.TrimRight(endpoint, "/"),
		ClientID:        clientID,
		AccessToken:     tr.AccessToken,
		RefreshToken:    tr.RefreshToken,
		AccessExpiresAt: time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}
	return writeSession(s)
}

func writeSession(s *Session) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return SetKeyring(string(b))
}

// HasValidSession is a cheap check used by the GUI to decide whether
// to show the Welcome screen vs. the dashboard. It does not validate
// against the server, only that a structured session is present.
func HasValidSession() bool {
	_, err := LoadSession()
	return err == nil
}
