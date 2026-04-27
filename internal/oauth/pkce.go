// Package oauth implements the OAuth 2.1 + PKCE + loopback redirect
// client flow that s2-sync uses to authenticate against the S2 server,
// plus RFC 7591 Dynamic Client Registration. The package is deliberately
// small: it knows how to register a client (flow.go: Register), start
// a login (flow.go: Login), receive the redirect (loopback.go), and
// produce PKCE pairs (this file). Token storage and refresh live in
// auth/.
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// pkcePair is the (verifier, challenge) tuple sent to /oauth/authorize
// and exchanged at /oauth/token. challenge = base64url(SHA-256(verifier)).
type pkcePair struct {
	verifier  string
	challenge string
}

// newPKCE returns a fresh S256 PKCE pair. verifier is 43 chars of
// URL-safe base64 (RFC 7636 §4.1: 43-128 chars).
func newPKCE() (pkcePair, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return pkcePair{}, fmt.Errorf("rand: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return pkcePair{
		verifier:  verifier,
		challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

// randState returns 32 bytes of URL-safe base64 for the OAuth `state`
// parameter (CSRF binding between authorize redirect and callback).
func randState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
