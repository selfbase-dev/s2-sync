package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const (
	// ClientID is hardcoded by ADR 0056: s2-sync is registered as a public
	// loopback client at migration time. Changing this requires a coordinated
	// update on the server side.
	ClientID = "s2-sync-desktop"
	// Scope is the only OAuth scope s2-sync requests. ADR 0056 §"OAuth scope"
	// keeps verbs out of scopes — read/write is on the grant.
	Scope = "files"

	defaultLoginTimeout = 5 * time.Minute
)

// TokenResponse is the parsed body of a successful /oauth/token call.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope,omitempty"`
}

// Login runs the full Authorization Code + PKCE flow against `endpoint`
// (e.g. "https://scopeds.dev"). Returns the issued tokens.
//
// Caller responsibilities: persist the result (auth.SaveSession) and
// inform the user. This function blocks until the user completes consent
// in the browser, cancels, or the context expires.
func Login(ctx context.Context, endpoint string) (*TokenResponse, error) {
	endpoint = strings.TrimRight(endpoint, "/")
	if ctx == nil {
		ctx = context.Background()
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultLoginTimeout)
		defer cancel()
	}

	pkce, err := newPKCE()
	if err != nil {
		return nil, err
	}
	state, err := randState()
	if err != nil {
		return nil, err
	}

	lb, err := startLoopback(state)
	if err != nil {
		return nil, err
	}
	defer lb.shutdown()

	redirectURI := lb.redirectURI()
	authURL := buildAuthorizeURL(endpoint, redirectURI, state, pkce.challenge)

	if err := openBrowser(authURL); err != nil {
		return nil, fmt.Errorf("open browser: %w (open this URL manually: %s)", err, authURL)
	}

	code, err := lb.wait(ctx)
	if err != nil {
		return nil, err
	}

	return exchangeCode(ctx, endpoint, code, redirectURI, pkce.verifier)
}

func buildAuthorizeURL(endpoint, redirectURI, state, codeChallenge string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", Scope)
	q.Set("state", state)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	return endpoint + "/oauth/authorize?" + q.Encode()
}

// exchangeCode hits POST /oauth/token (grant_type=authorization_code).
// redirectURI must match what the loopback server returned, byte-for-byte
// (s2 server compares strings, not parsed URLs — RFC 6749 §4.1.3).
func exchangeCode(ctx context.Context, endpoint, code, redirectURI, codeVerifier string) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", codeVerifier)
	form.Set("client_id", ClientID)
	return postToken(ctx, endpoint, form)
}

// Refresh exchanges a refresh_token for a new access/refresh pair.
// Used by auth.Session when the access token has expired.
func Refresh(ctx context.Context, endpoint, refreshToken string) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", ClientID)
	return postToken(ctx, strings.TrimRight(endpoint, "/"), form)
}

func postToken(ctx context.Context, endpoint string, form url.Values) (*TokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var oerr struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&oerr)
		return nil, fmt.Errorf("token endpoint %d: %s: %s", resp.StatusCode, oerr.Error, oerr.ErrorDescription)
	}

	var tr TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if tr.AccessToken == "" || tr.RefreshToken == "" {
		return nil, fmt.Errorf("token response missing access_token or refresh_token")
	}
	return &tr, nil
}

// openBrowser launches the user's default browser at url. Each platform's
// command takes the URL as a single argv entry — never a shell string —
// so a malicious URL cannot inject shell metacharacters.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
