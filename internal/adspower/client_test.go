package adspower

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	// Disable the throttle so tests do not sleep.
	return NewClient(WithBaseURL(srv.URL), WithMinInterval(0))
}

func writeEnvelope(t *testing.T, w http.ResponseWriter, code int, msg string, data any) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	env := map[string]any{"code": code, "msg": msg, "data": json.RawMessage(raw)}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(env)
}

func TestProxyConfigURLRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty is direct", "", ""},
		{"http with auth", "http://user:pass@host.example:8080", "http://user:pass@host.example:8080"},
		{"socks5 no auth", "socks5://1.2.3.4:1080", "socks5://1.2.3.4:1080"},
		{"user only", "http://user@host.example:3128", "http://user@host.example:3128"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc, err := ProxyConfigFromURL(tc.raw)
			if err != nil {
				t.Fatalf("ProxyConfigFromURL(%q): %v", tc.raw, err)
			}
			if got := pc.URL(); got != tc.want {
				t.Errorf("round trip %q: got %q want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestProxyConfigFromURLRejectsHostless(t *testing.T) {
	if _, err := ProxyConfigFromURL("http://"); err == nil {
		t.Error("expected error for proxy url without host")
	}
}

func TestListProfilesParsesProxy(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/user/list" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		writeEnvelope(t, w, 0, "success", map[string]any{
			"list": []map[string]any{
				{
					"user_id": "prof-1",
					"name":    "acct-a",
					"user_proxy_config": map[string]any{
						"proxy_soft":     "other",
						"proxy_type":     "http",
						"proxy_host":     "p.example",
						"proxy_port":     "8080",
						"proxy_user":     "u",
						"proxy_password": "pw",
					},
				},
			},
		})
	})

	profiles, err := c.ListProfiles(context.Background(), 1, 50)
	if err != nil {
		t.Fatalf("ListProfiles: %v", err)
	}
	if len(profiles) != 1 {
		t.Fatalf("expected 1 profile, got %d", len(profiles))
	}
	if got := profiles[0].ProxyConfig.URL(); got != "http://u:pw@p.example:8080" {
		t.Errorf("proxy url: got %q", got)
	}
}

func TestCreateProfileSendsProxyAndReturnsID(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/user/create" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body CreateProfileRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.ProxyConfig.ProxyHost != "p.example" || body.ProxyConfig.ProxyPort != "9000" {
			t.Errorf("proxy not forwarded: %+v", body.ProxyConfig)
		}
		if body.GroupID == "" {
			t.Error("group_id must default, got empty")
		}
		writeEnvelope(t, w, 0, "success", map[string]any{"id": "new-prof"})
	})

	pc, err := ProxyConfigFromURL("http://p.example:9000")
	if err != nil {
		t.Fatalf("ProxyConfigFromURL: %v", err)
	}
	id, err := c.CreateProfile(context.Background(), CreateProfileRequest{Name: "x", ProxyConfig: pc})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if id != "new-prof" {
		t.Errorf("id: got %q", id)
	}
}

func TestStartBrowserReturnsCDPEndpoint(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("headless") != "0" {
			t.Errorf("assisted login must be visible (headless=0), got %q", r.URL.Query().Get("headless"))
		}
		writeEnvelope(t, w, 0, "success", map[string]any{
			"ws":         map[string]any{"puppeteer": "ws://127.0.0.1:1234/devtools/browser/abc", "selenium": "127.0.0.1:1234"},
			"debug_port": "1234",
		})
	})

	data, err := c.StartBrowser(context.Background(), "prof-1", false)
	if err != nil {
		t.Fatalf("StartBrowser: %v", err)
	}
	if data.WS.Puppeteer == "" {
		t.Error("expected puppeteer CDP endpoint")
	}
}

func TestNonZeroCodeIsAPIError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(t, w, 500, "profile busy", nil)
	})
	_, err := c.StartBrowser(context.Background(), "prof-1", false)
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T (%v)", err, err)
	}
	if apiErr.Code != 500 {
		t.Errorf("code: got %d", apiErr.Code)
	}
}

func TestThrottleSerializesCalls(t *testing.T) {
	var callTimes []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callTimes = append(callTimes, time.Now())
		writeEnvelope(t, w, 0, "success", map[string]any{"status": "Active"})
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithMinInterval(60*time.Millisecond))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := c.BrowserActive(ctx, "prof-1"); err != nil {
			t.Fatalf("BrowserActive: %v", err)
		}
	}
	if len(callTimes) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(callTimes))
	}
	for i := 1; i < len(callTimes); i++ {
		if gap := callTimes[i].Sub(callTimes[i-1]); gap < 50*time.Millisecond {
			t.Errorf("calls %d/%d too close: %v (throttle not applied)", i-1, i, gap)
		}
	}
}
