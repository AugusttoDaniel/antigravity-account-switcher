// Package proxypool allocates outbound proxies for new accounts from a static candidate list,
// guaranteeing each new account receives a proxy not already bound to any existing account.
//
// It deliberately holds no provider integration: the candidate list is supplied by the caller
// (from config), and "used" is defined solely by the account store. A provider-backed source can
// later satisfy the same Pool interface without changing callers.
package proxypool

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
)

// ErrExhausted is returned when every candidate proxy is already bound to an account.
var ErrExhausted = errors.New("proxy pool exhausted: every configured proxy is already assigned to an account")

// ErrEmpty is returned when no candidate proxies are configured at all.
var ErrEmpty = errors.New("no proxies configured: add proxies to the config pool before onboarding a new account")

// Pool allocates a proxy for a new account.
type Pool interface {
	// Allocate returns a proxy URL not currently assigned to any account. It does not persist the
	// assignment; the caller binds the returned proxy to the account it is onboarding.
	Allocate(ctx context.Context) (string, error)
}

// accountLister is the subset of the account repository the pool needs.
type accountLister interface {
	List(ctx context.Context) ([]*domain.Account, error)
}

// StaticPool draws from a fixed candidate list, excluding proxies already in use.
type StaticPool struct {
	candidates []string
	repo       accountLister
}

// NewStaticPool builds a StaticPool from the configured candidate list and the account store.
// Blank and duplicate candidates are dropped while preserving order.
func NewStaticPool(candidates []string, repo accountLister) *StaticPool {
	seen := make(map[string]bool, len(candidates))
	cleaned := make([]string, 0, len(candidates))
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		key := normalize(c)
		if seen[key] {
			continue
		}
		seen[key] = true
		cleaned = append(cleaned, c)
	}
	return &StaticPool{candidates: cleaned, repo: repo}
}

// Allocate returns the first candidate not currently bound to any account.
func (p *StaticPool) Allocate(ctx context.Context) (string, error) {
	if len(p.candidates) == 0 {
		return "", ErrEmpty
	}
	if p.repo == nil {
		return "", fmt.Errorf("proxypool: nil account repository")
	}

	accounts, err := p.repo.List(ctx)
	if err != nil {
		return "", fmt.Errorf("proxypool: list accounts: %w", err)
	}

	used := make(map[string]bool, len(accounts))
	for _, a := range accounts {
		if a == nil {
			continue
		}
		if proxy := strings.TrimSpace(a.ProxyURL); proxy != "" {
			used[normalize(proxy)] = true
		}
	}

	for _, c := range p.candidates {
		if !used[normalize(c)] {
			return c, nil
		}
	}
	return "", ErrExhausted
}

// normalize produces a comparison key so trivially different spellings of the same proxy
// (surrounding whitespace, scheme/host case) are treated as one.
func normalize(proxy string) string {
	return strings.ToLower(strings.TrimSpace(proxy))
}
