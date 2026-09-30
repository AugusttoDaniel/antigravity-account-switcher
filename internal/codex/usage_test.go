package codex

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
)

const usageJSON = `{
  "plan_type": "plus",
  "rate_limit": {
    "allowed": true, "limit_reached": false,
    "primary_window":   {"used_percent": 42, "limit_window_seconds": 18000,  "reset_after_seconds": 3600,  "reset_at": 1790000000},
    "secondary_window": {"used_percent": 7.6, "limit_window_seconds": 604800, "reset_after_seconds": 90000, "reset_at": 1790500000}
  },
  "credits": {"has_credits": true, "unlimited": false, "balance": "12.50"}
}`

func TestUsageReadsStoresAndSendsTheRightHeaders(t *testing.T) {
	e := newSvcEnv(t)
	a := e.add(t, "a@example.com", "acct-a", "http://h:1")
	var gotAuth, gotAcct, gotUA, gotPath string
	e.issuer(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotAcct, gotUA, gotPath = r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-Id"), r.Header.Get("User-Agent"), r.URL.Path
		_, _ = w.Write([]byte(usageJSON))
	})

	u, err := e.svc.Usage(context.Background(), a.ID, RefreshOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/wham/usage" || gotAuth != "Bearer at-a@example.com" || gotAcct != "acct-a" || gotUA == "" || strings.Contains(strings.ToLower(gotUA), "codex-cli") {
		t.Fatalf("request = %s auth=%q acct=%q ua=%q", gotPath, gotAuth, gotAcct, gotUA)
	}
	if u.AccountID != a.ID || u.PlanType != "plus" || !u.Allowed || u.LimitReached {
		t.Fatalf("usage = %+v", u)
	}
	if u.Primary.UsedPercent != 42 || u.Primary.WindowSeconds != 18000 || u.Primary.ResetAt.Unix() != 1790000000 {
		t.Fatalf("primary = %+v", u.Primary)
	}
	if u.Secondary.UsedPercent != 8 { // 7.6 rounds to 8
		t.Fatalf("secondary = %+v", u.Secondary)
	}
	if !u.HasCredits || u.CreditBalance != "12.50" {
		t.Fatalf("credits = %+v", u)
	}

	stored, err := e.repo.GetUsage(context.Background(), a.ID)
	if err != nil || stored.Primary.UsedPercent != 42 {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
}

func TestUsageRequiresAProxy(t *testing.T) {
	e := newSvcEnv(t)
	var calls int32
	e.issuer(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(usageJSON))
	})
	a := e.add(t, "a@example.com", "acct-a", "")
	if _, err := e.svc.Usage(context.Background(), a.ID, RefreshOptions{}); !errors.Is(err, ErrProxyRequired) {
		t.Fatalf("err = %v, want ErrProxyRequired", err)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("the backend was contacted without a proxy")
	}
	if _, err := e.svc.Usage(context.Background(), a.ID, RefreshOptions{AllowDirect: true}); err != nil {
		t.Fatal(err)
	}
}

func TestUsageRenewsAnExpiredTokenOnceAndRetries(t *testing.T) {
	e := newSvcEnv(t)
	a := e.add(t, "a@example.com", "acct-a", "http://h:1")
	var usageCalls, refreshCalls int32
	e.issuer(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			atomic.AddInt32(&refreshCalls, 1)
			_, _ = w.Write([]byte(`{"access_token":"at-fresh","refresh_token":"rt-rotated"}`))
		case "/wham/usage":
			atomic.AddInt32(&usageCalls, 1)
			if r.Header.Get("Authorization") != "Bearer at-fresh" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(usageJSON))
		}
	})
	u, err := e.svc.Usage(context.Background(), a.ID, RefreshOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if u.Primary.UsedPercent != 42 || atomic.LoadInt32(&usageCalls) != 2 || atomic.LoadInt32(&refreshCalls) != 1 {
		t.Fatalf("usage=%+v usageCalls=%d refreshCalls=%d", u, usageCalls, refreshCalls)
	}
	got, _ := e.repo.GetByID(context.Background(), a.ID)
	if got.AccessToken != "at-fresh" || got.RefreshToken != "rt-rotated" {
		t.Fatalf("the renewed tokens were not stored: %+v", got)
	}
}

func TestUsageRevokedSessionMarksTheAccount(t *testing.T) {
	e := newSvcEnv(t)
	a := e.add(t, "a@example.com", "acct-a", "http://h:1")
	e.issuer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/wham/usage" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	})
	_, err := e.svc.Usage(context.Background(), a.ID, RefreshOptions{})
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("err = %v, want invalid_grant", err)
	}
	got, _ := e.repo.GetByID(context.Background(), a.ID)
	if got.Status != domain.AccountStatusError {
		t.Fatalf("status = %s, want error", got.Status)
	}
}

func TestUsageErrorsNeverLeakTokensOrBodies(t *testing.T) {
	e := newSvcEnv(t)
	a := e.add(t, "a@example.com", "acct-a", "http://h:1")
	e.issuer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal: token at-a@example.com and secret-body"))
	})
	_, err := e.svc.Usage(context.Background(), a.ID, RefreshOptions{})
	if err == nil || strings.Contains(err.Error(), "at-a@example.com") || strings.Contains(err.Error(), "secret-body") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v", err)
	}

	e.issuer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>not json secret-body</html>"))
	})
	_, err = e.svc.Usage(context.Background(), a.ID, RefreshOptions{})
	if err == nil || strings.Contains(err.Error(), "secret-body") {
		t.Fatalf("err = %v", err)
	}
}

func TestUsageFromPayloadFallbacks(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	// No rate_limit block: still readable, defaults to allowed.
	u := usageFromPayload(&usagePayload{PlanType: "free"}, now)
	if !u.Allowed || u.Primary != nil || u.Secondary != nil {
		t.Fatalf("usage = %+v", u)
	}
	// reset_after_seconds is used when reset_at is missing.
	w := windowFromPayload(&usageWindowPayload{UsedPercent: 10, LimitWindowSeconds: 100, ResetAfterSeconds: 60}, now)
	if !w.ResetAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("reset = %v", w.ResetAt)
	}
	if windowFromPayload(nil, now) != nil {
		t.Fatal("a missing window must stay missing")
	}
}
