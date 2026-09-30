package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/codex"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/store/sqlite"
)

const codexTestProxy = "http://alice:s3cretPW@127.0.0.1:9"

type codexEnv struct {
	server *Server
	svc    *codex.Service
	repo   *sqlite.CodexAccountRepository
	home   string
}

func codexJWT(email, acct string) string {
	enc := base64.RawURLEncoding.EncodeToString
	payload, _ := json.Marshal(map[string]any{
		"email":                       email,
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": acct, "chatgpt_plan_type": "plus"},
	})
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + "." + enc([]byte("sig"))
}

// newCodexEnv serves the dashboard with a Codex service whose issuer is a fake. loginEmail is who
// the fake issuer says signed in.
func newCodexEnv(t *testing.T, loginEmail string) *codexEnv {
	t.Helper()
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)

	db, err := sqlite.Open(filepath.Join(t.TempDir(), "codex.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	repo := sqlite.NewCodexAccountRepository(db)

	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Content-Type"), "json") { // refresh
			_, _ = w.Write([]byte(`{"access_token":"at-new","refresh_token":"rt-new"}`))
			return
		}
		out, _ := json.Marshal(map[string]string{"id_token": codexJWT(loginEmail, "acct-login"), "access_token": "at-login", "refresh_token": "rt-login"})
		_, _ = w.Write(out)
	}))
	t.Cleanup(issuer.Close)

	home := t.TempDir()
	svc := &codex.Service{
		Repo: repo, Home: home,
		NewClient: func(string) (*codex.Client, error) {
			return &codex.Client{Issuer: issuer.URL, ClientID: "cid", HTTP: issuer.Client()}, nil
		},
		Now: func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) },
	}
	server, err := NewServer(accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, nil, WithCodexService(svc))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	server.api.codexRandomPort = true
	return &codexEnv{server: server, svc: svc, repo: repo, home: home}
}

func (e *codexEnv) call(t *testing.T, method, path string, body any) (int, string) {
	t.Helper()
	var rd *strings.Reader
	if body == nil {
		rd = strings.NewReader("")
	} else {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, path, rd)
	req.Host = "127.0.0.1:8080"
	rr := httptest.NewRecorder()
	e.server.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

func (e *codexEnv) seed(t *testing.T, email, acct, proxy string) *domain.CodexAccount {
	t.Helper()
	a, err := e.svc.AddFromTokens(context.Background(), &codex.TokenResponse{IDToken: codexJWT(email, acct), AccessToken: "at-" + acct, RefreshToken: "rt-" + acct}, proxy, "")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestCodex_DisabledWithoutService(t *testing.T) {
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)
	server, err := NewServer(accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/codex/accounts", nil)
	req.Host = "127.0.0.1:8080"
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501", rr.Code)
	}
}

func TestCodex_ListNeverExposesSecrets(t *testing.T) {
	e := newCodexEnv(t, "x@example.com")
	e.seed(t, "a@example.com", "acct-a", codexTestProxy)

	code, body := e.call(t, http.MethodGet, "/api/codex/accounts", nil)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	for _, secret := range []string{"s3cretPW", "alice", "rt-acct-a", "at-acct-a", "id_token", "refresh_token", "access_token"} {
		if strings.Contains(body, secret) {
			t.Errorf("the list leaks %q: %s", secret, body)
		}
	}
	var views []map[string]any
	if err := json.Unmarshal([]byte(body), &views); err != nil || len(views) != 1 {
		t.Fatalf("list = %s (%v)", body, err)
	}
	if views[0]["email"] != "a@example.com" || views[0]["plan_type"] != "plus" {
		t.Fatalf("view = %v", views[0])
	}
	if p, _ := views[0]["proxy_url"].(string); !strings.Contains(p, "127.0.0.1:9") || strings.Contains(p, "alice") {
		t.Fatalf("proxy_url = %q, want host without credentials", p)
	}
}

func TestCodex_MutationsAreRestrictedToPOST(t *testing.T) {
	e := newCodexEnv(t, "x@example.com")
	a := e.seed(t, "a@example.com", "acct-a", codexTestProxy)
	for _, path := range []string{"/api/codex/accounts/switch", "/api/codex/accounts/refresh", "/api/codex/accounts/remove", "/api/codex/accounts/proxy", "/api/codex/login/start"} {
		if code, _ := e.call(t, http.MethodGet, path+"?id="+a.ID, nil); code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405", path, code)
		}
	}
	if code, _ := e.call(t, http.MethodPost, "/api/codex/accounts", nil); code != http.StatusMethodNotAllowed {
		t.Errorf("POST /accounts = %d, want 405", code)
	}
}

func TestCodex_SwitchWritesAuthFile(t *testing.T) {
	e := newCodexEnv(t, "x@example.com")
	a := e.seed(t, "a@example.com", "acct-a", codexTestProxy)

	code, body := e.call(t, http.MethodPost, "/api/codex/accounts/switch", map[string]string{"id": a.ID})
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	if strings.Contains(body, "rt-acct-a") {
		t.Fatalf("the switch response leaks a token: %s", body)
	}
	f, err := codex.ReadAuthFile(codex.AuthPath(e.home))
	if err != nil || f == nil || f.Tokens.RefreshToken != "rt-acct-a" {
		t.Fatalf("auth.json = %+v, %v", f, err)
	}
	act, err := e.repo.GetActive(context.Background())
	if err != nil || act.ID != a.ID {
		t.Fatalf("active = %+v, %v", act, err)
	}
	if code, _ := e.call(t, http.MethodPost, "/api/codex/accounts/switch", map[string]string{"id": "missing"}); code != http.StatusNotFound {
		t.Fatalf("switch to a missing account = %d, want 404", code)
	}
}

func TestCodex_RefreshNeedsProxyThenRotates(t *testing.T) {
	e := newCodexEnv(t, "x@example.com")
	noProxy := e.seed(t, "a@example.com", "acct-a", "")
	if code, body := e.call(t, http.MethodPost, "/api/codex/accounts/refresh", map[string]string{"id": noProxy.ID}); code != http.StatusConflict {
		t.Fatalf("refresh without a proxy = %d, want 409 (%s)", code, body)
	}

	withProxy := e.seed(t, "b@example.com", "acct-b", codexTestProxy)
	code, body := e.call(t, http.MethodPost, "/api/codex/accounts/refresh", map[string]string{"id": withProxy.ID})
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	got, _ := e.repo.GetByID(context.Background(), withProxy.ID)
	if got.RefreshToken != "rt-new" || got.AccessToken != "at-new" {
		t.Fatalf("stored = %+v", got)
	}
}

func TestCodex_SetProxyValidatesAndNeverEchoes(t *testing.T) {
	e := newCodexEnv(t, "x@example.com")
	a := e.seed(t, "a@example.com", "acct-a", "")

	for name, body := range map[string]map[string]string{
		"blank":    {"id": a.ID, "proxy_url": " "},
		"bad form": {"id": a.ID, "proxy_url": "1.2.3.4:8080:alice:s3cretPW"},
		"no pool":  {"id": a.ID, "pool_id": "nope"},
	} {
		code, raw := e.call(t, http.MethodPost, "/api/codex/accounts/proxy", body)
		if code != http.StatusBadRequest || strings.Contains(raw, "s3cretPW") {
			t.Errorf("%s: %d %s", name, code, raw)
		}
	}
	code, raw := e.call(t, http.MethodPost, "/api/codex/accounts/proxy", map[string]string{"id": a.ID, "proxy_url": codexTestProxy})
	if code != http.StatusOK || strings.Contains(raw, "s3cretPW") {
		t.Fatalf("%d %s", code, raw)
	}
	got, _ := e.repo.GetByID(context.Background(), a.ID)
	if got.ProxyURL != codexTestProxy {
		t.Fatalf("stored proxy = %q", got.ProxyURL)
	}
}

func TestCodex_Remove(t *testing.T) {
	e := newCodexEnv(t, "x@example.com")
	a := e.seed(t, "a@example.com", "acct-a", "")
	if code, body := e.call(t, http.MethodPost, "/api/codex/accounts/remove", map[string]string{"id": a.ID}); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	if _, err := e.repo.GetByID(context.Background(), a.ID); err == nil {
		t.Fatal("the account is still there")
	}
}

func TestCodexLogin_RefusesWithoutAProxy(t *testing.T) {
	e := newCodexEnv(t, "x@example.com")
	for name, body := range map[string]any{
		"no body":     nil,
		"blank proxy": map[string]string{"proxy_url": "  "},
		"bad format":  map[string]string{"proxy_url": "1.2.3.4:8080:alice:s3cretPW"},
		"bad mode":    map[string]string{"proxy_url": codexTestProxy, "mode": "direct"},
	} {
		code, raw := e.call(t, http.MethodPost, "/api/codex/login/start", body)
		if code != http.StatusBadRequest || strings.Contains(raw, "s3cretPW") {
			t.Errorf("%s: %d %s", name, code, raw)
		}
	}
	// A refused request must not leave the login slot taken.
	if atomic.LoadInt32(&e.server.api.codexLoginBusy) != 0 {
		t.Fatal("a refused request kept the login slot busy")
	}
}

func TestCodexLogin_LinkModeCompletesAndIsSingleFlight(t *testing.T) {
	e := newCodexEnv(t, "new@example.com")

	code, raw := e.call(t, http.MethodPost, "/api/codex/login/start", map[string]string{"proxy_url": codexTestProxy})
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	if strings.Contains(raw, "s3cretPW") {
		t.Fatalf("the response leaks the proxy password: %s", raw)
	}
	var resp struct {
		Mode    string `json:"mode"`
		AuthURL string `json:"auth_url"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil || resp.AuthURL == "" || resp.Mode != "link" {
		t.Fatalf("response = %s (%v)", raw, err)
	}

	// Only one sign-in at a time: the callback port is shared.
	if code, _ := e.call(t, http.MethodPost, "/api/codex/login/start", map[string]string{"proxy_url": codexTestProxy}); code != http.StatusConflict {
		t.Fatalf("second start = %d, want 409", code)
	}

	// The user finishes consent: the browser hits the loopback callback.
	u, err := url.Parse(resp.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
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
			if accs[0].Email != "new@example.com" || accs[0].ProxyURL != codexTestProxy || accs[0].RefreshToken != "rt-login" {
				t.Fatalf("stored = %+v", accs[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the account was never stored")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The slot frees once the flow is over.
	for i := 0; atomic.LoadInt32(&e.server.api.codexLoginBusy) != 0; i++ {
		if i > 200 {
			t.Fatal("the login slot was never released")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCodexLogin_ManualFallbackWhenThePortCannotBeOpened(t *testing.T) {
	e := newCodexEnv(t, "manual@example.com")
	// Something else holds the port, as Windows does for 1374-1473.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blocker.Close() })
	e.server.api.codexRandomPort = false
	e.server.api.codexPort = blocker.Addr().(*net.TCPAddr).Port

	// Nothing is waiting yet.
	if code, _ := e.call(t, http.MethodPost, "/api/codex/login/complete", map[string]string{"url": "x"}); code != http.StatusConflict {
		t.Fatalf("complete with no sign-in = %d, want 409", code)
	}

	code, raw := e.call(t, http.MethodPost, "/api/codex/login/start", map[string]string{"proxy_url": codexTestProxy})
	if code != http.StatusOK {
		t.Fatalf("start = %d: %s", code, raw)
	}
	var resp struct {
		AuthURL string `json:"auth_url"`
		Manual  bool   `json:"manual"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil || !resp.Manual || resp.AuthURL == "" {
		t.Fatalf("response = %s (%v); want manual with a link", raw, err)
	}
	u, _ := url.Parse(resp.AuthURL)
	redirect := u.Query().Get("redirect_uri")
	state := u.Query().Get("state")

	// A wrong paste is refused without ending the sign-in, and without echoing the code.
	code, raw = e.call(t, http.MethodPost, "/api/codex/login/complete", map[string]string{"url": redirect + "?code=LEAKYCODE&state=nope"})
	if code != http.StatusBadRequest || strings.Contains(raw, "LEAKYCODE") {
		t.Fatalf("wrong paste = %d %s", code, raw)
	}
	if atomic.LoadInt32(&e.server.api.codexLoginBusy) != 1 {
		t.Fatal("a wrong paste ended the sign-in")
	}

	code, raw = e.call(t, http.MethodPost, "/api/codex/login/complete", map[string]string{"url": redirect + "?code=the-code&state=" + state})
	if code != http.StatusOK {
		t.Fatalf("good paste = %d %s", code, raw)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		accs, _ := e.repo.List(context.Background())
		if len(accs) == 1 {
			if accs[0].Email != "manual@example.com" || accs[0].ProxyURL != codexTestProxy {
				t.Fatalf("stored = %+v", accs[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the account was never stored")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for i := 0; atomic.LoadInt32(&e.server.api.codexLoginBusy) != 0; i++ {
		if i > 200 {
			t.Fatal("the login slot was never released")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Once it is over, a late paste finds nothing waiting.
	if code, _ := e.call(t, http.MethodPost, "/api/codex/login/complete", map[string]string{"url": redirect + "?code=the-code&state=" + state}); code != http.StatusConflict {
		t.Fatalf("late paste = %d, want 409", code)
	}
}
