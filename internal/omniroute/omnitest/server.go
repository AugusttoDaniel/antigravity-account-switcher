// Package omnitest is a stateful fake of the OmniRoute management endpoints this project uses, for
// tests. It mirrors the behaviour read from OmniRoute's source: listings redact usernames and
// passwords, the bulk import upserts by host+port+username, an account-scope assignment replaces the
// connection's previous proxy, and resolution returns the real credentials.
package omnitest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Proxy is a registry entry.
type Proxy struct {
	ID, Name, Type, Host, Username, Password, Region string
	Port                                             int
}

// Connection is a provider connection.
type Connection struct{ Provider, ID, Email, Name string }

// Server is the fake. Configure it before use; it is safe for concurrent requests.
type Server struct {
	URL string
	// IgnoreAssignments makes assignments succeed without taking effect, to simulate an OmniRoute
	// that accepts a binding but does not apply it (e.g. proxies disabled in its settings).
	IgnoreAssignments bool

	mu       sync.Mutex
	proxies  []Proxy
	conns    []Connection
	assigned map[string]string // connection id -> proxy id
	requests []string
	nextID   int
}

// New starts the fake and stops it when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	s, stop := Start()
	t.Cleanup(stop)
	return s
}

// Start runs the fake outside a test (e.g. behind a demo dashboard); call the returned func to stop it.
func Start() (*Server, func()) {
	s := &Server{assigned: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = srv.URL
	return s, srv.Close
}

// AddConnection registers a connection under a provider ("agy" or "antigravity").
func (s *Server) AddConnection(provider, id, email string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conns = append(s.conns, Connection{Provider: provider, ID: id, Email: email, Name: email})
}

// AddProxy registers a proxy entry and returns its id.
func (s *Server) AddProxy(name, host string, port int, username, password string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addProxyLocked(Proxy{Name: name, Type: "http", Host: host, Port: port, Username: username, Password: password})
}

func (s *Server) addProxyLocked(p Proxy) string {
	p.ID = fmt.Sprintf("px-%d", s.nextID)
	s.nextID++
	s.proxies = append(s.proxies, p)
	return p.ID
}

// Bind assigns a registry proxy to a connection directly, as if done in OmniRoute's own UI.
func (s *Server) Bind(connectionID, proxyID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.assigned[connectionID] = proxyID
}

// Proxies returns a copy of the registry.
func (s *Server) Proxies() []Proxy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Proxy(nil), s.proxies...)
}

// BoundProxy returns the registry entry assigned to a connection, if any.
func (s *Server) BoundProxy(connectionID string) (Proxy, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.assigned[connectionID]
	if !ok {
		return Proxy{}, false
	}
	for _, p := range s.proxies {
		if p.ID == id {
			return p, true
		}
	}
	return Proxy{}, false
}

// Requests returns the "METHOD request-uri" log of every request received.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// Writes counts the requests that change state (anything but GET).
func (s *Server) Writes() int {
	n := 0
	for _, r := range s.Requests() {
		if !strings.HasPrefix(r, "GET ") {
			n++
		}
	}
	return n
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Method+" "+r.URL.RequestURI())
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/providers":
		provider := r.URL.Query().Get("provider")
		conns := []map[string]any{}
		for _, c := range s.conns {
			if c.Provider == provider {
				conns = append(conns, map[string]any{"id": c.ID, "provider": c.Provider, "email": c.Email, "name": c.Name})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"connections": conns, "total": len(conns)})

	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/management/proxies":
		items := []map[string]any{}
		for _, p := range s.proxies {
			items = append(items, map[string]any{"id": p.ID, "name": p.Name, "type": p.Type, "host": p.Host, "port": p.Port, "username": "***", "password": "***", "status": "active"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "page": map[string]any{"total": len(items)}})

	case r.Method == http.MethodPost && r.URL.Path == "/api/settings/proxies/bulk-import":
		var body struct {
			Items []struct {
				Name, Type, Host, Username, Password, Region string
				Port                                         int
			} `json:"items"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		results := []map[string]any{}
		for _, it := range body.Items {
			action := "created"
			idx := -1
			for i, p := range s.proxies {
				if p.Host == it.Host && p.Port == it.Port && p.Username == it.Username {
					idx = i
				}
			}
			if idx >= 0 { // upsert: the name, type and password follow the request
				action = "updated"
				s.proxies[idx].Name, s.proxies[idx].Type, s.proxies[idx].Password = it.Name, it.Type, it.Password
				if it.Region != "" {
					s.proxies[idx].Region = it.Region
				}
			} else {
				s.addProxyLocked(Proxy{Name: it.Name, Type: it.Type, Host: it.Host, Port: it.Port, Username: it.Username, Password: it.Password, Region: it.Region})
				idx = len(s.proxies) - 1
			}
			results = append(results, map[string]any{"name": it.Name, "success": true, "action": action, "id": s.proxies[idx].ID})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})

	case r.Method == http.MethodPut && r.URL.Path == "/api/v1/management/proxies/assignments":
		var body struct{ Scope, ScopeID, ProxyID string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Scope != "account" || body.ScopeID == "" || body.ProxyID == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"bad assignment"}}`))
			return
		}
		if !s.IgnoreAssignments {
			s.assigned[body.ScopeID] = body.ProxyID
		}
		_, _ = w.Write([]byte(`{"success":true}`))

	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/management/proxies/assignments":
		id := r.URL.Query().Get("resolve_connection_id")
		if id == "" { // the plain listing: which proxy is bound to which connection
			items := []map[string]any{}
			for connID, proxyID := range s.assigned {
				items = append(items, map[string]any{"proxyId": proxyID, "scope": "account", "scopeId": connID})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "page": map[string]any{"total": len(items)}})
			return
		}
		for _, p := range s.proxies {
			if p.ID == s.assigned[id] {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"proxy": map[string]any{"type": p.Type, "host": p.Host, "port": strconv.Itoa(p.Port), "username": p.Username, "password": p.Password},
					"level": "account", "levelId": id,
				})
				return
			}
		}
		_, _ = w.Write([]byte(`{"proxy":null,"level":"direct","levelId":null}`))

	default:
		http.NotFound(w, r)
	}
}
