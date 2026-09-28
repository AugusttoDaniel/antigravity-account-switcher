package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/config"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
)

func TestCompareWithOmniRoute(t *testing.T) {
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)
	ctx := context.Background()
	now := time.Now().UTC()
	local := []*domain.Account{
		{ID: "acc-match", Email: "a@gmail.com", ProxyURL: "http://u:s3cretPW@10.0.0.1:8080"},
		{ID: "acc-diff", Email: "b@gmail.com", ProxyURL: "http://u:s3cretPW@10.0.0.2:8080"},
		{ID: "acc-missing-there", Email: "c@gmail.com", ProxyURL: "http://u:s3cretPW@10.0.0.3:8080"},
		{ID: "acc-only-there", Email: "d@gmail.com"},
		{ID: "acc-none", Email: "e@gmail.com"},
		{ID: "acc-absent", Email: "f@gmail.com"},
		{ID: "acc-invalid", Email: "g@gmail.com", ProxyURL: "1.2.3.4:8080:u:s3cretPW"}, // legacy value, absent there
	}
	for _, a := range local {
		a.Status, a.CreatedAt, a.UpdatedAt = domain.AccountStatusActive, now, now
		if err := accRepo.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := config.UpdateProxies(func([]string) ([]string, error) {
		return []string{
			"http://u:s3cretPW@10.0.0.1:8080",  // in OmniRoute once
			"http://u2:s3cretPW@10.0.0.5:9000", // endpoint held twice there
			"socks5://10.0.0.6:1080",           // not in OmniRoute
		}, nil
	}); err != nil {
		t.Fatal(err)
	}

	omni := &fakeOmniRoute{
		conns: []map[string]any{
			{"id": "c-a", "provider": "agy", "email": "A@Gmail.com"}, // case differs
			{"id": "c-b", "provider": "agy", "email": "b@gmail.com"},
			{"id": "c-c", "provider": "agy", "email": "c@gmail.com"},
			{"id": "c-d", "provider": "agy", "email": "d@gmail.com"},
			{"id": "c-e", "provider": "agy", "email": "e@gmail.com"},
			{"id": "c-x", "provider": "agy", "email": "x@gmail.com"}, // unknown here
		},
		resolved: map[string]string{
			"c-a": `{"proxy":{"type":"http","host":"10.0.0.1","port":8080,"username":"u","password":"s3cretPW"},"level":"account"}`,
			"c-b": `{"proxy":{"type":"http","host":"10.0.0.9","port":"3128","password":"s3cretPW"},"level":"key"}`,
			"c-d": `{"proxy":{"type":"socks5","host":"10.0.0.4","port":1080},"level":"global"}`,
		},
		registry: []fakeRegistryProxy{
			{ID: "p1", Type: "http", Host: "10.0.0.1", Port: 8080, Username: "u"},
			{ID: "p2", Type: "http", Host: "10.0.0.5", Port: 9000, Username: "u1"},
			{ID: "p3", Type: "http", Host: "10.0.0.5", Port: 9000, Username: "u2"},
		},
	}
	omniURL := omni.start(t)

	server, err := NewServer(accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"url": omniURL, "token": "manage-key"})
	req := httptest.NewRequest(http.MethodPost, "/api/omniroute/compare", strings.NewReader(string(body)))
	req.Host = "127.0.0.1:8080"
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "s3cretPW") {
		t.Fatalf("comparison leaks a password: %s", rr.Body.String())
	}

	var got struct {
		Accounts        []accountSync          `json:"accounts"`
		OnlyInOmniRoute []omnirouteOnlyAccount `json:"only_in_omniroute"`
		Pool            []poolSync             `json:"pool"`
		Summary         map[string]int         `json:"summary"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	byID := map[string]accountSync{}
	for _, a := range got.Accounts {
		byID[a.AccountID] = a
	}

	want := map[string]struct {
		in     bool
		status string
	}{
		"acc-match":         {true, proxyMatch},
		"acc-diff":          {true, proxyDiffers},
		"acc-missing-there": {true, proxyMissingThere},
		"acc-only-there":    {true, proxyOnlyThere},
		"acc-none":          {true, proxyNone},
		"acc-absent":        {false, ""},
	}
	for id, w := range want {
		a := byID[id]
		if a.InOmniRoute != w.in || a.ProxyStatus != w.status {
			t.Errorf("%s: in_omniroute=%v status=%q, want %v %q (%+v)", id, a.InOmniRoute, a.ProxyStatus, w.in, w.status, a)
		}
	}
	if a := byID["acc-diff"]; a.OmniRouteProxy != "http://10.0.0.9:3128" || a.OmniRouteLevel != "key" || a.LocalProxy != "http://***@10.0.0.2:8080" {
		t.Errorf("acc-diff sides: %+v", a)
	}
	if a := byID["acc-match"]; a.LocalInvalid {
		t.Errorf("a valid local proxy was flagged invalid: %+v", a)
	}
	// Even for an account OmniRoute lacks, an unusable proxy must read as invalid, not "direct".
	if a := byID["acc-invalid"]; !a.LocalInvalid || a.LocalProxy != "" || a.InOmniRoute {
		t.Errorf("acc-invalid: %+v", a)
	}
	if a := byID["acc-only-there"]; a.OmniRouteLevel != "global" {
		t.Errorf("inherited proxy level not reported: %+v", a)
	}

	if len(got.OnlyInOmniRoute) != 1 || got.OnlyInOmniRoute[0].Email != "x@gmail.com" {
		t.Errorf("only_in_omniroute = %+v", got.OnlyInOmniRoute)
	}

	pool := map[string]poolSync{}
	for _, p := range got.Pool {
		pool[p.Proxy] = p
	}
	if p := pool["http://***@10.0.0.1:8080"]; !p.InOmniRoute || p.SharedEndpoint {
		t.Errorf("pool 10.0.0.1: %+v", p)
	}
	if p := pool["http://***@10.0.0.5:9000"]; !p.InOmniRoute || !p.SharedEndpoint {
		t.Errorf("pool 10.0.0.5 (held twice there): %+v", p)
	}
	if p := pool["socks5://10.0.0.6:1080"]; p.InOmniRoute {
		t.Errorf("pool 10.0.0.6: %+v", p)
	}

	s := got.Summary
	if s["accounts_missing"] != 2 || s["only_in_omniroute"] != 1 || s["proxy_needs_attention"] != 3 || s["pool_in_omniroute"] != 2 {
		t.Errorf("summary = %v", s)
	}
}

func TestCompareWithOmniRoute_ReportsUnreachableOmniRoute(t *testing.T) {
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)
	server, err := NewServer(accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"url": "http://127.0.0.1:1", "token": "manage-key"})
	req := httptest.NewRequest(http.MethodPost, "/api/omniroute/compare", strings.NewReader(string(body)))
	req.Host = "127.0.0.1:8080"
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for an unreachable OmniRoute, got %d: %s", rr.Code, rr.Body.String())
	}
}
