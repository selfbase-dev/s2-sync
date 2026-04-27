package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	stdsync "sync"
	"time"

	"github.com/selfbase-dev/s2-sync/internal/oauth"
	"github.com/spf13/viper"
)

// Source produces a current bearer token for outgoing API calls. The
// HTTP client consults Token() before each request and Refresh() once
// on a 401 reply. Two implementations exist: a static (env / --token)
// source for CI and scripts, and a session-backed source that knows
// how to rotate its credentials at /oauth/token.
type Source interface {
	Token(ctx context.Context) (string, error)
	Refresh(ctx context.Context) (string, error)
}

// refreshSkew is how far ahead of expiry we proactively refresh, so a
// request issued shortly after Token() doesn't race the server clock.
const refreshSkew = 60 * time.Second

// NewSource picks the right Source for the current process:
//
//  1. --token flag / S2_TOKEN env  → staticSource
//  2. structured session in keyring → sessionSource
//  3. otherwise                     → ErrNoSession
//
// Endpoint mismatch (user changed --endpoint after a previous login)
// invalidates the session so we don't send tokens to the wrong server.
func NewSource(endpoint string) (Source, error) {
	if t := strings.TrimSpace(viper.GetString("token")); t != "" {
		if !strings.HasPrefix(t, "s2_") {
			return nil, fmt.Errorf("invalid token format: must start with s2_")
		}
		return &staticSource{token: t}, nil
	}
	sess, err := LoadSession()
	if err != nil {
		return nil, err
	}
	endpoint = strings.TrimRight(endpoint, "/")
	if sess.Endpoint != endpoint {
		_ = DeleteKeyring()
		return nil, ErrNoSession
	}
	return &sessionSource{sess: *sess, endpoint: endpoint}, nil
}

// NewStaticSource wraps a fixed token in a Source. Useful for tests
// and for scripted callers that pass a token directly without going
// through the env / flag plumbing.
func NewStaticSource(token string) Source { return &staticSource{token: token} }

// staticSource wraps a fixed token (env / flag). Refresh() returns the
// same token rather than erroring, so the client's 401-retry logic
// degrades gracefully — the second attempt fails the same way and the
// caller sees ErrUnauthorized.
type staticSource struct{ token string }

func (s *staticSource) Token(_ context.Context) (string, error)   { return s.token, nil }
func (s *staticSource) Refresh(_ context.Context) (string, error) { return s.token, nil }

// sessionSource serves an access_token from the keyring-backed session,
// transparently calling /oauth/token (grant_type=refresh_token) when
// the cached token is near expiry. The mutex serializes refreshes so
// a 401 storm from many goroutines cannot trigger reuse-detection on
// the server (multiple concurrent rotations of the same refresh token
// trip RFC 9700 §4.14 — the server cascades the grant).
type sessionSource struct {
	mu       stdsync.Mutex
	sess     Session
	endpoint string
}

func (s *sessionSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Until(s.sess.AccessExpiresAt) > refreshSkew {
		return s.sess.AccessToken, nil
	}
	return s.refreshLocked(ctx)
}

func (s *sessionSource) Refresh(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshLocked(ctx)
}

func (s *sessionSource) refreshLocked(ctx context.Context) (string, error) {
	tr, err := oauth.Refresh(ctx, s.endpoint, s.sess.ClientID, s.sess.RefreshToken)
	if err != nil {
		// invalid_grant means the refresh token is permanently dead
		// (revoked, expired, or reuse-detected). Wipe so the next
		// `s2 login` starts clean. Network / 5xx errors keep the
		// session — they're transient.
		if strings.Contains(err.Error(), "invalid_grant") {
			_ = DeleteKeyring()
			return "", errors.Join(ErrNoSession, err)
		}
		return "", err
	}
	s.sess.AccessToken = tr.AccessToken
	s.sess.RefreshToken = tr.RefreshToken
	s.sess.AccessExpiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	if err := writeSession(&s.sess); err != nil {
		return "", err
	}
	return s.sess.AccessToken, nil
}
