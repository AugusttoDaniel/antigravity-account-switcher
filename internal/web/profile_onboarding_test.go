package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/adspower"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/config"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
)

// fakeProfileAPI stands in for AliasMode's Local API. reachable decides which base URLs answer.
type fakeProfileAPI struct {
	mu        sync.Mutex
	reachable map[string]bool
	pings     []string
	created   []adspower.CreateProfileRequest
	started   []string
	stopped   []string
	createErr error
	startErr  error
}

type fakeProfileView struct {
	f   *fakeProfileAPI
	url string
}

func (f *fakeProfileAPI) factory(baseURL, _ string) profileAPI {
	return &fakeProfileView{f: f, url: baseURL}
}

func (v *fakeProfileView) Ping(context.Context) error {
	v.f.mu.Lock()
	defer v.f.mu.Unlock()
	v.f.pings = append(v.f.pings, v.url)
	if !v.f.reachable[v.url] {
		return errors.New("connection refused")
	}
	return nil
}

func (v *fakeProfileView) CreateProfile(_ context.Context, req adspower.CreateProfileRequest) (string, error) {
	v.f.mu.Lock()
	defer v.f.mu.Unlock()
	if v.f.createErr != nil {
		return "", v.f.createErr
	}
	v.f.created = append(v.f.created, req)
	return "profile-1", nil
}

func (v *fakeProfileView) StartBrowser(_ context.Context, id string, _ bool) (*adspower.BrowserStartData, error) {
	v.f.mu.Lock()
	defer v.f.mu.Unlock()
	if v.f.startErr != nil {
		return nil, v.f.startErr
	}
	v.f.started = append(v.f.started, id)
	return &adspower.BrowserStartData{WS: adspower.BrowserWS{Puppeteer: "ws://127.0.0.1:9222/devtools/browser/abc"}, DebugPort: "9222"}, nil
}

func (v *fakeProfileView) StopBrowser(_ context.Context, id string) error {
	v.f.mu.Lock()
	defer v.f.mu.Unlock()
	v.f.stopped = append(v.f.stopped, id)
	return nil
}

func (f *fakeProfileAPI) snapshot() (pings []string, created []adspower.CreateProfileRequest, started, stopped []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.pings...), append([]adspower.CreateProfileRequest(nil), f.created...),
		append([]string(nil), f.started...), append([]string(nil), f.stopped...)
}

const proxyWithSecret = "http://alice:s3cretPW@proxy.example.com:3128"

// profileTestSetup wires a server whose profile API is the fake, configured at apiURL.
func profileTestSetup(t *testing.T, apiURL, engine string, fake *fakeProfileAPI) (*Server, *fakeOAuthEngine, domain.AccountRepository, func() []string) {
	t.Helper()
	server, oauthEngine, repo := newOAuthTestServer(t)
	cfg := config.DefaultConfig()
	cfg.AdsPowerAPIURL, cfg.AdsPowerEngine = apiURL, engine
	server.api.SetConfig(cfg)
	server.api.profileAPIFactory = fake.factory

	var navigated []string
	var navMu sync.Mutex
	server.api.profileNavigate = func(_ context.Context, ws, port, target string) error {
		navMu.Lock()
		defer navMu.Unlock()
		navigated = append(navigated, ws+"|"+port+"|"+target)
		return nil
	}
	navigations := func() []string {
		navMu.Lock()
		defer navMu.Unlock()
		return append([]string(nil), navigated...)
	}
	return server, oauthEngine, repo, navigations
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestProfileOnboarding_CreatesTheProfileOpensItAndSavesTheAccount(t *testing.T) {
	fake := &fakeProfileAPI{reachable: map[string]bool{"http://127.0.0.1:50400": true}}
	server, engine, repo, navigations := profileTestSetup(t, "http://127.0.0.1:50400", "cloak", fake)

	code, out, raw := startOAuth(t, server, http.MethodPost, map[string]any{"mode": "profile", "proxy_url": proxyWithSecret})
	if code != http.StatusOK || out["mode"] != "profile" || out["profile_id"] != "profile-1" {
		t.Fatalf("start: %d %s", code, raw)
	}
	if strings.Contains(raw, "s3cretPW") || strings.Contains(raw, "alice") {
		t.Errorf("response leaks the proxy credentials: %s", raw)
	}

	// The profile is created bound to the account's proxy, with the configured engine.
	_, created, _, _ := fake.snapshot()
	if len(created) != 1 || created[0].Browser != "cloak" || created[0].ProxyConfig.ProxyHost != "proxy.example.com" ||
		created[0].ProxyConfig.ProxyUser != "alice" || created[0].ProxyConfig.ProxyPassword != "s3cretPW" {
		t.Fatalf("created profiles = %+v", created)
	}

	// The account leaves the flow with its proxy and the profile it was added through, and the
	// profile's browser is closed once the flow ends.
	if got := waitForProxy(t, repo, engine.id); got != proxyWithSecret {
		t.Errorf("account proxy = %q", got)
	}
	acc, _ := repo.GetByID(context.Background(), engine.id)
	if acc.AdsPowerProfileID != "profile-1" {
		t.Errorf("profile id recorded = %q", acc.AdsPowerProfileID)
	}
	waitFor(t, "the profile browser to be stopped", func() bool { _, _, _, stopped := fake.snapshot(); return len(stopped) == 1 })

	// The consent page was opened inside the profile's browser, not the default browser.
	navigated := navigations()
	if len(navigated) != 1 || !strings.HasPrefix(navigated[0], "ws://127.0.0.1:9222/devtools/browser/abc|9222|https://accounts.google.com/") {
		t.Errorf("navigated = %v", navigated)
	}
	if proxies, openerNil, direct, _ := engine.snapshot(); len(proxies) != 1 || proxies[0] != proxyWithSecret || direct != 0 || openerNil[0] {
		t.Errorf("flow started with proxies=%v openerNil=%v direct=%d", proxies, openerNil, direct)
	}
}

func TestProfileOnboarding_UsesTheConfiguredEngine(t *testing.T) {
	fake := &fakeProfileAPI{reachable: map[string]bool{"http://127.0.0.1:50400": true}}
	server, engine, repo, _ := profileTestSetup(t, "http://127.0.0.1:50400", "firefox", fake)
	if code, _, raw := startOAuth(t, server, http.MethodPost, map[string]any{"mode": "profile", "proxy_url": proxyWithSecret}); code != http.StatusOK {
		t.Fatalf("start: %d %s", code, raw)
	}
	waitForProxy(t, repo, engine.id)
	if _, created, _, _ := fake.snapshot(); len(created) != 1 || created[0].Browser != "firefox" {
		t.Errorf("created = %+v, want the configured engine", created)
	}
}

func TestProfileOnboarding_ReusesAnExistingProfile(t *testing.T) {
	fake := &fakeProfileAPI{reachable: map[string]bool{"http://127.0.0.1:50400": true}}
	server, engine, repo, _ := profileTestSetup(t, "http://127.0.0.1:50400", "", fake)
	code, out, raw := startOAuth(t, server, http.MethodPost, map[string]any{"mode": "profile", "proxy_url": proxyWithSecret, "profile_id": "existing-7"})
	if code != http.StatusOK || out["profile_id"] != "existing-7" {
		t.Fatalf("start: %d %s", code, raw)
	}
	waitForProxy(t, repo, engine.id)
	if _, created, started, _ := fake.snapshot(); len(created) != 0 || len(started) != 1 || started[0] != "existing-7" {
		t.Errorf("created=%d started=%v; an existing profile must be reused, not recreated", len(created), started)
	}
}

// Whatever goes wrong with the profile API, the flow must not start: falling back to a direct
// sign-in or to the link would defeat the isolation this mode exists for.
func TestProfileOnboarding_FailsClosed(t *testing.T) {
	cases := map[string]struct {
		fake   *fakeProfileAPI
		apiURL string
	}{
		"API not running":       {&fakeProfileAPI{reachable: map[string]bool{}}, "http://127.0.0.1:50400"},
		"create refused":        {&fakeProfileAPI{reachable: map[string]bool{"http://127.0.0.1:50400": true}, createErr: errors.New("This feature is only available in paid subscriptions (proxy alice:s3cretPW)")}, "http://127.0.0.1:50400"},
		"browser will not open": {&fakeProfileAPI{reachable: map[string]bool{"http://127.0.0.1:50400": true}, startErr: errors.New("browser start timed out")}, "http://127.0.0.1:50400"},
		"API on another host":   {&fakeProfileAPI{reachable: map[string]bool{"http://192.0.2.10:50400": true}}, "http://192.0.2.10:50400"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			server, engine, _, _ := profileTestSetup(t, c.apiURL, "cloak", c.fake)
			code, _, raw := startOAuth(t, server, http.MethodPost, map[string]any{"mode": "profile", "proxy_url": proxyWithSecret})
			if code != http.StatusBadGateway {
				t.Fatalf("status %d, want 502 (%s)", code, raw)
			}
			if strings.Contains(raw, "s3cretPW") || strings.Contains(raw, "alice") {
				t.Errorf("error leaks the proxy credentials: %s", raw)
			}
			if proxies, _, direct, _ := engine.snapshot(); len(proxies) != 0 || direct != 0 {
				t.Errorf("a flow started anyway (proxied=%d direct=%d)", len(proxies), direct)
			}
		})
	}
}

func TestProfileOnboarding_NeverContactsAnAPIOffThisMachine(t *testing.T) {
	fake := &fakeProfileAPI{reachable: map[string]bool{"http://192.0.2.10:50400": true}}
	server, _, _, _ := profileTestSetup(t, "http://192.0.2.10:50400", "cloak", fake)
	startOAuth(t, server, http.MethodPost, map[string]any{"mode": "profile", "proxy_url": proxyWithSecret})
	if pings, created, _, _ := fake.snapshot(); len(pings) != 0 || len(created) != 0 {
		t.Errorf("a non-local profile API was contacted (pings=%v created=%d): the proxy credentials would have gone to it", pings, len(created))
	}
}

func TestProfileOnboarding_StillRequiresAProxy(t *testing.T) {
	fake := &fakeProfileAPI{reachable: map[string]bool{"http://127.0.0.1:50400": true}}
	server, _, _, _ := profileTestSetup(t, "http://127.0.0.1:50400", "cloak", fake)
	if code, _, raw := startOAuth(t, server, http.MethodPost, map[string]any{"mode": "profile"}); code != http.StatusBadRequest {
		t.Errorf("no proxy: status %d, want 400 (%s)", code, raw)
	}
	if code, _, raw := startOAuth(t, server, http.MethodPost, map[string]any{"mode": "nonsense", "proxy_url": proxyWithSecret}); code != http.StatusBadRequest {
		t.Errorf("unknown mode: status %d, want 400 (%s)", code, raw)
	}
	if pings, created, _, _ := fake.snapshot(); len(pings) != 0 || len(created) != 0 {
		t.Errorf("the profile API was contacted before the request was valid")
	}
}

func TestOnboardingStatus(t *testing.T) {
	statusOf := func(fake *fakeProfileAPI, apiURL string) map[string]any {
		t.Helper()
		server, _, _, _ := profileTestSetup(t, apiURL, "", fake)
		req := httptest.NewRequest(http.MethodGet, "/api/onboarding/status", nil)
		req.Host = "127.0.0.1:8080"
		rr := httptest.NewRecorder()
		server.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Nothing configured: AliasMode's port is tried first, then ADS Power's.
	out := statusOf(&fakeProfileAPI{reachable: map[string]bool{"http://127.0.0.1:50400": true}}, "")
	if out["available"] != true || out["url"] != "http://127.0.0.1:50400" {
		t.Errorf("AliasMode auto-detect: %v", out)
	}
	out = statusOf(&fakeProfileAPI{reachable: map[string]bool{adspower.DefaultBaseURL: true}}, "")
	if out["available"] != true || out["url"] != adspower.DefaultBaseURL {
		t.Errorf("ADS Power fallback: %v", out)
	}
	out = statusOf(&fakeProfileAPI{reachable: map[string]bool{}}, "")
	if out["available"] != false || out["error"] == nil || len(out["tried"].([]any)) != 2 {
		t.Errorf("nothing running: %v", out)
	}
	// A configured URL is the only one tried.
	out = statusOf(&fakeProfileAPI{reachable: map[string]bool{"http://127.0.0.1:50400": true}}, "http://127.0.0.1:9999")
	if out["available"] != false || len(out["tried"].([]any)) != 1 {
		t.Errorf("configured URL should be the only candidate: %v", out)
	}
	// A URL off this machine is refused with a reason.
	out = statusOf(&fakeProfileAPI{}, "http://192.0.2.10:50400")
	if out["available"] != false || !strings.Contains(out["error"].(string), "on this machine") {
		t.Errorf("non-local URL: %v", out)
	}
}
