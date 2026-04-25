package oauth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
)

// callbackResult is what the loopback server delivers to the caller.
// On user-cancel or upstream error, err is non-nil and code is empty.
type callbackResult struct {
	code string
	err  error
}

// loopbackServer listens on 127.0.0.1:0 and serves a single /callback.
// It binds to a random free port and exposes the absolute redirect URI
// the caller must register with /oauth/authorize.
//
// The server only completes (signaling result) when it receives a
// callback whose `state` matches the expected value. Stray requests
// (favicon probes, port scans, accidental refreshes) get a 404 and do
// not terminate the wait — otherwise a misbehaving page that hits the
// loopback before the real redirect would deadlock the login.
type loopbackServer struct {
	listener      net.Listener
	server        *http.Server
	expectedState string
	resultCh      chan callbackResult
	once          sync.Once
}

func startLoopback(expectedState string) (*loopbackServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("loopback listen: %w", err)
	}
	lb := &loopbackServer{
		listener:      ln,
		expectedState: expectedState,
		resultCh:      make(chan callbackResult, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", lb.handleCallback)
	lb.server = &http.Server{Handler: mux}
	go func() { _ = lb.server.Serve(ln) }()
	return lb, nil
}

// redirectURI is the absolute URL to register with /oauth/authorize.
// Must be reused verbatim at the /oauth/token exchange (RFC 6749 §4.1.3).
func (l *loopbackServer) redirectURI() string {
	addr := l.listener.Addr().(*net.TCPAddr)
	return fmt.Sprintf("http://127.0.0.1:%d/callback", addr.Port)
}

// wait blocks until the callback arrives (or ctx cancels) and returns
// the authorization code. It does not stop the server; call shutdown.
func (l *loopbackServer) wait(ctx context.Context) (string, error) {
	select {
	case r := <-l.resultCh:
		return r.code, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (l *loopbackServer) shutdown() {
	_ = l.server.Shutdown(context.Background())
}

func (l *loopbackServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	if state != l.expectedState {
		// Don't terminate on state mismatch: it could be a stray
		// request and we don't want to abort the legitimate one. RFC
		// 6749 §10.12 calls this out — the binding goes the other
		// way; the real callback will arrive with the right state.
		http.NotFound(w, r)
		return
	}

	if errCode := q.Get("error"); errCode != "" {
		desc := q.Get("error_description")
		writeBrowserPage(w, false, fmt.Sprintf("%s: %s", errCode, desc))
		l.deliver(callbackResult{err: fmt.Errorf("authorization failed: %s: %s", errCode, desc)})
		return
	}

	code := q.Get("code")
	if code == "" {
		writeBrowserPage(w, false, "missing authorization code")
		l.deliver(callbackResult{err: fmt.Errorf("missing authorization code")})
		return
	}

	writeBrowserPage(w, true, "")
	l.deliver(callbackResult{code: code})
}

func (l *loopbackServer) deliver(r callbackResult) {
	l.once.Do(func() { l.resultCh <- r })
}

func writeBrowserPage(w http.ResponseWriter, ok bool, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if ok {
		_, _ = w.Write([]byte(`<!doctype html><meta charset=utf-8><title>Signed in</title><body style="font-family:system-ui;text-align:center;padding:4rem"><h1>You're signed in.</h1><p>You can close this window and return to S2 Sync.</p>`))
		return
	}
	_, _ = w.Write([]byte(`<!doctype html><meta charset=utf-8><title>Sign-in failed</title><body style="font-family:system-ui;text-align:center;padding:4rem"><h1>Sign-in failed.</h1><p>` + htmlEscape(msg) + `</p>`))
}

// htmlEscape is the minimum needed to avoid XSS in the failure page;
// only error_description from the server flows through here.
func htmlEscape(s string) string {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch r {
		case '<':
			out = append(out, '&', 'l', 't', ';')
		case '>':
			out = append(out, '&', 'g', 't', ';')
		case '&':
			out = append(out, '&', 'a', 'm', 'p', ';')
		case '"':
			out = append(out, '&', 'q', 'u', 'o', 't', ';')
		default:
			out = append(out, string(r)...)
		}
	}
	return string(out)
}
