package proxypool

import (
	"context"
	"errors"
	"testing"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
)

type fakeLister struct {
	accounts []*domain.Account
	err      error
}

func (f *fakeLister) List(context.Context) ([]*domain.Account, error) {
	return f.accounts, f.err
}

func TestAllocateReturnsFirstUnused(t *testing.T) {
	lister := &fakeLister{accounts: []*domain.Account{
		{ProxyURL: "http://a.example:8080"},
	}}
	pool := NewStaticPool([]string{"http://a.example:8080", "http://b.example:8080"}, lister)

	got, err := pool.Allocate(context.Background())
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got != "http://b.example:8080" {
		t.Errorf("expected first unused proxy, got %q", got)
	}
}

func TestAllocateExhausted(t *testing.T) {
	lister := &fakeLister{accounts: []*domain.Account{
		{ProxyURL: "http://a.example:8080"},
		{ProxyURL: "HTTP://B.EXAMPLE:8080"}, // case-different spelling still counts as used
	}}
	pool := NewStaticPool([]string{"http://a.example:8080", "http://b.example:8080"}, lister)

	_, err := pool.Allocate(context.Background())
	if !errors.Is(err, ErrExhausted) {
		t.Errorf("expected ErrExhausted, got %v", err)
	}
}

func TestAllocateEmptyPool(t *testing.T) {
	pool := NewStaticPool(nil, &fakeLister{})
	_, err := pool.Allocate(context.Background())
	if !errors.Is(err, ErrEmpty) {
		t.Errorf("expected ErrEmpty, got %v", err)
	}
}

func TestNewStaticPoolDedupesAndTrims(t *testing.T) {
	pool := NewStaticPool([]string{" http://a.example:8080 ", "http://a.example:8080", "", "http://b.example:8080"}, &fakeLister{})
	if len(pool.candidates) != 2 {
		t.Errorf("expected 2 unique candidates, got %d: %v", len(pool.candidates), pool.candidates)
	}
}

func TestAllocateSkipsAccountsWithoutProxy(t *testing.T) {
	lister := &fakeLister{accounts: []*domain.Account{
		{ProxyURL: ""},
		{ProxyURL: "http://a.example:8080"},
	}}
	pool := NewStaticPool([]string{"http://a.example:8080", "http://b.example:8080"}, lister)
	got, err := pool.Allocate(context.Background())
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got != "http://b.example:8080" {
		t.Errorf("got %q", got)
	}
}
