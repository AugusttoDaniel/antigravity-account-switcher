package web

// Read-only comparison between this switcher and an OmniRoute instance: which accounts OmniRoute
// already has, which proxy it effectively uses for each (versus the account's proxy here), which
// OmniRoute accounts this switcher does not know, and which pool proxies OmniRoute's registry holds.
// Nothing is written on either side.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/egress"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/omniroute"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/proxypool"
)

const (
	omnirouteResolveConcurrency = 4
	omnirouteResolveTimeout     = 15 * time.Second
)

// Proxy comparison outcomes for an account present on both sides.
const (
	proxyMatch        = "match"         // same proxy endpoint on both sides
	proxyDiffers      = "differs"       // both use a proxy, but not the same one
	proxyMissingThere = "missing_there" // proxied here, OmniRoute uses it directly or inherits one
	proxyOnlyThere    = "only_there"    // direct here, proxied in OmniRoute
	proxyNone         = "none"          // direct on both sides
	proxyInvalidHere  = "invalid_here"  // the stored proxy here is unusable
	proxyUnknown      = "unknown"       // OmniRoute could not resolve the connection's proxy
)

type accountSync struct {
	AccountID      string `json:"account_id"`
	Email          string `json:"email"`
	InOmniRoute    bool   `json:"in_omniroute"`
	ConnectionID   string `json:"connection_id,omitempty"`
	LocalProxy     string `json:"local_proxy,omitempty"` // masked
	LocalInvalid   bool   `json:"local_proxy_invalid,omitempty"`
	OmniRouteProxy string `json:"omniroute_proxy,omitempty"` // type://host:port, never credentials
	OmniRouteLevel string `json:"omniroute_level,omitempty"` // where OmniRoute's proxy comes from
	ProxyStatus    string `json:"proxy_status,omitempty"`
	Error          string `json:"error,omitempty"`
}

type omnirouteOnlyAccount struct {
	ConnectionID string `json:"connection_id"`
	Email        string `json:"email,omitempty"`
	Name         string `json:"name,omitempty"`
}

type poolSync struct {
	ID          string `json:"id"`
	Proxy       string `json:"proxy,omitempty"` // masked
	InOmniRoute bool   `json:"in_omniroute"`
	// SharedEndpoint marks a host:port OmniRoute holds more than once (e.g. one gateway with
	// several credentials): OmniRoute redacts usernames, so which one is this proxy is unknown.
	SharedEndpoint bool `json:"shared_endpoint,omitempty"`
}

// HandleOmniRoute serves the /api/omniroute routes.
func (a *APIHandler) HandleOmniRoute(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/omniroute/compare" && r.Method == http.MethodPost {
		a.compareWithOmniRoute(w, r)
		return
	}
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

// endpointKey identifies a proxy endpoint as OmniRoute can report it: lowercase host and port.
func endpointKey(host string, port int) string {
	return net.JoinHostPort(strings.ToLower(host), strconv.Itoa(port))
}

func endpointKeyOf(u *url.URL) string {
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		switch u.Scheme {
		case "https":
			port = 443
		case "socks5", "socks5h":
			port = 1080
		default:
			port = 80
		}
	}
	return endpointKey(u.Hostname(), port)
}

func (a *APIHandler) compareWithOmniRoute(w http.ResponseWriter, r *http.Request) {
	var target omnirouteTarget
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&target); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, "invalid request payload", err)
		return
	}
	if err := validateOmniRouteURL(target.URL); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, "invalid OmniRoute URL", err)
		return
	}
	if strings.TrimSpace(target.Token) == "" {
		writeErrorJSON(w, http.StatusBadRequest, "missing OmniRoute token", errors.New("a management token is required"))
		return
	}

	ctx := r.Context()
	accounts, err := a.accountRepo.List(ctx)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to list accounts", err)
		return
	}
	pool, err := a.storedProxyPool()
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to read proxy pool", err)
		return
	}

	client := omnirouteClient(&target)
	listCtx, cancel := context.WithTimeout(ctx, omnirouteCallTimeout)
	defer cancel()
	conns, err := client.ListConnections(listCtx, "agy")
	if err != nil {
		writeErrorJSON(w, http.StatusBadGateway, "could not list OmniRoute accounts", err)
		return
	}
	registry, err := client.ListProxies(listCtx)
	if err != nil {
		writeErrorJSON(w, http.StatusBadGateway, "could not list OmniRoute proxies", err)
		return
	}

	// Accounts: match by email (OmniRoute names imported accounts after their email too).
	byEmail := make(map[string]omniroute.Connection, len(conns))
	for _, c := range conns {
		for _, key := range []string{c.Email, c.Name} {
			if k := strings.ToLower(strings.TrimSpace(key)); k != "" {
				if _, taken := byEmail[k]; !taken {
					byEmail[k] = c
				}
			}
		}
	}
	matched := map[string]bool{}
	rows := make([]accountSync, 0, len(accounts))
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		row := accountSync{AccountID: acc.ID, Email: acc.Email}
		var valid bool
		row.LocalProxy, valid = egress.MaskProxyURL(acc.ProxyURL)
		row.LocalInvalid = !valid
		if c, ok := byEmail[strings.ToLower(strings.TrimSpace(acc.Email))]; ok {
			row.InOmniRoute, row.ConnectionID = true, c.ID
			matched[c.ID] = true
		}
		rows = append(rows, row)
	}

	// For accounts on both sides, ask OmniRoute which proxy it really uses for the connection.
	sem := make(chan struct{}, omnirouteResolveConcurrency)
	var wg sync.WaitGroup
	for i := range rows {
		if !rows[i].InOmniRoute {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(row *accountSync, acc string) {
			defer wg.Done()
			defer func() { <-sem }()
			rctx, rcancel := context.WithTimeout(ctx, omnirouteResolveTimeout)
			defer rcancel()
			resolved, err := client.ResolveConnectionProxy(rctx, row.ConnectionID)
			compareAccountProxy(row, acc, resolved, err)
		}(&rows[i], accountProxy(accounts, rows[i].AccountID))
	}
	wg.Wait()

	onlyThere := []omnirouteOnlyAccount{}
	for _, c := range conns {
		if !matched[c.ID] {
			onlyThere = append(onlyThere, omnirouteOnlyAccount{ConnectionID: c.ID, Email: c.Email, Name: c.Name})
		}
	}

	// Pool: OmniRoute redacts usernames, so match by endpoint and flag shared endpoints.
	endpoints := map[string]int{}
	for _, e := range registry {
		endpoints[endpointKey(e.Host, e.Port)]++
	}
	poolRows := []poolSync{}
	for _, e := range proxypool.Entries(pool, nil) {
		u, err := egress.ParseProxyURL(e.URL)
		if err != nil {
			continue
		}
		n := endpoints[endpointKeyOf(u)]
		poolRows = append(poolRows, poolSync{ID: e.ID, Proxy: maskedProxy(u), InOmniRoute: n > 0, SharedEndpoint: n > 1})
	}

	summary := map[string]int{
		"accounts":              len(rows),
		"omniroute_accounts":    len(conns),
		"only_in_omniroute":     len(onlyThere),
		"pool":                  len(poolRows),
		"omniroute_proxies":     len(registry),
		"accounts_missing":      0,
		"proxy_needs_attention": 0,
		"pool_in_omniroute":     0,
	}
	for _, row := range rows {
		if !row.InOmniRoute {
			summary["accounts_missing"]++
		}
		switch row.ProxyStatus {
		case proxyDiffers, proxyMissingThere, proxyOnlyThere, proxyInvalidHere, proxyUnknown:
			summary["proxy_needs_attention"]++
		}
	}
	for _, p := range poolRows {
		if p.InOmniRoute {
			summary["pool_in_omniroute"]++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":          rows,
		"only_in_omniroute": onlyThere,
		"pool":              poolRows,
		"summary":           summary,
	})
}

func accountProxy(accounts []*domain.Account, id string) string {
	for _, a := range accounts {
		if a != nil && a.ID == id {
			return a.ProxyURL
		}
	}
	return ""
}

// compareAccountProxy fills row's OmniRoute side and the comparison outcome.
func compareAccountProxy(row *accountSync, localProxy string, resolved omniroute.ResolvedProxy, err error) {
	if err != nil {
		row.ProxyStatus, row.Error = proxyUnknown, err.Error()
		return
	}
	row.OmniRouteLevel = resolved.Level
	var remote string
	if resolved.Proxy != nil && resolved.Proxy.Host != "" {
		typ := resolved.Proxy.Type
		if typ == "" {
			typ = "http"
		}
		remote = endpointKey(resolved.Proxy.Host, int(resolved.Proxy.Port))
		row.OmniRouteProxy = typ + "://" + remote
	}

	var local string
	if strings.TrimSpace(localProxy) != "" {
		u, err := egress.ParseProxyURL(localProxy)
		if err != nil {
			row.ProxyStatus = proxyInvalidHere
			return
		}
		local = endpointKeyOf(u)
	}

	switch {
	case local == "" && remote == "":
		row.ProxyStatus = proxyNone
	case local == "":
		row.ProxyStatus = proxyOnlyThere
	case remote == "":
		row.ProxyStatus = proxyMissingThere
	case local == remote:
		row.ProxyStatus = proxyMatch
	default:
		row.ProxyStatus = proxyDiffers
	}
}
