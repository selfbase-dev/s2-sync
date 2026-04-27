package oauth

import (
	"bytes"
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
	// Scope is the only OAuth scope s2-sync requests. Verbs (read/write)
	// live on the grant, not the scope.
	Scope = "files"

	// ClientName is the human-friendly name s2-sync presents during DCR
	// (RFC 7591). The server shows this on the consent page.
	ClientName = "s2-sync"

	// loopbackTemplate is the placeholder redirect URI registered at DCR
	// time. Per RFC 8252 §7.3, native apps register the loopback IP and
	// the authorization server must accept any port at runtime.
	loopbackTemplate = "http://127.0.0.1/callback"

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

// registerRequest is the RFC 7591 §3.1 registration request body.
type registerRequest struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

// registerResponse is the subset of the RFC 7591 §3.2.1 response we use.
type registerResponse struct {
	ClientID string `json:"client_id"`
}

// Register performs RFC 7591 Dynamic Client Registration against the
// given endpoint and returns the issued client_id. The caller persists
// the result alongside the OAuth session so subsequent /oauth/authorize
// and /oauth/token calls can reuse it.
//
// Note on backups: if a keyring backup duplicates a client_id across
// machines, the server's standard refresh-token rotation handles the
// conflict — no client-side mitigation needed.
func Register(ctx context.Context, endpoint string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint = strings.TrimRight(endpoint, "/")

	body, err := json.Marshal(registerRequest{
		ClientName:              ClientName,
		RedirectURIs:            []string{loopbackTemplate},
		TokenEndpointAuthMethod: "none",
	})
	if err != nil {
		return "", fmt.Errorf("marshal register request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint+"/oauth/register", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("register endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		var oerr struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&oerr)
		return "", fmt.Errorf("register endpoint %d: %s: %s", resp.StatusCode, oerr.Error, oerr.ErrorDescription)
	}

	var rr registerResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return "", fmt.Errorf("decode register response: %w", err)
	}
	if rr.ClientID == "" {
		return "", fmt.Errorf("register response missing client_id")
	}
	return rr.ClientID, nil
}

// LoginResult is what Login returns: the issued tokens plus the client_id
// that produced them. The caller persists both into the session.
type LoginResult struct {
	ClientID string
	Tokens   *TokenResponse
}

// Login runs the full Authorization Code + PKCE flow against `endpoint`
// (e.g. "https://scopeds.dev").
//
// If clientID is empty, Login first performs RFC 7591 Dynamic Client
// Registration to obtain one, then proceeds. The returned LoginResult
// always carries the client_id actually used so the caller can persist
// it for future /oauth/token (refresh) calls.
//
// Caller responsibilities: persist the result (auth.SaveSession) and
// inform the user. This function blocks until the user completes consent
// in the browser, cancels, or the context expires.
func Login(ctx context.Context, endpoint string, clientID string) (*LoginResult, error) {
	endpoint = strings.TrimRight(endpoint, "/")
	if ctx == nil {
		ctx = context.Background()
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultLoginTimeout)
		defer cancel()
	}

	if clientID == "" {
		id, err := Register(ctx, endpoint)
		if err != nil {
			return nil, fmt.Errorf("register client: %w", err)
		}
		clientID = id
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
	authURL := buildAuthorizeURL(endpoint, clientID, redirectURI, state, pkce.challenge)

	if err := openBrowser(authURL); err != nil {
		return nil, fmt.Errorf("open browser: %w (open this URL manually: %s)", err, authURL)
	}

	code, err := lb.wait(ctx)
	if err != nil {
		return nil, err
	}

	tr, err := exchangeCode(ctx, endpoint, clientID, code, redirectURI, pkce.verifier)
	if err != nil {
		return nil, err
	}
	return &LoginResult{ClientID: clientID, Tokens: tr}, nil
}

func buildAuthorizeURL(endpoint, clientID, redirectURI, state, codeChallenge string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", Scope)
	q.Set("state", state)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	return endpoint + "/oauth/authorize?" + q.Encode()
}

// exchangeCode hits POST /oauth/token (grant_type=authorization_code).
// redirectURI must match what the loopback server returned, byte-for-byte
// (the server compares strings, not parsed URLs — RFC 6749 §4.1.3).
func exchangeCode(ctx context.Context, endpoint, clientID, code, redirectURI, codeVerifier string) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", codeVerifier)
	form.Set("client_id", clientID)
	return postToken(ctx, endpoint, form)
}

// Refresh exchanges a refresh_token for a new access/refresh pair.
// Used by auth.Session when the access token has expired.
func Refresh(ctx context.Context, endpoint, clientID, refreshToken string) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", clientID)
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
