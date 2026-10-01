package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/adspower"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/config"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
)

// codexProfileEnv serves the dashboard with a Codex service and a fake AliasMode that already holds the
// given profiles, with codexTestProxy in the pool.
func codexProfileEnv(t *testing.T, loginEmail string, profiles ...adspower.Profile) (*codexEnv, *fakeProfileAPI) {
	t.Helper()
	fake := &fakeProfileAPI{reachable: map[string]bool{"http://127.0.0.1:50400": true}, profiles: profiles}
	e := newCodexEnv(t, loginEmail)
	cfg := config.DefaultConfig()
	cfg.AdsPowerAPIURL = "http://127.0.0.1:50400"
	e.server.api.SetConfig(cfg)
	e.server.api.profileAPIFactory = fake.factory
	e.server.api.profileNavigate = func(context.Context, string, string, string) error { return nil }
	seedPool(t, codexTestProxy)
	return e, fake
}

func startCodexProfileLogin(t *testing.T, e *codexEnv, body map[string]any) (int, string) {
	t.Helper()
	return e.call(t, http.MethodPost, "/api/codex/login/start", body)
}

// The defect: choosing an existing profile in "Add Codex account" created ANOTHER profile, because only
// the Google dialog knew how to reuse one.
func TestCodexLogin_AnExistingProfileIsReusedNotRecreated(t *testing.T) {
	e, fake := codexProfileEnv(t, "new@example.com", adspower.Profile{UserID: "p-free", Name: "proxy-127.0.0.1-9"})

	code, raw := startCodexProfileLogin(t, e, map[string]any{"mode": "profile", "profile_id": "p-free"})
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	if strings.Contains(raw, "s3cretPW") || strings.Contains(raw, "alice") {
		t.Fatalf("the response leaks the proxy credentials: %s", raw)
	}
	var resp struct {
		ProfileID string `json:"profile_id"`
		AuthURL   string `json:"auth_url"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil || resp.ProfileID != "p-free" || resp.AuthURL == "" {
		t.Fatalf("response = %s (%v)", raw, err)
	}

	// The user finishes consent in that profile's browser.
	u, _ := url.Parse(resp.AuthURL)
	q := url.Values{"state": {u.Query().Get("state")}, "code": {"the-code"}}
	hit, err := http.Get(u.Query().Get("redirect_uri") + "?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	hit.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		accs, _ := e.repo.List(context.Background())
		if len(accs) == 1 {
			if accs[0].AdsPowerProfileID != "p-free" || accs[0].ProxyURL != codexTestProxy {
				t.Fatalf("stored = %+v", accs[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the account was never stored")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, created, started, _ := fake.snapshot(); len(created) != 0 || len(started) != 1 || started[0] != "p-free" {
		t.Fatalf("created=%d started=%v: the existing profile must be opened, not recreated", len(created), started)
	}
}

func TestCodexLogin_ExistingProfileRefusals(t *testing.T) {
	e, fake := codexProfileEnv(t, "x@example.com",
		adspower.Profile{UserID: "p-codex", Name: "proxy-127.0.0.1-9"},
		adspower.Profile{UserID: "p-free", Name: "proxy-127.0.0.1-9"},
		adspower.Profile{UserID: "p-nopool", Name: "proxy-10.0.0.7-6001"},
		adspower.Profile{UserID: "p-manual", Name: "my own profile"},
	)
	owner := e.seed(t, "owner@example.com", "acct-o", codexTestProxy)
	if err := e.repo.UpdateAdsPowerProfileID(context.Background(), owner.ID, "p-codex"); err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		body map[string]any
		want int
	}{
		"profile that does not exist":   {map[string]any{"mode": "profile", "profile_id": "nope"}, http.StatusNotFound},
		"another Codex account's":       {map[string]any{"mode": "profile", "profile_id": "p-codex"}, http.StatusConflict},
		"proxy missing from the pool":   {map[string]any{"mode": "profile", "profile_id": "p-nopool"}, http.StatusConflict},
		"profile with an unknown proxy": {map[string]any{"mode": "profile", "profile_id": "p-manual"}, http.StatusBadRequest},
		"a different proxy alongside":   {map[string]any{"mode": "profile", "profile_id": "p-free", "proxy_url": "http://bob:pw2@10.0.0.9:6000"}, http.StatusBadRequest},
	}
	for name, c := range cases {
		code, raw := startCodexProfileLogin(t, e, c.body)
		if code != c.want {
			t.Errorf("%s: status %d, want %d (%s)", name, code, c.want, raw)
		}
		for _, secret := range []string{"s3cretPW", "alice", "pw2", "bob"} {
			if strings.Contains(raw, secret) {
				t.Errorf("%s: the error leaks %q", name, secret)
			}
		}
	}
	if _, _, started, _ := fake.snapshot(); len(started) != 0 {
		t.Fatalf("a refused request opened a browser: %v", started)
	}
}

// A Google account and an OpenAI account see nothing of each other, so they may share a profile and
// its proxy; two accounts of the SAME service may not.
func TestProfiles_OneAccountPerServiceButOneOfEachMayShare(t *testing.T) {
	e, _ := codexProfileEnv(t, "x@example.com",
		adspower.Profile{UserID: "p-shared", Name: "proxy-127.0.0.1-9"},
		adspower.Profile{UserID: "p-google", Name: "proxy-127.0.0.1-8"},
	)
	ctx := context.Background()
	seedPool(t, codexTestProxy, "http://alice:s3cretPW@127.0.0.1:8")

	now := time.Now().UTC()
	for _, g := range []struct{ id, email, profile string }{
		{"g1", "google-shared@example.com", "p-shared"},
		{"g2", "google-only@example.com", "p-google"},
	} {
		if err := e.server.api.accountRepo.Create(ctx, &domain.Account{ID: g.id, Email: g.email, AdsPowerProfileID: g.profile, Status: domain.AccountStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	codexOwner := e.seed(t, "codex-shared@example.com", "acct-s", codexTestProxy)
	if err := e.repo.UpdateAdsPowerProfileID(ctx, codexOwner.ID, "p-shared"); err != nil {
		t.Fatal(err)
	}

	// The listing tells the page who holds each profile, per service.
	req, _ := http.NewRequest(http.MethodGet, "", nil)
	_ = req
	code, raw := e.call(t, http.MethodGet, "/api/onboarding/profiles", nil)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	var out struct {
		Profiles []profileChoice `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	by := map[string]profileChoice{}
	for _, p := range out.Profiles {
		by[p.ID] = p
	}
	if p := by["p-shared"]; p.LinkedTo != "google-shared@example.com" || p.CodexLinkedTo != "codex-shared@example.com" {
		t.Fatalf("shared profile = %+v", p)
	}
	if p := by["p-google"]; p.LinkedTo != "google-only@example.com" || p.CodexLinkedTo != "" {
		t.Fatalf("google-only profile = %+v", p)
	}

	// Codex may take a profile a Google account holds...
	if _, status, err := e.server.api.existingProfileProxyFor(ctx, "p-google", "", providerCodex); err != nil || status != http.StatusOK {
		t.Fatalf("Codex on a Google-only profile: %d %v", status, err)
	}
	// ...but not one another Codex account holds.
	if _, status, err := e.server.api.existingProfileProxyFor(ctx, "p-shared", "", providerCodex); err == nil || status != http.StatusConflict {
		t.Fatalf("Codex on a Codex-held profile: %d %v", status, err)
	}
	// Google may take a profile a Codex account holds, but not one another Google account holds.
	if _, status, err := e.server.api.existingProfileProxyFor(ctx, "p-shared", "", providerGoogle); err == nil || status != http.StatusConflict {
		t.Fatalf("Google on a Google-held profile: %d %v", status, err)
	}
	other := e.seed(t, "codex-only@example.com", "acct-c", codexTestProxy)
	if err := e.repo.UpdateAdsPowerProfileID(ctx, other.ID, "p-google"); err != nil {
		t.Fatal(err)
	}
	if _, status, err := e.server.api.existingProfileProxyFor(ctx, "p-google", "", providerCodex); err == nil || status != http.StatusConflict {
		t.Fatalf("Codex on a profile it now holds: %d %v", status, err)
	}
}
