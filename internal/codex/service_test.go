package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/store/sqlite"
)

// ---- loopback login ----

func loginIssuer(t *testing.T) *Client {
	return fakeIssuer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("code") != "good-code" || r.PostForm.Get("code_verifier") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id_token":"i","access_token":"a","refresh_token":"r"}`))
	})
}

// browserHit simulates the user finishing consent: it calls the callback with the given params
// merged over the real state from the authorize URL.
func browserHit(t *testing.T, params map[string]string) func(string) error {
	return func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := url.Values{}
		q.Set("state", u.Query().Get("state"))
		for k, v := range params {
			if v == "" {
				q.Del(k)
			} else {
				q.Set(k, v)
			}
		}
		go func() {
			resp, err := http.Get(u.Query().Get("redirect_uri") + "?" + q.Encode())
			if err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
}

func TestLoginCompletesThroughLoopback(t *testing.T) {
	tr, err := Login(context.Background(), LoginOptions{
		Client: loginIssuer(t), Timeout: 5 * time.Second,
		Opener: browserHit(t, map[string]string{"code": "good-code"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if tr.RefreshToken != "r" || tr.AccessToken != "a" {
		t.Fatalf("tokens = %+v", tr)
	}
}

func TestLoginRejectsWrongState(t *testing.T) {
	_, err := Login(context.Background(), LoginOptions{
		Client: loginIssuer(t), Timeout: 5 * time.Second,
		Opener: browserHit(t, map[string]string{"code": "good-code", "state": "forged"}),
	})
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("err = %v, want a state mismatch", err)
	}
}

func TestLoginSurfacesProviderErrorWithoutEcho(t *testing.T) {
	_, err := Login(context.Background(), LoginOptions{
		Client: loginIssuer(t), Timeout: 5 * time.Second,
		Opener: browserHit(t, map[string]string{"error": "access_denied", "code": ""}),
	})
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoginTimesOut(t *testing.T) {
	_, err := Login(context.Background(), LoginOptions{
		Client: loginIssuer(t), Timeout: 200 * time.Millisecond, Opener: func(string) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoginReportsBusyPort(t *testing.T) {
	first := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, _ = Login(ctx, LoginOptions{Client: loginIssuer(t), Timeout: 5 * time.Second, Opener: func(u string) error { first <- u; return nil }})
	}()
	u, _ := url.Parse(<-first)
	redirect, _ := url.Parse(u.Query().Get("redirect_uri"))
	port := 0
	for _, r := range redirect.Port() {
		port = port*10 + int(r-'0')
	}
	_, err := Login(context.Background(), LoginOptions{Client: loginIssuer(t), Port: port, Opener: func(string) error { return nil }})
	if err == nil || !strings.Contains(err.Error(), "cannot be opened") {
		t.Fatalf("err = %v, want a port error", err)
	}
}

// ---- service ----

type svcEnv struct {
	svc    *Service
	repo   *sqlite.CodexAccountRepository
	home   string
	issuer func(http.HandlerFunc)
}

func newSvcEnv(t *testing.T) *svcEnv {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &svcEnv{repo: sqlite.NewCodexAccountRepository(db), home: t.TempDir()}
	var handler http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) { http.Error(w, "not set", 500) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler(w, r) }))
	t.Cleanup(srv.Close)
	e.issuer = func(h http.HandlerFunc) { handler = h }
	e.svc = &Service{
		Repo: e.repo, Home: e.home,
		Usages:  e.repo,
		Warmups: e.repo,
		NewClient: func(string) (*Client, error) {
			return &Client{Issuer: srv.URL, ClientID: "cid", HTTP: &http.Client{Transport: localOnly{srv.Client().Transport}}, BackendURL: srv.URL}, nil
		},
		Now: func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) },
	}
	return e
}

func idTok(t *testing.T, email, acct string) string {
	return jwtWith(t, map[string]any{
		"email":                       email,
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": acct, "chatgpt_plan_type": "plus"},
	})
}

func (e *svcEnv) add(t *testing.T, email, acct, proxy string) *domain.CodexAccount {
	t.Helper()
	a, err := e.svc.AddFromTokens(context.Background(), &TokenResponse{IDToken: idTok(t, email, acct), AccessToken: "at-" + email, RefreshToken: "rt-" + email}, proxy, "")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (e *svcEnv) authFile(t *testing.T) *AuthFile {
	t.Helper()
	f, err := ReadAuthFile(AuthPath(e.home))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestServiceAddAndImport(t *testing.T) {
	e := newSvcEnv(t)
	a := e.add(t, "a@example.com", "acc-a", "http://u:p@h:1")
	if a.Email != "a@example.com" || a.ChatGPTAccountID != "acc-a" || a.PlanType != "plus" || a.ProxyURL != "http://u:p@h:1" {
		t.Fatalf("added = %+v", a)
	}
	if _, err := e.svc.AddFromTokens(context.Background(), &TokenResponse{AccessToken: "x"}, "", ""); err == nil {
		t.Fatal("a login with no id_token/refresh token must be refused")
	}

	src := filepath.Join(t.TempDir(), "auth.json")
	if err := WriteAuthFile(src, NewChatGPTAuthFile(Tokens{IDToken: idTok(t, "b@example.com", "acc-b"), AccessToken: "ab", RefreshToken: "rb"}, time.Now())); err != nil {
		t.Fatal(err)
	}
	b, err := e.svc.ImportAuthFile(context.Background(), src, "http://h:2")
	if err != nil || b.Email != "b@example.com" || b.RefreshToken != "rb" {
		t.Fatalf("imported = %+v, %v", b, err)
	}

	apiKey := filepath.Join(t.TempDir(), "key.json")
	k := "sk-x"
	if err := WriteAuthFile(apiKey, &AuthFile{OpenAIAPIKey: &k}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ImportAuthFile(context.Background(), apiKey, ""); err == nil {
		t.Fatal("an API-key auth.json has no ChatGPT login to import")
	}
}

func TestSwitchWritesAuthFileAndActivates(t *testing.T) {
	e := newSvcEnv(t)
	a := e.add(t, "a@example.com", "acc-a", "http://h:1")
	got, err := e.svc.Switch(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsActive {
		t.Fatal("the account is not active")
	}
	f := e.authFile(t)
	if f.AuthMode != "chatgpt" || f.Tokens.RefreshToken != "rt-a@example.com" || f.Tokens.AccountID != "acc-a" {
		t.Fatalf("auth.json = %+v / %+v", f, f.Tokens)
	}
	if _, err := e.svc.Switch(context.Background(), "missing"); !errors.Is(err, domain.ErrCodexAccountNotFound) {
		t.Fatalf("switch to a missing account = %v", err)
	}
	// A failed switch leaves auth.json and the active account alone.
	if e.authFile(t).Tokens.RefreshToken != "rt-a@example.com" {
		t.Fatal("a failed switch touched auth.json")
	}
	_ = e.repo.UpdateStatus(context.Background(), a.ID, domain.AccountStatusDisabled)
	if _, err := e.svc.Switch(context.Background(), a.ID); err == nil {
		t.Fatal("a disabled account must not be switchable")
	}
}

// The CLI rotates its refresh token inside auth.json. Switching away must bank the rotated token,
// or returning to the account later would use one the issuer already retired.
func TestSwitchSavesRotatedTokensOfOutgoingAccount(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	a := e.add(t, "a@example.com", "acc-a", "http://h:1")
	b := e.add(t, "b@example.com", "acc-b", "http://h:2")
	if _, err := e.svc.Switch(ctx, a.ID); err != nil {
		t.Fatal(err)
	}

	// Simulate the Codex CLI refreshing: same login, new tokens, in auth.json.
	rotated := NewChatGPTAuthFile(Tokens{IDToken: idTok(t, "a@example.com", "acc-a"), AccessToken: "at-rotated", RefreshToken: "rt-rotated", AccountID: "acc-a"}, time.Date(2026, 9, 30, 13, 0, 0, 0, time.UTC))
	if err := WriteAuthFile(AuthPath(e.home), rotated); err != nil {
		t.Fatal(err)
	}

	if _, err := e.svc.Switch(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	stored, _ := e.repo.GetByID(ctx, a.ID)
	if stored.RefreshToken != "rt-rotated" || stored.AccessToken != "at-rotated" {
		t.Fatalf("outgoing account kept %q / %q, want the rotated tokens", stored.RefreshToken, stored.AccessToken)
	}
	if e.authFile(t).Tokens.RefreshToken != "rt-b@example.com" {
		t.Fatal("auth.json does not hold the new account")
	}

	// Switching back restores the rotated login, not the original.
	if _, err := e.svc.Switch(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if e.authFile(t).Tokens.RefreshToken != "rt-rotated" {
		t.Fatal("switching back used a retired token")
	}
}

func TestCaptureRotationIgnoresUnknownLogins(t *testing.T) {
	e := newSvcEnv(t)
	e.add(t, "a@example.com", "acc-a", "http://h:1")
	// auth.json belongs to an account the switcher does not know: leave everything alone.
	if err := WriteAuthFile(AuthPath(e.home), NewChatGPTAuthFile(Tokens{IDToken: idTok(t, "stranger@example.com", "x"), AccessToken: "s", RefreshToken: "s"}, time.Now())); err != nil {
		t.Fatal(err)
	}
	if got, err := e.svc.CaptureRotation(context.Background()); got != nil || err != nil {
		t.Fatalf("capture = %+v, %v; want nil, nil", got, err)
	}
	all, _ := e.repo.List(context.Background())
	if len(all) != 1 || all[0].RefreshToken != "rt-a@example.com" {
		t.Fatalf("accounts changed: %+v", all)
	}
}

func TestRefreshRequiresProxy(t *testing.T) {
	e := newSvcEnv(t)
	called := false
	e.issuer(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{"access_token":"n"}`))
	})
	a := e.add(t, "a@example.com", "acc-a", "")
	if _, err := e.svc.Refresh(context.Background(), a.ID, RefreshOptions{}); !errors.Is(err, ErrProxyRequired) {
		t.Fatalf("err = %v, want ErrProxyRequired", err)
	}
	if called {
		t.Fatal("the issuer was contacted without a proxy")
	}
	if _, err := e.svc.Refresh(context.Background(), a.ID, RefreshOptions{AllowDirect: true}); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshRotatesAndRewritesAuthFileForActive(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	var sent string
	e.issuer(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent = body["refresh_token"]
		_, _ = w.Write([]byte(`{"access_token":"at-new","refresh_token":"rt-new"}`))
	})
	a := e.add(t, "a@example.com", "acc-a", "http://h:1")
	if _, err := e.svc.Switch(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	// The CLI rotated meanwhile: refresh must use the newest token, not the stored one.
	if err := WriteAuthFile(AuthPath(e.home), NewChatGPTAuthFile(Tokens{IDToken: idTok(t, "a@example.com", "acc-a"), AccessToken: "cli-at", RefreshToken: "cli-rt", AccountID: "acc-a"}, time.Now())); err != nil {
		t.Fatal(err)
	}

	got, err := e.svc.Refresh(ctx, a.ID, RefreshOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sent != "cli-rt" {
		t.Fatalf("refreshed with %q, want the CLI's newest token", sent)
	}
	if got.RefreshToken != "rt-new" || got.AccessToken != "at-new" {
		t.Fatalf("stored = %+v", got)
	}
	if f := e.authFile(t); f.Tokens.RefreshToken != "rt-new" || f.Tokens.AccessToken != "at-new" {
		t.Fatalf("auth.json kept a retired token: %+v", f.Tokens)
	}
}

func TestRefreshInvalidGrantMarksAccountErrored(t *testing.T) {
	e := newSvcEnv(t)
	e.issuer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	})
	a := e.add(t, "a@example.com", "acc-a", "http://h:1")
	_, err := e.svc.Refresh(context.Background(), a.ID, RefreshOptions{})
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("err = %v", err)
	}
	got, _ := e.repo.GetByID(context.Background(), a.ID)
	if got.Status != domain.AccountStatusError {
		t.Fatalf("status = %s, want error", got.Status)
	}
}

func TestRefreshKeepsFieldsTheIssuerOmits(t *testing.T) {
	e := newSvcEnv(t)
	e.issuer(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"access_token":"only-access"}`)) })
	a := e.add(t, "a@example.com", "acc-a", "http://h:1")
	got, err := e.svc.Refresh(context.Background(), a.ID, RefreshOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshToken != "rt-a@example.com" || got.IDToken != a.IDToken || got.AccessToken != "only-access" {
		t.Fatalf("stored = %+v", got)
	}
}

func TestResolve(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	a := e.add(t, "a@example.com", "personal", "")
	e.add(t, "team@example.com", "ws-1", "")
	e.add(t, "team@example.com", "ws-2", "")

	if got, err := e.svc.Resolve(ctx, "A@Example.com"); err != nil || got.ID != a.ID {
		t.Fatalf("by email = %+v, %v", got, err)
	}
	if got, err := e.svc.Resolve(ctx, a.ID); err != nil || got.ID != a.ID {
		t.Fatalf("by id = %+v, %v", got, err)
	}
	if _, err := e.svc.Resolve(ctx, "team@example.com"); err == nil || !strings.Contains(err.Error(), "share the email") {
		t.Fatalf("ambiguous email = %v", err)
	}
	if _, err := e.svc.Resolve(ctx, "nobody@example.com"); !errors.Is(err, domain.ErrCodexAccountNotFound) {
		t.Fatalf("unknown = %v", err)
	}
	if _, err := e.svc.Resolve(ctx, " "); err == nil {
		t.Fatal("an empty reference must be refused")
	}
}

func TestProxiedClientIsFailClosed(t *testing.T) {
	if _, err := ProxiedClient("not a proxy url"); err == nil {
		t.Fatal("an invalid proxy must be an error, not a direct client")
	}
	c, err := ProxiedClient("")
	if err != nil || c == nil {
		t.Fatalf("empty proxy = direct client: %v", err)
	}
	if _, err := ProxiedClient("http://user:pass@127.0.0.1:8080"); err != nil {
		t.Fatal(err)
	}
}

// ---- manual fallback (port cannot be bound) ----

// occupy binds a loopback port so the login's listener cannot have it.
func occupy(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

func TestLoginManualFallbackCompletesFromPastedURL(t *testing.T) {
	port := occupy(t)
	pasted := make(chan Paste, 4)
	manual := make(chan error, 1)

	var state string
	tr, err := Login(context.Background(), LoginOptions{
		Client: loginIssuer(t), Port: port, Timeout: 5 * time.Second, Pasted: pasted,
		OnManual: func(reason error) { manual <- reason },
		Opener: func(authURL string) error {
			u, _ := url.Parse(authURL)
			state = u.Query().Get("state")
			// The browser lands on a page that cannot load; the user pastes its address. A first
			// attempt with a typo must not end the sign-in.
			res1 := make(chan error, 1)
			pasted <- Paste{URL: "http://127.0.0.1:" + strconv.Itoa(port) + "/auth/callback?code=good-code&state=WRONG", Result: res1}
			go func() {
				if e := <-res1; e == nil {
					t.Error("a wrong state must be rejected")
				}
				pasted <- Paste{URL: "http://127.0.0.1:" + strconv.Itoa(port) + "/auth/callback?code=good-code&state=" + state}
			}()
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if tr.RefreshToken != "r" {
		t.Fatalf("tokens = %+v", tr)
	}
	if len(manual) != 1 {
		t.Fatal("OnManual was not called")
	}
}

func TestLoginWithoutPasteFailsWhenPortIsTaken(t *testing.T) {
	port := occupy(t)
	_, err := Login(context.Background(), LoginOptions{Client: loginIssuer(t), Port: port, Opener: func(string) error { return nil }})
	if err == nil || !strings.Contains(err.Error(), "excludedportrange") {
		t.Fatalf("err = %v, want a hint about reserved ports", err)
	}
}

func TestCodeFromRedirect(t *testing.T) {
	good := "http://127.0.0.1:1455/auth/callback?code=abc&scope=openid&state=ST"
	if c, err := codeFromRedirect(good, "ST"); err != nil || c != "abc" {
		t.Fatalf("good = %q, %v", c, err)
	}
	if c, err := codeFromRedirect("?code=abc&state=ST#frag", "ST"); err != nil || c != "abc" {
		t.Fatalf("bare query = %q, %v", c, err)
	}
	for name, raw := range map[string]string{
		"empty":      "  ",
		"no query":   "http://127.0.0.1:1455/auth/callback",
		"bad state":  "http://x/?code=SECRETCODE&state=OTHER",
		"no code":    "http://x/?state=ST",
		"error page": "http://x/?error=access_denied&state=ST",
	} {
		_, err := codeFromRedirect(raw, "ST")
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		if strings.Contains(err.Error(), "SECRETCODE") {
			t.Errorf("%s: the error echoes the pasted code: %v", name, err)
		}
	}
}

// localOnly refuses any request that is not for the loopback test server, so a test that forgets to
// point a URL at its fake can never reach the real internet.
type localOnly struct{ next http.RoundTripper }

func (l localOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if h := r.URL.Hostname(); h != "127.0.0.1" && h != "localhost" {
		return nil, errors.New("test tried to reach a non-loopback host: " + h)
	}
	return l.next.RoundTrip(r)
}
