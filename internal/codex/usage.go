package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
)

// ErrUnauthorized marks an access token the backend rejected (expired or revoked).
var ErrUnauthorized = errors.New("the access token was rejected")

const usageUserAgent = "antigravity-account-switcher"

type usageWindowPayload struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds float64 `json:"limit_window_seconds"`
	ResetAfterSeconds  float64 `json:"reset_after_seconds"`
	ResetAt            float64 `json:"reset_at"`
}

type usagePayload struct {
	PlanType  string `json:"plan_type"`
	RateLimit *struct {
		Allowed         bool                `json:"allowed"`
		LimitReached    bool                `json:"limit_reached"`
		PrimaryWindow   *usageWindowPayload `json:"primary_window"`
		SecondaryWindow *usageWindowPayload `json:"secondary_window"`
	} `json:"rate_limit"`
	Credits *struct {
		HasCredits bool `json:"has_credits"`
		Unlimited  bool `json:"unlimited"`
		Balance    any  `json:"balance"`
	} `json:"credits"`
}

// FetchUsage reads the account's rate-limit windows from the ChatGPT backend (the endpoint the
// Codex CLI itself uses). Failures never include the response body or the token.
func (c *Client) FetchUsage(ctx context.Context, accessToken, chatgptAccountID string) (*domain.CodexUsage, error) {
	base := strings.TrimRight(c.BackendURL, "/")
	if base == "" {
		base = DefaultBackendURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/wham/usage", nil)
	if err != nil {
		return nil, fmt.Errorf("build the usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", usageUserAgent)
	req.Header.Set("Accept", "application/json")
	if chatgptAccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", chatgptAccountID)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("usage endpoint unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, ErrUnauthorized
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("usage endpoint answered HTTP %d", resp.StatusCode)
	}
	var p usagePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, errors.New("usage endpoint returned an unreadable response")
	}
	return usageFromPayload(&p, time.Now().UTC()), nil
}

func usageFromPayload(p *usagePayload, now time.Time) *domain.CodexUsage {
	u := &domain.CodexUsage{PlanType: p.PlanType, Allowed: true, FetchedAt: now}
	if rl := p.RateLimit; rl != nil {
		u.Allowed, u.LimitReached = rl.Allowed, rl.LimitReached
		u.Primary = windowFromPayload(rl.PrimaryWindow, now)
		u.Secondary = windowFromPayload(rl.SecondaryWindow, now)
	}
	if cr := p.Credits; cr != nil {
		u.HasCredits, u.UnlimitedCreds = cr.HasCredits, cr.Unlimited
		if cr.Balance != nil {
			u.CreditBalance = fmt.Sprint(cr.Balance)
		}
	}
	return u
}

func windowFromPayload(w *usageWindowPayload, now time.Time) *domain.CodexUsageWindow {
	if w == nil {
		return nil
	}
	out := &domain.CodexUsageWindow{UsedPercent: int(w.UsedPercent + 0.5), WindowSeconds: int(w.LimitWindowSeconds)}
	switch {
	case w.ResetAt > 0:
		out.ResetAt = time.Unix(int64(w.ResetAt), 0).UTC()
	case w.ResetAfterSeconds > 0:
		out.ResetAt = now.Add(time.Duration(w.ResetAfterSeconds) * time.Second)
	}
	return out
}

// Usage reads an account's current limits through its own proxy and stores the snapshot. A
// rejected access token is renewed once (which may rotate the refresh token) and the read retried.
// Like every other call that reaches OpenAI, it fails closed for an account with no proxy.
func (s *Service) Usage(ctx context.Context, id string, opts RefreshOptions) (*domain.CodexUsage, error) {
	acc, err := s.Repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := checkHandedOff(acc, opts.Force); err != nil {
		return nil, err
	}
	if acc.ProxyURL == "" && !opts.AllowDirect {
		return nil, ErrProxyRequired
	}
	if acc.IsActive {
		// The CLI may have rotated the tokens since we stored them: start from its newest.
		if fresh, err := s.CaptureRotation(ctx); err == nil && fresh != nil && fresh.ID == acc.ID {
			acc = fresh
		}
	}
	client, err := s.NewClient(acc.ProxyURL)
	if err != nil {
		return nil, err
	}

	u, err := client.FetchUsage(ctx, acc.AccessToken, acc.ChatGPTAccountID)
	if errors.Is(err, ErrUnauthorized) {
		renewed, rerr := s.Refresh(ctx, acc.ID, opts)
		if rerr != nil {
			return nil, fmt.Errorf("the access token expired and could not be renewed: %w", rerr)
		}
		u, err = client.FetchUsage(ctx, renewed.AccessToken, renewed.ChatGPTAccountID)
	}
	if err != nil {
		return nil, err
	}
	u.AccountID = acc.ID
	if u.PlanType == "" {
		u.PlanType = acc.PlanType
	}
	if s.Usages != nil {
		if err := s.Usages.SaveUsage(ctx, u); err != nil {
			return u, fmt.Errorf("the usage was read but could not be saved: %w", err)
		}
	}
	return u, nil
}
