package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/adspower"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/config"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
)

func seedPool(t *testing.T, proxies ...string) {
	t.Helper()
	if _, err := config.UpdateProxies(func([]string) ([]string, error) { return proxies, nil }); err != nil {
		t.Fatal(err)
	}
}

func TestEndpointFromProfileName(t *testing.T) {
	good := map[string]string{
		"proxy-31.58.9.4-6077":           "31.58.9.4:6077",
		"proxy-proxy.example.com-3128":   "proxy.example.com:3128",
		"proxy-my-host.example.com-8080": "my-host.example.com:8080",
		"  proxy-1.2.3.4-80  ":           "1.2.3.4:80",
	}
	for in, want := range good {
		if got, ok := endpointFromProfileName(in); !ok || got != want {
			t.Errorf("%q -> %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "my profile", "proxy-", "proxy-1.2.3.4", "proxy-1.2.3.4-", "proxy--80", "proxy-1.2.3.4-abc", "proxy-1.2.3.4-0", "proxy-1.2.3.4-70000", "Proxy-1.2.3.4-80"} {
		if got, ok := endpointFromProfileName(in); ok {
			t.Errorf("%q must not parse, got %q", in, got)
		}
	}
}

func profilesGet(t *testing.T, server *Server) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/onboarding/profiles", nil)
	req.Host = "127.0.0.1:8080"
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

func TestProfilesEndpoint_ListsLinksAndPoolMatches(t *testing.T) {
	fake := &fakeProfileAPI{reachable: map[string]bool{"http://127.0.0.1:50400": true}, profiles: []adspower.Profile{
		{UserID: "p-free", Name: "proxy-proxy.example.com-3128"},
		{UserID: "p-used", Name: "proxy-10.0.0.9-6000"},
		{UserID: "p-nopool", Name: "proxy-10.0.0.7-6001"},
		{UserID: "p-manual", Name: "my own profile"},
	}}
	server, _, repo, _ := profileTestSetup(t, "http://127.0.0.1:50400", "", fake)
	seedPool(t, proxyWithSecret, "http://bob:pw2@10.0.0.9:6000")
	now := time.Now().UTC()
	if err := repo.Create(context.Background(), &domain.Account{ID: "acc-used", Email: "owner@example.com", AdsPowerProfileID: "p-used", Status: domain.AccountStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	code, raw := profilesGet(t, server)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	for _, secret := range []string{"s3cretPW", "alice", "pw2", "bob"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("the list leaks %q: %s", secret, raw)
		}
	}
	var out struct {
		Profiles []profileChoice `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil || len(out.Profiles) != 4 {
		t.Fatalf("profiles = %s (%v)", raw, err)
	}
	by := map[string]profileChoice{}
	for _, p := range out.Profiles {
		by[p.ID] = p
	}
	if p := by["p-free"]; p.Proxy != "proxy.example.com:3128" || !p.ProxyInPool || p.LinkedTo != "" {
		t.Errorf("free = %+v", p)
	}
	if p := by["p-used"]; p.LinkedTo != "owner@example.com" || !p.ProxyInPool {
		t.Errorf("used = %+v", p)
	}
	if p := by["p-nopool"]; p.Proxy != "10.0.0.7:6001" || p.ProxyInPool {
		t.Errorf("not in the pool = %+v", p)
	}
	if p := by["p-manual"]; p.Proxy != "" || p.ProxyInPool {
		t.Errorf("manual = %+v", p)
	}
}

func TestProfilesEndpoint_NeedsTheProfileAPIAndGET(t *testing.T) {
	fake := &fakeProfileAPI{reachable: map[string]bool{}}
	server, _, _, _ := profileTestSetup(t, "http://127.0.0.1:50400", "", fake)
	if code, _ := profilesGet(t, server); code != http.StatusBadGateway {
		t.Fatalf("no API: status %d, want 502", code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/onboarding/profiles", nil)
	req.Host = "127.0.0.1:8080"
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", rr.Code)
	}
}

func startWithProfile(t *testing.T, server *Server, body map[string]any) (int, string) {
	t.Helper()
	code, _, raw := startOAuth(t, server, http.MethodPost, body)
	return code, raw
}

func TestExistingProfile_TakesItsProxyFromThePoolAndNeverSendsOne(t *testing.T) {
	fake := &fakeProfileAPI{reachable: map[string]bool{"http://127.0.0.1:50400": true},
		profiles: []adspower.Profile{{UserID: "p-free", Name: "proxy-proxy.example.com-3128"}}}
	server, engine, repo, _ := profileTestSetup(t, "http://127.0.0.1:50400", "", fake)
	seedPool(t, proxyWithSecret)

	code, raw := startWithProfile(t, server, map[string]any{"mode": "profile", "profile_id": "p-free"})
	if code != http.StatusOK || strings.Contains(raw, "s3cretPW") {
		t.Fatalf("start: %d %s", code, raw)
	}
	if got := waitForProxy(t, repo, engine.id); got != proxyWithSecret {
		t.Fatalf("the account proxy = %q, want the pool's", got)
	}
	acc, _ := repo.GetByID(context.Background(), engine.id)
	if acc.AdsPowerProfileID != "p-free" {
		t.Fatalf("profile recorded = %q", acc.AdsPowerProfileID)
	}
	if _, created, started, _ := fake.snapshot(); len(created) != 0 || len(started) != 1 || started[0] != "p-free" {
		t.Fatalf("created=%d started=%v", len(created), started)
	}
}

func TestExistingProfile_RefusesWhatCouldLeakOrMixAccounts(t *testing.T) {
	fake := &fakeProfileAPI{reachable: map[string]bool{"http://127.0.0.1:50400": true}, profiles: []adspower.Profile{
		{UserID: "p-free", Name: "proxy-proxy.example.com-3128"},
		{UserID: "p-used", Name: "proxy-proxy.example.com-3128"},
		{UserID: "p-nopool", Name: "proxy-10.0.0.7-6001"},
		{UserID: "p-manual", Name: "my own profile"},
	}}
	server, engine, repo, _ := profileTestSetup(t, "http://127.0.0.1:50400", "", fake)
	seedPool(t, proxyWithSecret, "http://bob:pw2@10.0.0.9:6000")
	now := time.Now().UTC()
	if err := repo.Create(context.Background(), &domain.Account{ID: "acc-used", Email: "owner@example.com", AdsPowerProfileID: "p-used", Status: domain.AccountStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		body map[string]any
		want int
	}{
		"profile that does not exist":   {map[string]any{"mode": "profile", "profile_id": "nope"}, http.StatusNotFound},
		"profile of another account":    {map[string]any{"mode": "profile", "profile_id": "p-used"}, http.StatusConflict},
		"proxy missing from the pool":   {map[string]any{"mode": "profile", "profile_id": "p-nopool"}, http.StatusConflict},
		"profile with an unknown proxy": {map[string]any{"mode": "profile", "profile_id": "p-manual", "proxy_url": proxyWithSecret}, http.StatusBadRequest},
		"a different proxy alongside":   {map[string]any{"mode": "profile", "profile_id": "p-free", "proxy_url": "http://bob:pw2@10.0.0.9:6000"}, http.StatusBadRequest},
	}
	for name, c := range cases {
		code, raw := startWithProfile(t, server, c.body)
		if code != c.want {
			t.Errorf("%s: status %d, want %d (%s)", name, code, c.want, raw)
		}
		for _, secret := range []string{"s3cretPW", "alice", "pw2", "bob"} {
			if strings.Contains(raw, secret) {
				t.Errorf("%s: the error leaks %q: %s", name, secret, raw)
			}
		}
	}
	if proxies, _, direct, _ := engine.snapshot(); len(proxies) != 0 || direct != 0 {
		t.Fatalf("a refused request still started a flow (proxied=%d direct=%d)", len(proxies), direct)
	}
	if _, _, started, _ := fake.snapshot(); len(started) != 0 {
		t.Fatalf("a refused request opened a browser: %v", started)
	}
}

func TestExistingProfile_AcceptsTheMatchingProxyChosenAlongside(t *testing.T) {
	fake := &fakeProfileAPI{reachable: map[string]bool{"http://127.0.0.1:50400": true},
		profiles: []adspower.Profile{{UserID: "p-free", Name: "proxy-proxy.example.com-3128"}}}
	server, engine, repo, _ := profileTestSetup(t, "http://127.0.0.1:50400", "", fake)
	seedPool(t, proxyWithSecret)
	code, raw := startWithProfile(t, server, map[string]any{"mode": "profile", "profile_id": "p-free", "proxy_url": proxyWithSecret})
	if code != http.StatusOK {
		t.Fatalf("start: %d %s", code, raw)
	}
	waitForProxy(t, repo, engine.id)
}
