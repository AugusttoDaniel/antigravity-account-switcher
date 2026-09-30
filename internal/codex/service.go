package codex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/egress"
)

// ErrProxyRequired is returned when a network call would leave from the operator's real IP
// because the account has no proxy.
var ErrProxyRequired = errors.New("this account has no proxy: refusing to contact OpenAI from the real IP (set one with codex-set-proxy, or pass --allow-direct)")

// Service is the Codex account workflow: add, import, switch and refresh.
type Service struct {
	Repo domain.CodexAccountRepository
	// Home is the Codex config directory holding auth.json (see Home()).
	Home string
	// NewClient builds the OAuth client for an egress proxy. Tests replace it.
	NewClient func(proxyURL string) (*Client, error)
	Now       func() time.Time
}

// NewService wires a Service with proxy-aware production clients.
func NewService(repo domain.CodexAccountRepository, home string) *Service {
	return &Service{Repo: repo, Home: home, NewClient: ProxiedClient, Now: time.Now}
}

// ProxiedClient returns an OAuth client whose traffic goes through proxyURL. An empty URL is a
// direct client; an invalid one is an error, never a silent fallback to direct.
func ProxiedClient(proxyURL string) (*Client, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return NewClient(nil), nil
	}
	u, err := egress.ParseProxyURL(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("the account's proxy is invalid: %w", err)
	}
	return NewClient(&http.Client{
		Timeout:   httpTimeout,
		Transport: &http.Transport{Proxy: http.ProxyURL(u), IdleConnTimeout: 30 * time.Second},
	}), nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) authPath() string { return AuthPath(s.Home) }

// AddFromTokens stores a fresh login (from Login) as an account.
func (s *Service) AddFromTokens(ctx context.Context, tr *TokenResponse, proxyURL, profileID string) (*domain.CodexAccount, error) {
	if tr == nil || tr.IDToken == "" || tr.RefreshToken == "" {
		return nil, errors.New("the login returned no id_token or refresh token")
	}
	return s.store(ctx, Tokens{IDToken: tr.IDToken, AccessToken: tr.AccessToken, RefreshToken: tr.RefreshToken}, proxyURL, profileID)
}

func (s *Service) store(ctx context.Context, t Tokens, proxyURL, profileID string) (*domain.CodexAccount, error) {
	claims, err := ParseIDToken(t.IDToken)
	if err != nil {
		return nil, err
	}
	if claims.AccountID == "" {
		claims.AccountID = t.AccountID
	}
	return s.Repo.Upsert(ctx, &domain.CodexAccount{
		Email: claims.Email, ChatGPTAccountID: claims.AccountID, PlanType: claims.PlanType,
		IDToken: t.IDToken, AccessToken: t.AccessToken, RefreshToken: t.RefreshToken,
		LastRefresh: s.now().UTC(), ProxyURL: proxyURL, AdsPowerProfileID: profileID,
	})
}

// ImportAuthFile adds the ChatGPT login stored in an existing auth.json.
func (s *Service) ImportAuthFile(ctx context.Context, path, proxyURL string) (*domain.CodexAccount, error) {
	f, err := ReadAuthFile(path)
	if err != nil {
		return nil, err
	}
	if f == nil || f.Tokens == nil || f.Tokens.RefreshToken == "" {
		return nil, errors.New("that auth.json has no ChatGPT login (API-key logins cannot be switched)")
	}
	return s.store(ctx, *f.Tokens, proxyURL, "")
}

// findByClaims returns the stored account a set of claims belongs to, or nil.
func (s *Service) findByClaims(ctx context.Context, c Claims) (*domain.CodexAccount, error) {
	all, err := s.Repo.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range all {
		if strings.EqualFold(a.Email, c.Email) && a.ChatGPTAccountID == c.AccountID {
			return a, nil
		}
	}
	return nil, nil
}

// CaptureRotation copies the tokens the Codex CLI has in auth.json back onto the matching stored
// account. The CLI rotates its refresh token in place; without this, switching away would leave
// the switcher holding a token the issuer already retired. It returns the account it updated, or
// nil when auth.json is empty or belongs to no stored account.
func (s *Service) CaptureRotation(ctx context.Context) (*domain.CodexAccount, error) {
	f, err := ReadAuthFile(s.authPath())
	if err != nil || f == nil || f.Tokens == nil || f.Tokens.IDToken == "" {
		return nil, err
	}
	claims, err := ParseIDToken(f.Tokens.IDToken)
	if err != nil {
		return nil, nil // not a login we can attribute
	}
	if claims.AccountID == "" {
		claims.AccountID = f.Tokens.AccountID
	}
	acc, err := s.findByClaims(ctx, claims)
	if err != nil || acc == nil {
		return nil, err
	}
	t := f.Tokens
	if t.RefreshToken == "" || (t.RefreshToken == acc.RefreshToken && t.AccessToken == acc.AccessToken) {
		return acc, nil
	}
	last := s.now().UTC()
	if f.LastRefresh != nil {
		last = *f.LastRefresh
	}
	if err := s.Repo.UpdateTokens(ctx, acc.ID, t.IDToken, t.AccessToken, t.RefreshToken, last); err != nil {
		return nil, err
	}
	return s.Repo.GetByID(ctx, acc.ID)
}

// Switch makes id the account the Codex CLI uses: it first saves any rotation the CLI made to the
// outgoing account, then writes the target's login to auth.json and marks it active. It makes no
// network call, so switching never exposes an IP. Close running Codex sessions first: a live CLI
// keeps the old login in memory and may write it back.
func (s *Service) Switch(ctx context.Context, id string) (*domain.CodexAccount, error) {
	if _, err := s.CaptureRotation(ctx); err != nil {
		return nil, fmt.Errorf("save the current login before switching: %w", err)
	}
	acc, err := s.Repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if acc.Status == domain.AccountStatusDisabled {
		return nil, errors.New("that account is disabled")
	}
	last := acc.LastRefresh
	if last.IsZero() {
		last = s.now()
	}
	f := NewChatGPTAuthFile(Tokens{
		IDToken: acc.IDToken, AccessToken: acc.AccessToken, RefreshToken: acc.RefreshToken, AccountID: acc.ChatGPTAccountID,
	}, last)
	if err := WriteAuthFile(s.authPath(), f); err != nil {
		return nil, err
	}
	if err := s.Repo.SetActive(ctx, acc.ID); err != nil {
		return nil, err
	}
	return s.Repo.GetByID(ctx, acc.ID)
}

// RefreshOptions tunes Refresh.
type RefreshOptions struct {
	// AllowDirect permits a refresh from the real IP for an account with no proxy.
	AllowDirect bool
}

// Refresh renews an account's tokens through its own proxy and stores the rotated credentials.
// For the active account auth.json is rewritten too, so the CLI does not keep a retired token.
// An invalid_grant marks the account errored: it needs a new login.
func (s *Service) Refresh(ctx context.Context, id string, opts RefreshOptions) (*domain.CodexAccount, error) {
	acc, err := s.Repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if acc.ProxyURL == "" && !opts.AllowDirect {
		return nil, ErrProxyRequired
	}
	if acc.IsActive {
		// The CLI may have rotated the token since we stored it: refresh the newest one.
		if fresh, err := s.CaptureRotation(ctx); err == nil && fresh != nil && fresh.ID == acc.ID {
			acc = fresh
		}
	}
	client, err := s.NewClient(acc.ProxyURL)
	if err != nil {
		return nil, err
	}
	tr, err := client.Refresh(ctx, acc.RefreshToken)
	if err != nil {
		if errors.Is(err, ErrInvalidGrant) {
			_ = s.Repo.UpdateStatus(ctx, acc.ID, domain.AccountStatusError)
		}
		return nil, err
	}
	idToken, refresh := tr.IDToken, tr.RefreshToken
	if idToken == "" {
		idToken = acc.IDToken
	}
	if refresh == "" {
		refresh = acc.RefreshToken
	}
	now := s.now().UTC()
	if err := s.Repo.UpdateTokens(ctx, acc.ID, idToken, tr.AccessToken, refresh, now); err != nil {
		return nil, fmt.Errorf("the tokens were renewed but could not be saved: %w", err)
	}
	if acc.Status == domain.AccountStatusError {
		_ = s.Repo.UpdateStatus(ctx, acc.ID, domain.AccountStatusActive)
	}
	if acc.IsActive {
		f := NewChatGPTAuthFile(Tokens{IDToken: idToken, AccessToken: tr.AccessToken, RefreshToken: refresh, AccountID: acc.ChatGPTAccountID}, now)
		if err := WriteAuthFile(s.authPath(), f); err != nil {
			return nil, fmt.Errorf("the tokens were renewed but auth.json could not be updated: %w", err)
		}
	}
	return s.Repo.GetByID(ctx, acc.ID)
}

// Resolve finds an account by id or by email. An email shared by several logins (personal and
// workspace) is ambiguous and must be given by id.
func (s *Service) Resolve(ctx context.Context, ref string) (*domain.CodexAccount, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("no account given")
	}
	all, err := s.Repo.List(ctx)
	if err != nil {
		return nil, err
	}
	var matches []*domain.CodexAccount
	for _, a := range all {
		if a.ID == ref {
			return a, nil
		}
		if strings.EqualFold(a.Email, ref) {
			matches = append(matches, a)
		}
	}
	switch len(matches) {
	case 0:
		return nil, domain.ErrCodexAccountNotFound
	case 1:
		return matches[0], nil
	}
	return nil, fmt.Errorf("%d logins share the email %s (personal and workspace): pass the account id instead", len(matches), ref)
}
