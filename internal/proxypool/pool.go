// Package proxypool allocates outbound proxies for new accounts from a static candidate list,
// guaranteeing each new account receives a proxy not already bound to any existing account.
//
// It deliberately holds no provider integration: the candidate list is supplied by the caller
// (from config), and "used" is defined solely by the account store. A provider-backed source can
// later satisfy the same Pool interface without changing callers.
package proxypool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// ID is a short, stable identifier for a pool entry, derived from its normalized URL. It lets the
// dashboard refer to an entry without ever holding the credentials inside it.
func ID(proxy string) string {
	sum := sha256.Sum256([]byte(normalize(proxy)))
	return hex.EncodeToString(sum[:])[:12]
}

// Entry is one pool proxy and the account currently bound to it, if any.
type Entry struct {
	ID     string
	URL    string // raw proxy URL, credentials included; never send it to a client as-is
	UsedBy string // email of the account bound to this proxy, "" when free
}

// Entries lists the pool in order with each proxy's current binding. Blank and duplicate
// candidates are dropped, as in NewStaticPool.
func Entries(candidates []string, accounts []*domain.Account) []Entry {
	usedBy := make(map[string]string, len(accounts))
	for _, a := range accounts {
		if a == nil {
			continue
		}
		if proxy := strings.TrimSpace(a.ProxyURL); proxy != "" {
			usedBy[normalize(proxy)] = a.Email
		}
	}
	pool := NewStaticPool(candidates, nil).candidates
	out := make([]Entry, 0, len(pool))
	for _, c := range pool {
		out = append(out, Entry{ID: ID(c), URL: c, UsedBy: usedBy[normalize(c)]})
	}
	return out
}

// Merge appends the proxies in add that the pool does not already hold, preserving order, and
// reports how many were added.
func Merge(existing, add []string) (merged []string, added int) {
	merged = NewStaticPool(existing, nil).candidates
	seen := make(map[string]bool, len(merged)+len(add))
	for _, c := range merged {
		seen[normalize(c)] = true
	}
	for _, c := range add {
		c = strings.TrimSpace(c)
		if c == "" || seen[normalize(c)] {
			continue
		}
		seen[normalize(c)] = true
		merged = append(merged, c)
		added++
	}
	return merged, added
}

// Find returns the pool proxy with the given ID.
func Find(existing []string, id string) (string, bool) {
	for _, c := range NewStaticPool(existing, nil).candidates {
		if ID(c) == id {
			return c, true
		}
	}
	return "", false
}

// Remove drops the pool proxy with the given ID, reporting whether it was present.
func Remove(existing []string, id string) ([]string, bool) {
	pool := NewStaticPool(existing, nil).candidates
	out := make([]string, 0, len(pool))
	removed := false
	for _, c := range pool {
		if ID(c) == id {
			removed = true
			continue
		}
		out = append(out, c)
	}
	return out, removed
}

// normalize produces a comparison key so trivially different spellings of the same proxy
// (surrounding whitespace, scheme/host case) are treated as one.
func normalize(proxy string) string {
	return strings.ToLower(strings.TrimSpace(proxy))
}
