package omniroute

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// OAuthStart is what OmniRoute's own OAuth flow hands out for a provider: the Google consent URL
// (built with the client OmniRoute itself renews tokens with) and the values the exchange needs.
type OAuthStart struct {
	AuthURL      string `json:"authUrl"`
	State        string `json:"state"`
	CodeVerifier string `json:"codeVerifier"`
	RedirectURI  string `json:"redirectUri"`
}

// OAuthExchange is the body of the code exchange.
type OAuthExchange struct {
	Code         string `json:"code"`
	RedirectURI  string `json:"redirectUri"`
	CodeVerifier string `json:"codeVerifier,omitempty"`
	State        string `json:"state,omitempty"`
}

// OAuthConnection is what OmniRoute reports after it stored the signed-in account. Tokens are
// never decoded.
type OAuthConnection struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

// StartOAuth begins OmniRoute's own sign-in for a provider (GET /api/oauth/{provider}/authorize).
// A connection created through it belongs to that provider and is renewed by OmniRoute's own
// OAuth client, which is what a token imported from elsewhere may not be.
func (c *Client) StartOAuth(ctx context.Context, provider string) (*OAuthStart, error) {
	var out OAuthStart
	if err := c.do(ctx, "GET", "/api/oauth/"+url.PathEscape(provider)+"/authorize", nil, &out); err != nil {
		return nil, err
	}
	if out.AuthURL == "" || out.State == "" {
		return nil, errors.New("OmniRoute did not return a sign-in URL")
	}
	return &out, nil
}

// ExchangeOAuth finishes the sign-in (POST /api/oauth/{provider}/exchange): OmniRoute trades the
// code for tokens and stores the connection.
func (c *Client) ExchangeOAuth(ctx context.Context, provider string, req OAuthExchange) (*OAuthConnection, error) {
	var out struct {
		Success    bool             `json:"success"`
		Connection *OAuthConnection `json:"connection"`
	}
	if err := c.do(ctx, "POST", "/api/oauth/"+url.PathEscape(provider)+"/exchange", req, &out); err != nil {
		return nil, err
	}
	if out.Connection == nil {
		return &OAuthConnection{}, nil
	}
	return out.Connection, nil
}

// CallbackCode extracts the authorization code from the address the browser landed on after the
// consent (the redirect page itself fails to load, so users copy its address), checking state.
func CallbackCode(raw, wantState string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.RawQuery == "" {
		return "", errors.New("that is not the address the browser landed on after sign-in (it should contain ?code=...)")
	}
	q := u.Query()
	if e := q.Get("error"); e != "" {
		return "", fmt.Errorf("Google refused the sign-in: %s", e)
	}
	if wantState != "" && q.Get("state") != wantState {
		return "", errors.New("that address belongs to a different sign-in (state mismatch); use the one from this run")
	}
	code := q.Get("code")
	if code == "" {
		return "", errors.New("the address has no code parameter")
	}
	return code, nil
}
