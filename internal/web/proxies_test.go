package web

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/config"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
)

// workingProxy is an HTTP CONNECT proxy that requires user:pass and really forwards the tunnel.
func workingProxy(t *testing.T, user, pass string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				if req.Header.Get("Proxy-Authorization") != want {
					_, _ = c.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n"))
					return
				}
				dst, err := net.Dial("tcp", req.Host)
				if err != nil {
					return
				}
				defer dst.Close()
				_, _ = c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
				go func() { _, _ = io.Copy(dst, br) }()
				_, _ = io.Copy(c, dst)
			}()
		}
	}()
	return ln.Addr().String()
}

// fakeOmniRoute records proxy registry calls.
type fakeOmniRoute struct {
	mu    sync.Mutex
	auth  []string
	hosts []string
}

func (f *fakeOmniRoute) start(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/management/proxies" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.hosts = append(f.hosts, body["host"].(string))
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestProxyPool_CheckImportAssignDelete(t *testing.T) {
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)
	ctx := context.Background()
	now := time.Now().UTC()
	acc := &domain.Account{ID: "acc-1", Email: "user1@gmail.com", Status: domain.AccountStatusActive, IsActive: true, CreatedAt: now, UpdatedAt: now}
	if err := accRepo.Create(ctx, acc); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	// Simulate serve's startup snapshot, taken before any pool existed.
	server.api.SetConfig(config.DefaultConfig())

	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "203.0.113.9")
	}))
	defer echo.Close()
	server.api.proxyCheckURL = echo.URL

	good := workingProxy(t, "alice", "s3cretPW")
	goodHost, goodPort, _ := net.SplitHostPort(good)
	text := strings.Join([]string{
		"# pasted from the provider",
		goodHost + ":" + goodPort + ":alice:s3cretPW", // line 2: works
		"127.0.0.1:1:bob:s3cretPW",                    // line 3: nothing listens -> failed
		"not-a-proxy s3cretPW",                        // line 4: invalid format
		"http://alice:s3cretPW@" + good,               // line 5: same proxy as line 2
		"",
	}, "\n")

	call := func(method, path string, body any) string {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = strings.NewReader(string(b))
		}
		req := httptest.NewRequest(method, path, rd)
		req.Host = "127.0.0.1:8080"
		rr := httptest.NewRecorder()
		server.ServeHTTP(rr, req)
		raw := rr.Body.String()
		for _, secret := range []string{"s3cretPW", "alice", "bob"} {
			if strings.Contains(raw, secret) {
				t.Errorf("%s %s leaks %q: %s", method, path, secret, raw)
			}
		}
		if rr.Code != http.StatusOK {
			t.Fatalf("%s %s: status %d: %s", method, path, rr.Code, raw)
		}
		return raw
	}

	// 1. Check: per-line status, nothing stored yet.
	var check struct {
		Results []proxyCheckResult `json:"results"`
		Summary map[string]int     `json:"summary"`
	}
	_ = json.Unmarshal([]byte(call(http.MethodPost, "/api/proxies/check", map[string]string{"text": text})), &check)
	byLine := map[int]proxyCheckResult{}
	for _, r := range check.Results {
		byLine[r.Line] = r
	}
	if r := byLine[2]; r.Status != "ok" || r.ExitIP != "203.0.113.9" || r.Proxy != "http://***@"+good {
		t.Errorf("line 2: %+v", r)
	}
	if r := byLine[3]; r.Status != "failed" || r.Error == "" {
		t.Errorf("line 3: %+v", r)
	}
	if r := byLine[4]; r.Status != "invalid" || r.Proxy != "" {
		t.Errorf("line 4: %+v", r)
	}
	if r := byLine[5]; r.Status != "duplicate" || !strings.Contains(r.Note, "line 2") {
		t.Errorf("line 5: %+v", r)
	}
	if _, comment := byLine[1]; comment {
		t.Error("comment line was treated as a proxy")
	}
	if pool, _ := config.StoredProxies(); len(pool) != 0 {
		t.Fatalf("check must not store anything, pool = %d entries", len(pool))
	}

	// 2. Import line 2 into the pool and into OmniRoute.
	omni := &fakeOmniRoute{}
	var imp struct {
		Added     int `json:"added"`
		OmniRoute struct {
			OK     int `json:"ok"`
			Failed int `json:"failed"`
		} `json:"omniroute"`
	}
	_ = json.Unmarshal([]byte(call(http.MethodPost, "/api/proxies/import", map[string]any{
		"text":      text,
		"lines":     []int{2},
		"omniroute": map[string]string{"url": omni.start(t), "token": "manage-key", "region": "United Kingdom"},
	})), &imp)
	if imp.Added != 1 || imp.OmniRoute.OK != 1 || imp.OmniRoute.Failed != 0 {
		t.Errorf("import result: %+v", imp)
	}
	if len(omni.hosts) != 1 || omni.hosts[0] != goodHost || omni.auth[0] != "Bearer manage-key" {
		t.Errorf("OmniRoute calls: hosts=%v auth=%v", omni.hosts, omni.auth)
	}

	// A settings change must not write the startup snapshot back over the imported pool.
	call(http.MethodPost, "/api/config", map[string]any{"model_primary": "gemini-2.5-pro", "model_secondary": "gemini-2.5-flash"})
	pool, _ := config.StoredProxies()
	if len(pool) != 1 {
		t.Fatalf("pool after a settings change = %v; the import was overwritten", pool)
	}

	// 3. List shows it masked and free; assign it by ID.
	var list struct {
		Proxies []proxyPoolItem `json:"proxies"`
		Free    int             `json:"free"`
	}
	_ = json.Unmarshal([]byte(call(http.MethodGet, "/api/proxies", nil)), &list)
	if len(list.Proxies) != 1 || list.Free != 1 || list.Proxies[0].Proxy != "http://***@"+good {
		t.Fatalf("list: %+v", list)
	}
	id := list.Proxies[0].ID
	call(http.MethodPut, "/api/accounts/acc-1", map[string]string{"pool_id": id})
	stored, _ := accRepo.GetByID(ctx, "acc-1")
	if stored.ProxyURL != pool[0] {
		t.Errorf("account proxy = %q, want the pool entry", stored.ProxyURL)
	}
	_ = json.Unmarshal([]byte(call(http.MethodGet, "/api/proxies", nil)), &list)
	if list.Proxies[0].UsedBy != "user1@gmail.com" || list.Free != 0 {
		t.Errorf("after assignment: %+v", list)
	}

	// 4. Delete it from the pool.
	call(http.MethodDelete, "/api/proxies/"+id, nil)
	if pool, _ := config.StoredProxies(); len(pool) != 0 {
		t.Errorf("pool after delete = %v", pool)
	}
}

func TestProxyImport_RejectsCleartextOmniRouteURL(t *testing.T) {
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)
	server, err := NewServer(accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"text":      "1.2.3.4:8080:alice:s3cretPW",
		"lines":     []int{1},
		"omniroute": map[string]string{"url": "http://router.example.com", "token": "manage-key"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/proxies/import", strings.NewReader(string(body)))
	req.Host = "127.0.0.1:8080"
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "https") {
		t.Fatalf("expected 400 asking for https, got %d: %s", rr.Code, rr.Body.String())
	}
	if pool, _ := config.StoredProxies(); len(pool) != 0 {
		t.Error("a rejected request must not touch the pool")
	}
}
