package web

// Read-only comparison between this switcher and an OmniRoute instance: which accounts OmniRoute
// already has, which proxy it effectively uses for each (versus the account's proxy here), which
// OmniRoute accounts this switcher does not know, and which pool proxies OmniRoute's registry holds.
// Nothing is written on either side.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	AccountID    string `json:"account_id"`
	Email        string `json:"email"`
	InOmniRoute  bool   `json:"in_omniroute"`
	ConnectionID string `json:"connection_id,omitempty"`
	// OmniRouteConnections counts the connections OmniRoute holds for this email when there is
	// more than one (a duplicate there); the proxy comparison uses the first.
	OmniRouteConnections int    `json:"omniroute_connections,omitempty"`
	LocalProxy           string `json:"local_proxy,omitempty"` // masked
	LocalInvalid         bool   `json:"local_proxy_invalid,omitempty"`
	OmniRouteProxy       string `json:"omniroute_proxy,omitempty"` // type://host:port, never credentials
	OmniRouteLevel       string `json:"omniroute_level,omitempty"` // where OmniRoute's proxy comes from
	ProxyStatus          string `json:"proxy_status,omitempty"`
	Error                string `json:"error,omitempty"`
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
	switch {
	case r.URL.Path == "/api/omniroute/compare" && r.Method == http.MethodPost:
		a.compareWithOmniRoute(w, r)
	case r.URL.Path == "/api/omniroute/bind" && r.Method == http.MethodPost:
		a.bindProxyInOmniRoute(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// listGoogleConnections lists the Google accounts OmniRoute keeps under its two providers: "agy"
// (imported from here) and "antigravity" (its own Antigravity login).
func listGoogleConnections(ctx context.Context, client *omniroute.Client) ([]omniroute.Connection, error) {
	var conns []omniroute.Connection
	for _, provider := range []string{"agy", "antigravity"} {
		list, err := client.ListConnections(ctx, provider)
		if err != nil {
			return nil, err
		}
		conns = append(conns, list...)
	}
	return conns, nil
}

// indexConnectionsByEmail groups connections by lowercase email (or name, which OmniRoute sets to
// the email on import). Every connection of an email is kept, so duplicates stay visible.
func indexConnectionsByEmail(conns []omniroute.Connection) map[string][]omniroute.Connection {
	byEmail := make(map[string][]omniroute.Connection, len(conns))
	for _, c := range conns {
		keys := map[string]bool{}
		for _, key := range []string{c.Email, c.Name} {
			if k := strings.ToLower(strings.TrimSpace(key)); k != "" && !keys[k] {
				keys[k] = true
				byEmail[k] = append(byEmail[k], c)
			}
		}
	}
	return byEmail
}

// bindProxyInOmniRoute makes an account's OmniRoute connection use the proxy the account has here.
// The proxy is read from the stored account and sent server-side, so its credentials never pass
// through the browser. It only writes when the two differ, and never removes a proxy.
func (a *APIHandler) bindProxyInOmniRoute(w http.ResponseWriter, r *http.Request) {
	var req struct {
		omnirouteTarget
		AccountID string `json:"account_id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, "invalid request payload", err)
		return
	}
	if err := validateOmniRouteURL(req.URL); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, "invalid OmniRoute URL", err)
		return
	}
	if strings.TrimSpace(req.Token) == "" {
		writeErrorJSON(w, http.StatusBadRequest, "missing OmniRoute token", errors.New("a management token is required"))
		return
	}

	ctx := r.Context()
	acc, err := a.accountRepo.GetByID(ctx, strings.TrimSpace(req.AccountID))
	if err != nil || acc == nil {
		writeErrorJSON(w, http.StatusNotFound, "account not found", errors.New("no account with that id"))
		return
	}
	if strings.TrimSpace(acc.ProxyURL) == "" {
		writeErrorJSON(w, http.StatusBadRequest, "this account has no proxy here", errors.New("set one first (Edit Proxy); binding never removes a proxy in OmniRoute"))
		return
	}
	proxy, err := egress.ParseProxyURL(acc.ProxyURL)
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, "this account's proxy is invalid", err)
		return
	}

	client := omnirouteClient(&req.omnirouteTarget)
	callCtx, cancel := context.WithTimeout(ctx, omnirouteCallTimeout)
	defer cancel()
	conns, err := listGoogleConnections(callCtx, client)
	if err != nil {
		writeErrorJSON(w, http.StatusBadGateway, "could not list OmniRoute accounts", err)
		return
	}
	matches := indexConnectionsByEmail(conns)[strings.ToLower(strings.TrimSpace(acc.Email))]
	switch len(matches) {
	case 0:
		writeErrorJSON(w, http.StatusNotFound, "account not in OmniRoute", errors.New("send it with export-omniroute first"))
		return
	case 1:
	default:
		writeErrorJSON(w, http.StatusConflict, "OmniRoute holds several connections for this account", fmt.Errorf("%d connections: remove the duplicate first so the proxy goes to the right one", len(matches)))
		return
	}

	res, err := client.BindConnectionProxy(callCtx, matches[0].ID, proxy)
	if err != nil {
		writeErrorJSON(w, http.StatusBadGateway, "could not bind the proxy in OmniRoute", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"changed":       res.Changed,
		"was":           res.Was,
		"connection_id": matches[0].ID,
	})
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
	conns, err := listGoogleConnections(listCtx, client)
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
	byEmail := indexConnectionsByEmail(conns)
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
		if cs := byEmail[strings.ToLower(strings.TrimSpace(acc.Email))]; len(cs) > 0 {
			row.InOmniRoute, row.ConnectionID = true, cs[0].ID
			if len(cs) > 1 {
				row.OmniRouteConnections = len(cs)
			}
			for _, c := range cs {
				matched[c.ID] = true
			}
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
		endpoint := endpointKey(resolved.Proxy.Host, int(resolved.Proxy.Port))
		row.OmniRouteProxy = typ + "://" + endpoint
		// The username is part of the identity: sessions on one gateway endpoint (e.g. Webshare's
		// rotating p.webshare.io:80) differ only by it and leave from different IPs.
		remote = endpoint + "|" + resolved.Proxy.Username
	}

	var local string
	if strings.TrimSpace(localProxy) != "" {
		u, err := egress.ParseProxyURL(localProxy)
		if err != nil {
			row.ProxyStatus = proxyInvalidHere
			return
		}
		local = endpointKeyOf(u) + "|" + u.User.Username()
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
