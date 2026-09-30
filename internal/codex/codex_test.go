package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func jwtWith(t *testing.T, payload map[string]any) string {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(b) + "." + enc([]byte("sig"))
}

func TestParseIDToken(t *testing.T) {
	good := jwtWith(t, map[string]any{
		"email": "a@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acc-1",
			"chatgpt_plan_type":  "plus",
		},
	})
	c, err := ParseIDToken(good)
	if err != nil {
		t.Fatal(err)
	}
	if c.Email != "a@example.com" || c.AccountID != "acc-1" || c.PlanType != "plus" {
		t.Fatalf("claims = %+v", c)
	}

	// The email can sit under the profile claim instead.
	c, err = ParseIDToken(jwtWith(t, map[string]any{"https://api.openai.com/profile": map[string]any{"email": "p@example.com"}}))
	if err != nil || c.Email != "p@example.com" {
		t.Fatalf("profile email: %+v, %v", c, err)
	}

	for name, bad := range map[string]string{
		"not a jwt":  "abc",
		"bad base64": "a.!!!.c",
		"bad json":   "a." + base64.RawURLEncoding.EncodeToString([]byte("{")) + ".c",
		"no email":   jwtWith(t, map[string]any{"sub": "x"}),
	} {
		if _, err := ParseIDToken(bad); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestAuthFileRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "auth.json")
	want := NewChatGPTAuthFile(Tokens{IDToken: "i", AccessToken: "a", RefreshToken: "r", AccountID: "acc"}, time.Date(2026, 9, 30, 1, 2, 3, 0, time.UTC))
	if err := WriteAuthFile(path, want); err != nil {
		t.Fatal(err)
	}

	// Overwriting must replace, not append, and leave no temp files behind.
	want.Tokens.AccessToken = "a2"
	if err := WriteAuthFile(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadAuthFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthMode != "chatgpt" || got.Tokens.AccessToken != "a2" || got.Tokens.AccountID != "acc" || got.OpenAIAPIKey != nil {
		t.Fatalf("round trip = %+v / %+v", got, got.Tokens)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
		}
	}

	// The JSON keys are the ones the Codex CLI reads.
	raw, _ := os.ReadFile(path)
	for _, key := range []string{`"auth_mode": "chatgpt"`, `"OPENAI_API_KEY": null`, `"id_token"`, `"refresh_token"`, `"last_refresh"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("auth.json lacks %s:\n%s", key, raw)
		}
	}
}

func TestReadAuthFileMissingAndInvalid(t *testing.T) {
	dir := t.TempDir()
	f, err := ReadAuthFile(filepath.Join(dir, "none.json"))
	if f != nil || err != nil {
		t.Fatalf("missing file = %v, %v; want nil, nil", f, err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json secret-refresh-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = ReadAuthFile(bad)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "secret-refresh-token") {
		t.Fatalf("error leaks file content: %v", err)
	}
}

func TestHomeHonorsCODEXHOME(t *testing.T) {
	t.Setenv("CODEX_HOME", "/x/codex")
	if h, _ := Home(); h != "/x/codex" {
		t.Fatalf("home = %q", h)
	}
}

func TestPKCE(t *testing.T) {
	p, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Verifier) < 43 || p.Challenge == "" || p.Challenge == p.Verifier {
		t.Fatalf("pkce = %+v", p)
	}
	q, _ := NewPKCE()
	if q.Verifier == p.Verifier {
		t.Fatal("verifiers repeat")
	}
}

func TestAuthorizeURL(t *testing.T) {
	c := NewClient(nil)
	u, err := url.Parse(c.AuthorizeURL(RedirectURI(CallbackPort), "st", PKCE{Challenge: "ch"}))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host+u.Path != "https://auth.openai.com/oauth/authorize" {
		t.Fatalf("endpoint = %s", u)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"response_type": "code", "client_id": DefaultClientID, "redirect_uri": "http://127.0.0.1:1455/auth/callback",
		"code_challenge": "ch", "code_challenge_method": "S256", "state": "st", "scope": Scope,
		"id_token_add_organizations": "true", "codex_cli_simplified_flow": "true", "originator": Originator,
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
}

func fakeIssuer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{Issuer: srv.URL, ClientID: "cid", HTTP: srv.Client()}
}

func TestExchangeSendsFormAndParses(t *testing.T) {
	c := fakeIssuer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" || r.Method != http.MethodPost {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("content type = %s", ct)
		}
		b, _ := io.ReadAll(r.Body)
		f, _ := url.ParseQuery(string(b))
		for k, want := range map[string]string{"grant_type": "authorization_code", "client_id": "cid", "code": "the-code", "code_verifier": "ver", "redirect_uri": "http://r"} {
			if f.Get(k) != want {
				t.Errorf("%s = %q, want %q", k, f.Get(k), want)
			}
		}
		_, _ = w.Write([]byte(`{"id_token":"i","access_token":"a","refresh_token":"r"}`))
	})
	tr, err := c.Exchange(context.Background(), "the-code", "ver", "http://r")
	if err != nil {
		t.Fatal(err)
	}
	if tr.IDToken != "i" || tr.AccessToken != "a" || tr.RefreshToken != "r" {
		t.Fatalf("tokens = %+v", tr)
	}
}

func TestRefreshSendsJSON(t *testing.T) {
	c := fakeIssuer(t, func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content type = %s", ct)
		}
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["grant_type"] != "refresh_token" || b["client_id"] != "cid" || b["refresh_token"] != "old" {
			t.Errorf("body = %v", b)
		}
		_, _ = w.Write([]byte(`{"access_token":"new-a","refresh_token":"rotated"}`))
	})
	tr, err := c.Refresh(context.Background(), "old")
	if err != nil {
		t.Fatal(err)
	}
	if tr.AccessToken != "new-a" || tr.RefreshToken != "rotated" {
		t.Fatalf("tokens = %+v", tr)
	}
}

func TestErrorsNeverLeakSecrets(t *testing.T) {
	const secret = "SUPER-SECRET-REFRESH"
	rejected := fakeIssuer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"token ` + secret + ` was already used"}`))
	})
	_, err := rejected.Refresh(context.Background(), secret)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "invalid_grant") || !strings.Contains(err.Error(), "400") {
		t.Fatalf("error = %v", err)
	}

	weird := fakeIssuer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"` + secret + ` with spaces"}`))
	})
	if _, err := weird.Refresh(context.Background(), "x"); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("free-form error code leaked: %v", err)
	}

	// An unreachable endpoint must not echo the URL (the request body is never in it, but keep it tidy).
	dead := &Client{Issuer: "http://127.0.0.1:1", ClientID: "c", HTTP: &http.Client{Timeout: time.Second}}
	if _, err := dead.Refresh(context.Background(), secret); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("transport error = %v", err)
	}
}

func TestNoAccessTokenIsAnError(t *testing.T) {
	c := fakeIssuer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	if _, err := c.Refresh(context.Background(), "x"); err == nil {
		t.Fatal("expected an error for a reply with no access token")
	}
}

func TestReadAuthFileAcceptsUTF8BOM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	body := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"auth_mode":"chatgpt","tokens":{"id_token":"i","access_token":"a","refresh_token":"r"}}`)...)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := ReadAuthFile(path)
	if err != nil || f == nil || f.Tokens == nil || f.Tokens.RefreshToken != "r" {
		t.Fatalf("read with BOM = %+v, %v", f, err)
	}
}
