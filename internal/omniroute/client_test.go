package omniroute

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBuildAgyTokenJSON(t *testing.T) {
	exp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tok := BuildAgyTokenJSON("acc-tok", "ref-tok", exp)
	if tok["access_token"] != "acc-tok" || tok["refresh_token"] != "ref-tok" {
		t.Fatalf("token fields wrong: %v", tok)
	}
	if tok["token_type"] != "Bearer" {
		t.Errorf("token_type: %v", tok["token_type"])
	}
	if tok["expiry"] != "2026-01-02T03:04:05Z" {
		t.Errorf("expiry: %v", tok["expiry"])
	}

	// Zero expiry must be omitted.
	if _, ok := BuildAgyTokenJSON("a", "b", time.Time{})["expiry"]; ok {
		t.Error("zero expiry should be omitted")
	}
}

func TestProxyConfigFromURL(t *testing.T) {
	cfg, ok, err := ProxyConfigFromURL("http://user:pass@host.example:8080")
	if err != nil || !ok {
		t.Fatalf("unexpected err=%v ok=%v", err, ok)
	}
	if cfg.Type != "http" || cfg.Host != "host.example" || cfg.Port != 8080 || cfg.Username != "user" || cfg.Password != "pass" {
		t.Errorf("parsed wrong: %+v", cfg)
	}

	if _, ok, _ := ProxyConfigFromURL(""); ok {
		t.Error("empty proxy should be ok=false")
	}
	if _, _, err := ProxyConfigFromURL("http://hostonly"); err == nil {
		t.Error("missing port should error")
	}
}

func TestImportBulkAgy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/providers/agy-auth/import-bulk" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok123" {
			t.Errorf("missing bearer auth: %q", r.Header.Get("Authorization"))
		}
		var body struct {
			Entries []AgyEntry `json:"entries"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Entries) != 1 || body.Entries[0].JSON["refresh_token"] != "ref" {
			t.Errorf("entries not forwarded: %+v", body.Entries)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":1,"failed":0,"total":1,"created":[{"id":"conn-1","email":"a@x.com","name":"a@x.com"}],"errors":[]}`))
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL), WithToken("tok123"))
	res, err := c.ImportBulkAgy(context.Background(), []AgyEntry{
		{JSON: BuildAgyTokenJSON("acc", "ref", time.Time{}), Name: "a@x.com", Email: "a@x.com"},
	}, false)
	if err != nil {
		t.Fatalf("ImportBulkAgy: %v", err)
	}
	if res.Success != 1 || len(res.Created) != 1 || res.Created[0].ID != "conn-1" {
		t.Errorf("unexpected result: %+v", res)
	}
}

func TestImportBulkAgyRejectsOverCap(t *testing.T) {
	c := NewClient(WithBaseURL("http://unused"))
	entries := make([]AgyEntry, MaxBulkEntries+1)
	if _, err := c.ImportBulkAgy(context.Background(), entries, false); err == nil {
		t.Error("expected error for >50 entries")
	}
}

func TestAssignConnectionProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/settings/proxy" || r.Method != http.MethodPut {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Level string      `json:"level"`
			ID    string      `json:"id"`
			Proxy ProxyConfig `json:"proxy"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Level != "key" || body.ID != "conn-1" {
			t.Errorf("bad assign body: %+v", body)
		}
		if body.Proxy.Host != "host.example" || body.Proxy.Port != 8080 {
			t.Errorf("bad proxy: %+v", body.Proxy)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL))
	cfg, _, _ := ProxyConfigFromURL("http://user:pass@host.example:8080")
	if err := c.AssignConnectionProxy(context.Background(), "conn-1", cfg); err != nil {
		t.Fatalf("AssignConnectionProxy: %v", err)
	}
}

func TestNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL))
	if _, err := c.ImportBulkAgy(context.Background(), []AgyEntry{{JSON: map[string]any{"access_token": "a", "refresh_token": "b"}}}, false); err == nil {
		t.Error("expected error on 401")
	}
}
