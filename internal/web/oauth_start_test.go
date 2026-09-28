package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/config"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/oauth"
)

// fakeOAuthEngine records how the dashboard starts the flow and, like the real one, creates the
// account once the sign-in is "done".
type fakeOAuthEngine struct {
	repo domain.AccountRepository

	mu        sync.Mutex
	proxies   []string // proxy passed to each StartLoopbackFlowWithProxy call
	openerNil []bool   // whether each call would open the default browser
	direct    int      // calls to StartLoopbackFlow (no proxy): must stay zero
	openerRan int      // times a non-nil opener was invoked
}

func (f *fakeOAuthEngine) StartLoopbackFlow(context.Context, oauth.BrowserOpener, func(string)) (*domain.Account, error) {
	f.mu.Lock()
	f.direct++
	f.mu.Unlock()
	return nil, nil
}

func (f *fakeOAuthEngine) StartLoopbackFlowWithProxy(ctx context.Context, opener oauth.BrowserOpener, urlLogger func(string), proxyURL string) (*domain.Account, error) {
	f.mu.Lock()
	f.proxies = append(f.proxies, proxyURL)
	f.openerNil = append(f.openerNil, opener == nil)
	f.mu.Unlock()

	authURL := "https://accounts.google.com/o/oauth2/v2/auth?client_id=x&state=y"
	urlLogger(authURL)
	if opener != nil {
		_ = opener(authURL)
		f.mu.Lock()
		f.openerRan++
		f.mu.Unlock()
	}
	now := time.Now().UTC()
	acc := &domain.Account{ID: "new-acc", Email: "new@gmail.com", Status: domain.AccountStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := f.repo.Create(ctx, acc); err != nil {
		return nil, err
	}
	return acc, nil
}

func (f *fakeOAuthEngine) BuildAuthURL(string, string, string) string { return "" }
func (f *fakeOAuthEngine) HandleCallbackRequest(*http.Request) (*domain.Account, error) {
	return nil, nil
}
func (f *fakeOAuthEngine) RefreshToken(context.Context, string) (*oauth.TokenResponse, error) {
	return nil, nil
}
func (f *fakeOAuthEngine) RefreshTokenVia(context.Context, string, string) (*oauth.TokenResponse, error) {
	return nil, nil
}
func (f *fakeOAuthEngine) EnsureValidToken(_ context.Context, acc *domain.Account, _ time.Duration) (*domain.Account, error) {
	return acc, nil
}

func (f *fakeOAuthEngine) snapshot() (proxies []string, openerNil []bool, direct, openerRan int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.proxies...), append([]bool(nil), f.openerNil...), f.direct, f.openerRan
}

func newOAuthTestServer(t *testing.T) (*Server, *fakeOAuthEngine, domain.AccountRepository) {
	t.Helper()
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)
	engine := &fakeOAuthEngine{repo: accRepo}
	server, err := NewServer(accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, engine)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return server, engine, accRepo
}

func startOAuth(t *testing.T, server *Server, method string, body any) (int, map[string]any, string) {
	t.Helper()
	var rd *strings.Reader
	if body == nil {
		rd = strings.NewReader("")
	} else {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, "/oauth/start", rd)
	req.Host = "127.0.0.1:8080"
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out, rr.Body.String()
}

func waitForProxy(t *testing.T, repo domain.AccountRepository, id string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if acc, err := repo.GetByID(context.Background(), id); err == nil && acc.ProxyURL != "" {
			return acc.ProxyURL
		}
		time.Sleep(20 * time.Millisecond)
	}
	return ""
}

// The leak this guards against: the sign-in used to start with no proxy, so the token exchange and
// profile lookup left from the real IP. Without a proxy nothing may start at all.
func TestOAuthStart_RefusesWithoutAProxy(t *testing.T) {
	server, engine, _ := newOAuthTestServer(t)

	for name, body := range map[string]any{
		"no body":      nil,
		"empty body":   map[string]any{},
		"blank proxy":  map[string]any{"proxy_url": "   "},
		"bad format":   map[string]any{"proxy_url": "1.2.3.4:8080:alice:s3cretPW"},
		"unknown pool": map[string]any{"pool_id": "does-not-exist"},
	} {
		code, _, raw := startOAuth(t, server, http.MethodPost, body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%s)", name, code, raw)
		}
		if strings.Contains(raw, "s3cretPW") {
			t.Errorf("%s: response echoes the proxy password: %s", name, raw)
		}
	}
	if proxies, _, direct, _ := engine.snapshot(); len(proxies) != 0 || direct != 0 {
		t.Errorf("a refused request still started a flow (proxied=%d direct=%d)", len(proxies), direct)
	}
}

func TestOAuthStart_RejectsGET(t *testing.T) {
	server, engine, _ := newOAuthTestServer(t)
	if code, _, _ := startOAuth(t, server, http.MethodGet, nil); code != http.StatusMethodNotAllowed {
		t.Errorf("GET status %d, want 405 (a page must not be able to start a flow with an image tag)", code)
	}
	if proxies, _, direct, _ := engine.snapshot(); len(proxies) != 0 || direct != 0 {
		t.Error("GET started a flow")
	}
}

func TestOAuthStart_UsesTheProxyAndKeepsTheBrowserClosed(t *testing.T) {
	server, engine, repo := newOAuthTestServer(t)
	const proxy = "http://alice:s3cretPW@proxy.example.com:3128"

	code, out, raw := startOAuth(t, server, http.MethodPost, map[string]any{"proxy_url": proxy})
	if code != http.StatusOK || out["status"] != "started" {
		t.Fatalf("start: %d %s", code, raw)
	}
	if !strings.HasPrefix(out["auth_url"].(string), "https://accounts.google.com/") {
		t.Errorf("auth_url = %v, want the sign-in link back so it can be opened in an isolated profile", out["auth_url"])
	}
	if out["proxy"] != "http://***@proxy.example.com:3128" || out["browser_opened"] != false {
		t.Errorf("response = %v", out)
	}
	if strings.Contains(raw, "s3cretPW") || strings.Contains(raw, "alice") {
		t.Errorf("response leaks the proxy credentials: %s", raw)
	}

	// The account leaves the flow already bound to the proxy the exchange went through.
	if got := waitForProxy(t, repo, "new-acc"); got != proxy {
		t.Errorf("account proxy = %q, want it saved after the flow", got)
	}
	proxies, openerNil, direct, openerRan := engine.snapshot()
	if len(proxies) != 1 || proxies[0] != proxy || direct != 0 {
		t.Errorf("flow started with proxies=%v direct=%d", proxies, direct)
	}
	if openerNil[0] {
		t.Error("the default browser would have opened: it reaches Google from the real IP")
	}
	if openerRan != 1 {
		t.Errorf("the no-op opener ran %d times", openerRan)
	}
}

func TestOAuthStart_OpeningTheDefaultBrowserIsAnExplicitOptIn(t *testing.T) {
	server, engine, _ := newOAuthTestServer(t)
	code, out, raw := startOAuth(t, server, http.MethodPost, map[string]any{"proxy_url": "http://proxy.example.com:3128", "open_browser": true})
	if code != http.StatusOK || out["browser_opened"] != true {
		t.Fatalf("start: %d %s", code, raw)
	}
	if _, openerNil, _, _ := engine.snapshot(); len(openerNil) != 1 || !openerNil[0] {
		t.Errorf("open_browser=true should hand the flow a nil opener (default browser), got %v", openerNil)
	}
}

func TestOAuthStart_ResolvesAPoolProxyByID(t *testing.T) {
	server, engine, repo := newOAuthTestServer(t)
	const proxy = "http://alice:s3cretPW@pool.example.com:8080"
	pool, err := config.UpdateProxies(func([]string) ([]string, error) { return []string{proxy}, nil })
	if err != nil || len(pool) != 1 {
		t.Fatalf("seed pool: %v", err)
	}

	id := ""
	{
		req := httptest.NewRequest(http.MethodGet, "/api/proxies", nil)
		req.Host = "127.0.0.1:8080"
		rr := httptest.NewRecorder()
		server.ServeHTTP(rr, req)
		var list struct {
			Proxies []proxyPoolItem `json:"proxies"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &list)
		if len(list.Proxies) != 1 {
			t.Fatalf("pool listing: %s", rr.Body.String())
		}
		id = list.Proxies[0].ID
	}

	code, _, raw := startOAuth(t, server, http.MethodPost, map[string]any{"pool_id": id})
	if code != http.StatusOK {
		t.Fatalf("start with pool_id: %d %s", code, raw)
	}
	if strings.Contains(raw, "s3cretPW") {
		t.Errorf("response leaks the pool proxy credentials: %s", raw)
	}
	if got := waitForProxy(t, repo, "new-acc"); got != proxy {
		t.Errorf("account proxy = %q, want the pool entry", got)
	}
	if proxies, _, _, _ := engine.snapshot(); len(proxies) != 1 || proxies[0] != proxy {
		t.Errorf("flow started with %v, want the pool proxy resolved server-side", proxies)
	}
}
