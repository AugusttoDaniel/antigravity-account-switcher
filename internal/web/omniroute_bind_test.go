package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/omniroute/omnitest"
)

func TestBindProxyInOmniRoute(t *testing.T) {
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)
	ctx := context.Background()
	now := time.Now().UTC()
	local := []*domain.Account{
		{ID: "acc-ok", Email: "ok@gmail.com", ProxyURL: "http://alice:s3cretPW@1.2.3.4:8080"},
		{ID: "acc-direct", Email: "direct@gmail.com"},
		{ID: "acc-invalid", Email: "invalid@gmail.com", ProxyURL: "1.2.3.4:8080:bob:s3cretPW"},
		{ID: "acc-absent", Email: "absent@gmail.com", ProxyURL: "http://alice:s3cretPW@1.2.3.4:8080"},
		{ID: "acc-dup", Email: "dup@gmail.com", ProxyURL: "http://alice:s3cretPW@1.2.3.4:8080"},
		{ID: "acc-antigravity", Email: "own-login@gmail.com", ProxyURL: "http://alice:s3cretPW@1.2.3.4:8080"},
	}
	for _, a := range local {
		a.Status, a.CreatedAt, a.UpdatedAt = domain.AccountStatusActive, now, now
		if err := accRepo.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
	}

	fake := omnitest.New(t)
	fake.AddConnection("agy", "c-ok", "ok@gmail.com")
	fake.AddConnection("agy", "c-direct", "direct@gmail.com")
	fake.AddConnection("agy", "c-invalid", "invalid@gmail.com")
	fake.AddConnection("agy", "c-dup-1", "dup@gmail.com")
	fake.AddConnection("agy", "c-dup-2", "dup@gmail.com")
	fake.AddConnection("antigravity", "c-own", "own-login@gmail.com")

	server, err := NewServer(accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, nil)
	if err != nil {
		t.Fatal(err)
	}
	bind := func(accountID string) (int, map[string]any, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"url": fake.URL, "token": "manage-key", "account_id": accountID})
		req := httptest.NewRequest(http.MethodPost, "/api/omniroute/bind", strings.NewReader(string(body)))
		req.Host = "127.0.0.1:8080"
		rr := httptest.NewRecorder()
		server.ServeHTTP(rr, req)
		raw := rr.Body.String()
		for _, secret := range []string{"s3cretPW", "alice", "bob"} {
			if strings.Contains(raw, secret) {
				t.Errorf("bind %s leaks %q: %s", accountID, secret, raw)
			}
		}
		var out map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return rr.Code, out, raw
	}

	// 1. Binds the account's proxy to its connection; the credentials come from the stored account.
	code, out, raw := bind("acc-ok")
	if code != http.StatusOK || out["changed"] != true || out["was"] != "direct" || out["connection_id"] != "c-ok" {
		t.Fatalf("first bind: %d %s", code, raw)
	}
	if p, ok := fake.BoundProxy("c-ok"); !ok || p.Host != "1.2.3.4" || p.Username != "alice" || p.Password != "s3cretPW" {
		t.Errorf("connection bound to %+v (%v)", p, ok)
	}

	// 2. Again: already in effect, nothing written.
	writes := fake.Writes()
	code, out, raw = bind("acc-ok")
	if code != http.StatusOK || out["changed"] != false {
		t.Fatalf("second bind: %d %s", code, raw)
	}
	if fake.Writes() != writes {
		t.Errorf("second bind wrote to OmniRoute")
	}

	// 3. Also works for a connection that lives under OmniRoute's own Antigravity login.
	if code, out, raw = bind("acc-antigravity"); code != http.StatusOK || out["connection_id"] != "c-own" {
		t.Errorf("antigravity-provider account: %d %s", code, raw)
	}

	// 4. Refusals, none of which may write to OmniRoute.
	writes = fake.Writes()
	refusals := map[string]int{
		"acc-direct":  http.StatusBadRequest, // nothing to bind: never removes a proxy
		"acc-invalid": http.StatusBadRequest, // unusable local value
		"acc-absent":  http.StatusNotFound,   // not in OmniRoute
		"acc-dup":     http.StatusConflict,   // two connections: which one?
		"no-such-id":  http.StatusNotFound,
	}
	for id, want := range refusals {
		if code, _, raw := bind(id); code != want {
			t.Errorf("bind %s: status %d, want %d (%s)", id, code, want, raw)
		}
	}
	if fake.Writes() != writes {
		t.Errorf("a refused bind wrote to OmniRoute")
	}
}

func TestBindProxyInOmniRoute_RejectsBadTargets(t *testing.T) {
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)
	server, err := NewServer(accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]map[string]string{
		"cleartext http": {"url": "http://router.example.com", "token": "k", "account_id": "x"},
		"path":           {"url": "https://router.example.com/v1", "token": "k", "account_id": "x"},
		"no token":       {"url": "https://router.example.com", "token": "", "account_id": "x"},
	} {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/omniroute/bind", strings.NewReader(string(b)))
		req.Host = "127.0.0.1:8080"
		rr := httptest.NewRecorder()
		server.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%s)", name, rr.Code, rr.Body.String())
		}
	}
}
