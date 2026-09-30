package domain

import (
	"context"
	"errors"
	"time"
)

// ErrCodexAccountNotFound indicates no Codex account matches the given criteria.
var ErrCodexAccountNotFound = errors.New("codex account not found")

// CodexAccount is an OpenAI Codex (ChatGPT) login managed by the switcher. It is kept apart from
// Account on purpose: Codex accounts never enter the Antigravity routing pool, the quota poller or
// the OmniRoute agy export.
type CodexAccount struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	// ChatGPTAccountID is the chatgpt_account_id claim: it tells apart personal and workspace
	// logins that share an email.
	ChatGPTAccountID string `json:"chatgpt_account_id,omitempty"`
	PlanType         string `json:"plan_type,omitempty"`

	// Credentials are omitted from JSON serialization.
	IDToken      string `json:"-"`
	AccessToken  string `json:"-"`
	RefreshToken string `json:"-"`

	LastRefresh time.Time `json:"last_refresh"`

	// ProxyURL is the egress the login and every refresh for this account must use.
	ProxyURL          string        `json:"proxy_url,omitempty"`
	AdsPowerProfileID string        `json:"adspower_profile_id,omitempty"`
	IsActive          bool          `json:"is_active"`
	Status            AccountStatus `json:"status"`
	CreatedAt         time.Time     `json:"created_at"`
	UpdatedAt         time.Time     `json:"updated_at"`
}

// CodexAccountRepository persists Codex accounts.
type CodexAccountRepository interface {
	// Upsert stores the account, matching an existing one by (email, ChatGPT account id), the email
	// compared case-insensitively. On a match
	// it refreshes the credentials and plan, reactivates an errored account, and keeps the stored
	// proxy and profile unless the new value is non-empty. It returns the stored account.
	Upsert(ctx context.Context, acc *CodexAccount) (*CodexAccount, error)
	GetByID(ctx context.Context, id string) (*CodexAccount, error)
	GetActive(ctx context.Context) (*CodexAccount, error)
	List(ctx context.Context) ([]*CodexAccount, error)
	// SetActive marks one account active and clears the others, atomically.
	SetActive(ctx context.Context, id string) error
	// UpdateTokens stores rotated credentials.
	UpdateTokens(ctx context.Context, id, idToken, accessToken, refreshToken string, lastRefresh time.Time) error
	UpdateStatus(ctx context.Context, id string, status AccountStatus) error
	UpdateProxyURL(ctx context.Context, id, proxyURL string) error
	UpdateAdsPowerProfileID(ctx context.Context, id, profileID string) error
	Delete(ctx context.Context, id string) error
}
