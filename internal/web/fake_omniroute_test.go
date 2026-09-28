package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

// fakeOmniRoute mimics the OmniRoute endpoints the switcher uses, including their redaction:
// a registry upserted by host+port+username, agy connections, and per-connection proxy resolution.
type fakeOmniRoute struct {
	mu       sync.Mutex
	auth     []string
	registry []fakeRegistryProxy
	conns    []map[string]any
	resolved map[string]string // connection id -> raw resolution JSON
}

type fakeRegistryProxy struct {
	ID, Type, Host, Username, Password string
	Port                               int
}

func (f *fakeOmniRoute) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/settings/proxies/bulk-import":
			var body struct {
				Items []struct {
					Name, Type, Host, Username, Password string
					Port                                 int
				} `json:"items"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			results := []map[string]any{}
			for _, it := range body.Items {
				action := "created"
				idx := -1
				for i, p := range f.registry {
					if p.Host == it.Host && p.Port == it.Port && p.Username == it.Username {
						idx = i
					}
				}
				if idx >= 0 {
					action = "updated"
					f.registry[idx].Password = it.Password
				} else {
					idx = len(f.registry)
					f.registry = append(f.registry, fakeRegistryProxy{ID: fmt.Sprintf("px-%d", idx), Type: it.Type, Host: it.Host, Port: it.Port, Username: it.Username, Password: it.Password})
				}
				results = append(results, map[string]any{"name": it.Name, "success": true, "action": action, "id": f.registry[idx].ID})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/management/proxies/assignments":
			id := r.URL.Query().Get("resolve_connection_id")
			raw, ok := f.resolved[id]
			if !ok {
				raw = `{"proxy":null,"level":"direct","levelId":null}`
			}
			_, _ = w.Write([]byte(raw))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/management/proxies":
			items := []map[string]any{}
			for _, p := range f.registry {
				// OmniRoute redacts both credentials in listings.
				items = append(items, map[string]any{"id": p.ID, "type": p.Type, "host": p.Host, "port": p.Port, "username": "***", "password": "***"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "page": map[string]any{"total": len(items)}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/providers":
			_ = json.NewEncoder(w).Encode(map[string]any{"connections": f.conns, "total": len(f.conns)})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (f *fakeOmniRoute) registryHosts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for _, p := range f.registry {
		out = append(out, p.Host+":"+strconv.Itoa(p.Port))
	}
	return out
}
