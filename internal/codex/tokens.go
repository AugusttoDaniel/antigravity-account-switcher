// Package codex manages OpenAI Codex (ChatGPT) accounts: the PKCE login against auth.openai.com,
// token refresh, and the ~/.codex/auth.json file the Codex CLI reads.
//
// The protocol constants (issuer, public client id, scopes, file layout) come from the
// Apache-2.0 openai/codex repository; nothing here is derived from third-party switchers.
package codex

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// Tokens is the "tokens" object of auth.json. IDToken is the raw JWT string.
type Tokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id,omitempty"`
}

// Claims is what the switcher needs from the id_token.
type Claims struct {
	Email     string
	AccountID string // chatgpt_account_id: identifies the ChatGPT account/workspace
	PlanType  string // e.g. "plus", "pro", "team"
}

type idTokenPayload struct {
	Email   string `json:"email"`
	Profile struct {
		Email string `json:"email"`
	} `json:"https://api.openai.com/profile"`
	Auth struct {
		AccountID string `json:"chatgpt_account_id"`
		PlanType  string `json:"chatgpt_plan_type"`
	} `json:"https://api.openai.com/auth"`
}

// ParseIDToken reads the claims of an id_token JWT. The signature is not verified: the token came
// straight from the issuer over TLS, and the claims only label the account locally.
func ParseIDToken(jwt string) (Claims, error) {
	parts := strings.Split(strings.TrimSpace(jwt), ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("id_token is not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return Claims{}, errors.New("id_token payload is not valid base64")
	}
	var p idTokenPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return Claims{}, errors.New("id_token payload is not valid JSON")
	}
	email := p.Email
	if email == "" {
		email = p.Profile.Email
	}
	if email == "" {
		return Claims{}, errors.New("id_token has no email claim")
	}
	return Claims{Email: email, AccountID: p.Auth.AccountID, PlanType: p.Auth.PlanType}, nil
}
