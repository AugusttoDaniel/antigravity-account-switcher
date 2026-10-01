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
type Connection struct {
	Provider, ID, Email, Name string
	// WorkspaceID is the ChatGPT account id a Codex connection was imported with; empty for one added
	// by AddConnection (matched by email alone, like a legacy row).
	WorkspaceID string
}

// CodexImport is what a Codex import request carried for one entry, kept for assertions.
type CodexImport struct {
	Name, Email, IDToken, AccessToken, RefreshToken, AccountID string
	Overwrite                                                  bool
}

// Server is the fake. Configure it before use; it is safe for concurrent requests.
type Server struct {
	URL string
	// IgnoreAssignments makes assignments succeed without taking effect, to simulate an OmniRoute
	// that accepts a binding but does not apply it (e.g. proxies disabled in its settings).
	IgnoreAssignments bool

	mu           sync.Mutex
	proxies      []Proxy
	conns        []Connection
	assigned     map[string]string // connection id -> proxy id
	codexImports []CodexImport
	requests     []string
	nextID       int
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

	case r.Method == http.MethodPost && r.URL.Path == "/api/providers/codex-auth/import-bulk":
		s.importCodex(w, r)

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

// CodexImports returns the Codex entries imported so far (tokens included, for assertions only).
func (s *Server) CodexImports() []CodexImport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]CodexImport(nil), s.codexImports...)
}

// importCodex mirrors POST /api/providers/codex-auth/import-bulk: each entry needs the three
// tokens, an account already held (same email, here) is refused with 409 unless overwriteExisting,
// and the response never carries tokens.
func (s *Server) importCodex(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Entries []struct {
			JSON  map[string]any `json:"json"`
			Name  string         `json:"name"`
			Email string         `json:"email"`
		} `json:"entries"`
		OverwriteExisting bool `json:"overwriteExisting"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"Invalid JSON body"}`))
		return
	}
	created := []map[string]any{}
	errs := []map[string]any{}
	for i, e := range body.Entries {
		label := e.Name
		if label == "" {
			label = fmt.Sprintf("entry %d", i+1)
		}
		tokens, _ := e.JSON["tokens"].(map[string]any)
		str := func(k string) string { v, _ := tokens[k].(string); return strings.TrimSpace(v) }
		switch {
		case str("id_token") == "":
			errs = append(errs, map[string]any{"index": i, "name": label, "message": "id_token is missing or empty in the auth.json"})
			continue
		case str("access_token") == "":
			errs = append(errs, map[string]any{"index": i, "name": label, "message": "access_token is missing or empty in the auth.json"})
			continue
		case str("refresh_token") == "":
			errs = append(errs, map[string]any{"index": i, "name": label, "message": "refresh_token is missing or empty in the auth.json"})
			continue
		}
		existing := -1
		for j, c := range s.conns {
			// OmniRoute dedups on workspace AND user: two logins sharing an email in different
			// workspaces are distinct.
			if c.Provider == "codex" && strings.EqualFold(c.Email, e.Email) && (c.WorkspaceID == "" || c.WorkspaceID == str("account_id")) {
				existing = j
			}
		}
		if existing >= 0 && !body.OverwriteExisting {
			errs = append(errs, map[string]any{"index": i, "name": label,
				"message": "A Codex connection for this account already exists. Pass overwriteExisting: true to replace it."})
			continue
		}
		var id string
		if existing >= 0 {
			id = s.conns[existing].ID
		} else {
			s.nextID++
			id = fmt.Sprintf("codex-conn-%d", s.nextID)
			s.conns = append(s.conns, Connection{Provider: "codex", ID: id, Email: e.Email, Name: e.Name, WorkspaceID: str("account_id")})
		}
		s.codexImports = append(s.codexImports, CodexImport{
			Name: e.Name, Email: e.Email, IDToken: str("id_token"), AccessToken: str("access_token"),
			RefreshToken: str("refresh_token"), AccountID: str("account_id"), Overwrite: body.OverwriteExisting,
		})
		created = append(created, map[string]any{"id": id, "provider": "codex", "name": e.Name, "email": e.Email})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": len(created), "failed": len(errs), "total": len(body.Entries), "created": created, "errors": errs,
	})
}
