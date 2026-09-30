package codex

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Protocol constants, taken from github.com/openai/codex (Apache-2.0), codex-rs/login.
const (
	DefaultIssuer   = "https://auth.openai.com"
	DefaultClientID = "app_EMoamEEZ73f0CkXaXp7hrann" // the Codex CLI's public OAuth client
	// CallbackPort is fixed: the public client only has this loopback redirect registered.
	CallbackPort = 1455
	Scope        = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	Originator   = "codex_cli_rs"

	httpTimeout   = 30 * time.Second
	bodyLimit     = 1 << 20
	refreshGrant  = "refresh_token"
	exchangeGrant = "authorization_code"
)

// ErrInvalidGrant marks a refresh token or code the issuer rejected as expired, used or revoked.
var ErrInvalidGrant = errors.New("invalid_grant")

// PKCE holds a code verifier and its S256 challenge.
type PKCE struct {
	Verifier  string
	Challenge string
}

// NewPKCE generates a fresh verifier/challenge pair.
func NewPKCE() (PKCE, error) {
	b := make([]byte, 64)
	if _, err := rand.Read(b); err != nil {
		return PKCE{}, fmt.Errorf("generate the PKCE verifier: %w", err)
	}
	v := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(v))
	return PKCE{Verifier: v, Challenge: base64.RawURLEncoding.EncodeToString(sum[:])}, nil
}

// NewState generates the anti-CSRF state parameter.
func NewState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate the OAuth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Client talks to the OpenAI auth server. HTTP decides the egress: pass a proxy-bound client so a
// token exchange or refresh never leaves from the operator's real IP.
type Client struct {
	Issuer   string
	ClientID string
	HTTP     *http.Client
}

// NewClient returns a Client for the production issuer using httpClient.
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: httpTimeout}
	}
	return &Client{Issuer: DefaultIssuer, ClientID: DefaultClientID, HTTP: httpClient}
}

// RedirectURI is the loopback callback registered for the public client.
func RedirectURI(port int) string { return fmt.Sprintf("http://127.0.0.1:%d/auth/callback", port) }

// AuthorizeURL builds the consent URL.
func (c *Client) AuthorizeURL(redirectURI, state string, p PKCE) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", c.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", Scope)
	q.Set("code_challenge", p.Challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("originator", Originator)
	q.Set("state", state)
	return strings.TrimRight(c.Issuer, "/") + "/oauth/authorize?" + q.Encode()
}

// TokenResponse is the issuer's reply to an exchange or a refresh. A refresh may omit fields that
// did not change.
type TokenResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// Exchange trades an authorization code for tokens.
func (c *Client) Exchange(ctx context.Context, code, verifier, redirectURI string) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", exchangeGrant)
	form.Set("client_id", c.ClientID)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", verifier)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build the token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.do(req)
}

// Refresh trades a refresh token for new tokens. The issuer may rotate the refresh token: the
// caller must persist the one in the response.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*TokenResponse, error) {
	body, err := json.Marshal(map[string]string{
		"client_id":     c.ClientID,
		"grant_type":    refreshGrant,
		"refresh_token": refreshToken,
	})
	if err != nil {
		return nil, fmt.Errorf("encode the refresh request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build the refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req)
}

func (c *Client) tokenURL() string { return strings.TrimRight(c.Issuer, "/") + "/oauth/token" }

// do sends the request. Failures never include the response body or URL: they can carry codes,
// verifiers or tokens.
func (c *Client) do(req *http.Request) (*TokenResponse, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("token endpoint unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Error != "" {
			code := sanitizeCode(e.Error)
			if code == "invalid_grant" {
				return nil, fmt.Errorf("token endpoint rejected the request: HTTP %d (%s): %w", resp.StatusCode, code, ErrInvalidGrant)
			}
			return nil, fmt.Errorf("token endpoint rejected the request: HTTP %d (%s)", resp.StatusCode, code)
		}
		return nil, fmt.Errorf("token endpoint rejected the request: HTTP %d", resp.StatusCode)
	}
	var tr TokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("token endpoint returned an unreadable response")
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("token endpoint returned no access token")
	}
	return &tr, nil
}

// sanitizeCode keeps an OAuth error code (e.g. invalid_grant) and drops anything else.
func sanitizeCode(s string) string {
	if len(s) > 64 {
		return "error"
	}
	for _, r := range s {
		if !(r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return "error"
		}
	}
	return s
}
