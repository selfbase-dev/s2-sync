package oauth

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestLoopback_Success runs the loopback server end-to-end: bind to a
// random port, hit /callback with the expected state, and confirm the
// code is returned and the success page is rendered.
func TestLoopback_Success(t *testing.T) {
	lb, err := startLoopback("STATE")
	if err != nil {
		t.Fatal(err)
	}
	defer lb.shutdown()

	uri := lb.redirectURI()
	if !strings.HasPrefix(uri, "http://127.0.0.1:") || !strings.HasSuffix(uri, "/callback") {
		t.Fatalf("redirect URI shape: %s", uri)
	}

	resp, err := http.Get(uri + "?code=ABC&state=STATE")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status %d", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	code, err := lb.wait(ctx)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if code != "ABC" {
		t.Errorf("code: %q", code)
	}
}

// TestLoopback_StateMismatch_DoesNotTerminate models a stray request
// (favicon probe, port scan). The wait() must keep going so the real
// callback can still resolve.
func TestLoopback_StateMismatch_DoesNotTerminate(t *testing.T) {
	lb, err := startLoopback("STATE")
	if err != nil {
		t.Fatal(err)
	}
	defer lb.shutdown()
	uri := lb.redirectURI()

	stray, err := http.Get(uri + "?code=X&state=WRONG")
	if err != nil {
		t.Fatal(err)
	}
	stray.Body.Close()
	if stray.StatusCode != 404 {
		t.Errorf("stray req: status %d, want 404", stray.StatusCode)
	}

	// Now the real callback. It should resolve.
	go func() {
		time.Sleep(50 * time.Millisecond)
		resp, _ := http.Get(uri + "?code=REAL&state=STATE")
		if resp != nil {
			resp.Body.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	code, err := lb.wait(ctx)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if code != "REAL" {
		t.Errorf("code: %q", code)
	}
}

// TestLoopback_AccessDenied is the user-clicked-Cancel path. The s2
// server redirects to redirect_uri with `error=access_denied`. We must
// surface that as an error rather than wait indefinitely.
func TestLoopback_AccessDenied(t *testing.T) {
	lb, err := startLoopback("STATE")
	if err != nil {
		t.Fatal(err)
	}
	defer lb.shutdown()

	resp, err := http.Get(lb.redirectURI() + "?error=access_denied&error_description=user+denied&state=STATE")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = lb.wait(ctx)
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("expected access_denied error, got %v", err)
	}
}

// TestLoopback_CtxCancel covers the user-closes-browser-without-acting
// path: ctx fires before any callback arrives.
func TestLoopback_CtxCancel(t *testing.T) {
	lb, err := startLoopback("STATE")
	if err != nil {
		t.Fatal(err)
	}
	defer lb.shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = lb.wait(ctx)
	if err == nil {
		t.Fatal("expected ctx error")
	}
}
