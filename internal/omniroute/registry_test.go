package omniroute

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestRegistryProxyFromURL(t *testing.T) {
	u, _ := url.Parse("socks5h://alice:s3cret@10.0.0.1:1080")
	p, err := RegistryProxyFromURL(u, " United Kingdom ")
	if err != nil {
		t.Fatal(err)
	}
	want := RegistryProxy{Name: "ws-10.0.0.1", Type: "socks5", Host: "10.0.0.1", Port: 1080, Username: "alice", Password: "s3cret", Region: "United Kingdom"}
	if p != want {
		t.Errorf("got %+v, want %+v", p, want)
	}

	noPort, _ := url.Parse("http://proxy.example.com")
	if _, err := RegistryProxyFromURL(noPort, ""); err == nil {
		t.Error("a proxy without an explicit port must be rejected")
	}
}

// The bulk endpoint upserts, so results say which proxies already existed. More than 100 proxies
// are split into several requests and the results keep the input order.
func TestBulkImportProxies_UpsertsInChunks(t *testing.T) {
	var batches []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/settings/proxies/bulk-import" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer manage-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body struct {
			Items []RegistryProxy `json:"items"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		batches = append(batches, len(body.Items))
		results := make([]map[string]any, len(body.Items))
		for i, it := range body.Items {
			action := "created"
			if it.Port%2 == 0 {
				action = "updated" // pretend even ports already existed
			}
			results[i] = map[string]any{"name": it.Name, "success": true, "action": action, "id": fmt.Sprintf("id-%d", it.Port)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	defer srv.Close()

	proxies := make([]RegistryProxy, 150)
	for i := range proxies {
		proxies[i] = RegistryProxy{Name: "p", Type: "http", Host: "1.2.3.4", Port: 1000 + i, Username: "u", Password: "pw"}
	}
	c := NewClient(WithBaseURL(srv.URL), WithToken("manage-key"))
	res, err := c.BulkImportProxies(context.Background(), proxies)
	if err != nil {
		t.Fatalf("BulkImportProxies: %v", err)
	}
	if len(batches) != 2 || batches[0] != 100 || batches[1] != 50 {
		t.Errorf("batches = %v, want [100 50]", batches)
	}
	if len(res) != 150 || res[0].Action != "updated" || res[1].Action != "created" || res[149].ID != "id-1149" {
		t.Errorf("results out of order or wrong: first=%+v second=%+v last=%+v", res[0], res[1], res[149])
	}
}

// OmniRoute's error body may echo the request; passwords must not reach the caller.
func TestBulkImportProxies_ErrorsNeverEchoPasswords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL), WithToken("manage-key"))
	_, err := c.BulkImportProxies(context.Background(), []RegistryProxy{{Name: "x", Type: "http", Host: "1.2.3.4", Port: 8080, Username: "alice", Password: "s3cretPW"}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "s3cretPW") {
		t.Errorf("error leaks the password: %v", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should still carry the status: %v", err)
	}
}

func TestListProxiesAndConnections_Paginate(t *testing.T) {
	const totalProxies, totalConns = 250, 3
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		switch r.URL.Path {
		case "/api/v1/management/proxies":
			items := []map[string]any{}
			for i := offset; i < totalProxies && i < offset+limit; i++ {
				items = append(items, map[string]any{"id": fmt.Sprintf("px-%d", i), "host": "10.0.0.1", "port": 2000 + i, "username": "***", "password": "***"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "page": map[string]any{"limit": limit, "offset": offset, "total": totalProxies}})
		case "/api/providers":
			if r.URL.Query().Get("provider") != "agy" {
				t.Errorf("provider filter = %q", r.URL.Query().Get("provider"))
			}
			conns := []map[string]any{}
			for i := offset; i < totalConns && i < offset+limit; i++ {
				conns = append(conns, map[string]any{"id": fmt.Sprintf("c-%d", i), "provider": "agy", "email": fmt.Sprintf("user%d@gmail.com", i)})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"connections": conns, "total": totalConns})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL))
	proxies, err := c.ListProxies(context.Background())
	if err != nil || len(proxies) != totalProxies || proxies[249].Port != 2249 {
		t.Fatalf("ListProxies: %d entries (err %v)", len(proxies), err)
	}
	conns, err := c.ListConnections(context.Background(), "agy")
	if err != nil || len(conns) != totalConns || conns[2].Email != "user2@gmail.com" {
		t.Fatalf("ListConnections: %+v (err %v)", conns, err)
	}
}

func TestResolveConnectionProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("resolve_connection_id") {
		case "bound":
			// Legacy settings may store the port as a string; the password must simply be ignored.
			_, _ = io.WriteString(w, `{"proxy":{"type":"http","host":"1.2.3.4","port":"8080","username":"alice","password":"s3cret"},"level":"key","levelId":"bound"}`)
		case "direct":
			_, _ = io.WriteString(w, `{"proxy":null,"level":"direct","levelId":null}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL))
	got, err := c.ResolveConnectionProxy(context.Background(), "bound")
	if err != nil || got.Level != "key" || got.Proxy == nil || got.Proxy.Host != "1.2.3.4" || got.Proxy.Port != 8080 {
		t.Fatalf("bound: %+v (err %v)", got, err)
	}
	got, err = c.ResolveConnectionProxy(context.Background(), "direct")
	if err != nil || got.Level != "direct" || got.Proxy != nil {
		t.Fatalf("direct: %+v (err %v)", got, err)
	}
}
