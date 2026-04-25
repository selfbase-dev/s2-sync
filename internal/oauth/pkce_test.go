package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

// TestNewPKCE_S256 confirms the verifier shape and the S256 transform
// match RFC 7636 §4.2: challenge = base64url(SHA-256(verifier)), no
// padding. The s2 server rejects any other code_challenge_method, so
// the constants must stay locked in.
func TestNewPKCE_S256(t *testing.T) {
	p, err := newPKCE()
	if err != nil {
		t.Fatalf("newPKCE: %v", err)
	}
	if len(p.verifier) < 43 || len(p.verifier) > 128 {
		t.Fatalf("verifier length %d out of [43,128]", len(p.verifier))
	}
	if strings.ContainsAny(p.verifier, "+/=") {
		t.Errorf("verifier must be url-safe (no +, /, =): %q", p.verifier)
	}
	sum := sha256.Sum256([]byte(p.verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if p.challenge != want {
		t.Errorf("challenge mismatch:\n got  %s\n want %s", p.challenge, want)
	}
	if strings.ContainsAny(p.challenge, "+/=") {
		t.Errorf("challenge must be url-safe (no +, /, =): %q", p.challenge)
	}
}

// TestNewPKCE_Unique guards against accidental reuse: every login must
// generate a fresh verifier, otherwise an attacker who once saw the
// challenge could replay later code exchanges.
func TestNewPKCE_Unique(t *testing.T) {
	a, _ := newPKCE()
	b, _ := newPKCE()
	if a.verifier == b.verifier || a.challenge == b.challenge {
		t.Fatalf("PKCE pairs collide: a=%v b=%v", a, b)
	}
}

func TestRandState_DistinctAndUrlSafe(t *testing.T) {
	a, err := randState()
	if err != nil {
		t.Fatalf("randState: %v", err)
	}
	b, _ := randState()
	if a == b {
		t.Fatalf("state values collide")
	}
	if strings.ContainsAny(a, "+/=") {
		t.Errorf("state must be url-safe: %q", a)
	}
}
