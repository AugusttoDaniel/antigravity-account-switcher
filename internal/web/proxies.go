package web

// Bulk proxy pool management for the dashboard: list the pool, validate a pasted list (format plus
// a live request through each proxy), import it into the pool and optionally into OmniRoute's proxy
// registry, and remove entries. Proxy credentials never leave the backend: the dashboard only sees
// masked URLs and pool IDs, and an import re-sends the original text rather than parsed proxies.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/config"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/egress"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/omniroute"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/proxypool"
)

const (
	// defaultProxyCheckURL answers with the caller's IP as plain text: through a proxy, its exit IP.
	defaultProxyCheckURL  = "https://api.ipify.org"
	maxProxyListBytes     = 1 << 20
	maxProxyLines         = 1000
	proxyCheckConcurrency = 8
	proxyCheckTimeout     = 15 * time.Second
	omnirouteCallTimeout  = 30 * time.Second
)

// proxyLine is one non-blank, non-comment line of a pasted proxy list.
type proxyLine struct {
	Line  int      // 1-based line number in the original text
	Proxy *url.URL // nil when Err is set
	Err   error
}

// maskedProxy is the display form of a proxy the API may return.
func maskedProxy(u *url.URL) string {
	masked, _ := egress.MaskProxyURL(u.String())
	return masked
}

// HandleProxies serves the /api/proxies routes.
func (a *APIHandler) HandleProxies(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/proxies"), "/")
	switch {
	case path == "" && r.Method == http.MethodGet:
		a.listProxies(w, r)
	case path == "check" && r.Method == http.MethodPost:
		a.checkProxies(w, r)
	case path == "import" && r.Method == http.MethodPost:
		a.importProxies(w, r)
	case path != "" && !strings.Contains(path, "/") && r.Method == http.MethodDelete:
		a.deleteProxy(w, r, path)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// storedProxyPool reads the pool under the config lock.
func (a *APIHandler) storedProxyPool() ([]string, error) {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return config.StoredProxies()
}

// updateProxyPool rewrites the stored pool under the config lock and mirrors it into the in-memory
// config, which updateConfig saves wholesale: without the mirror, the next settings change in the
// dashboard would write the stale pool back over the import.
func (a *APIHandler) updateProxyPool(fn func([]string) ([]string, error)) ([]string, error) {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	next, err := config.UpdateProxies(fn)
	if err != nil {
		return nil, err
	}
	if a.appConfig != nil {
		a.appConfig.Proxies = append([]string(nil), next...)
	}
	return next, nil
}

type proxyPoolItem struct {
	ID      string `json:"id"`
	Proxy   string `json:"proxy,omitempty"` // masked
	Invalid bool   `json:"invalid,omitempty"`
	UsedBy  string `json:"used_by,omitempty"`
}

func (a *APIHandler) listProxies(w http.ResponseWriter, r *http.Request) {
	pool, err := a.storedProxyPool()
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to read proxy pool", err)
		return
	}
	accounts, err := a.accountRepo.List(r.Context())
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to list accounts", err)
		return
	}
	items := make([]proxyPoolItem, 0, len(pool))
	free := 0
	for _, e := range proxypool.Entries(pool, accounts) {
		masked, ok := egress.MaskProxyURL(e.URL)
		items = append(items, proxyPoolItem{ID: e.ID, Proxy: masked, Invalid: !ok, UsedBy: e.UsedBy})
		if e.UsedBy == "" && ok {
			free++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"proxies": items, "total": len(items), "free": free})
}

func (a *APIHandler) deleteProxy(w http.ResponseWriter, r *http.Request, id string) {
	found := false
	_, err := a.updateProxyPool(func(pool []string) ([]string, error) {
		next, removed := proxypool.Remove(pool, id)
		found = removed
		return next, nil
	})
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to update proxy pool", err)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

type proxyCheckResult struct {
	Line      int    `json:"line"`
	Proxy     string `json:"proxy,omitempty"` // masked
	Status    string `json:"status"`          // ok | failed | invalid | duplicate
	Error     string `json:"error,omitempty"`
	ExitIP    string `json:"exit_ip,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	InPool    bool   `json:"in_pool,omitempty"`
	Note      string `json:"note,omitempty"`
}

func decodeProxyText(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxProxyListBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, "invalid request payload", err)
		return false
	}
	return true
}

func (a *APIHandler) checkProxies(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if !decodeProxyText(w, r, &req) {
		return
	}
	lines, err := parseProxyList(req.Text)
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, "invalid proxy list", err)
		return
	}
	pool, err := a.storedProxyPool()
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to read proxy pool", err)
		return
	}
	inPool := make(map[string]bool, len(pool))
	for _, p := range pool {
		inPool[proxypool.ID(p)] = true
	}

	results := make([]proxyCheckResult, len(lines))
	firstLine := map[string]int{}
	var toCheck []int
	for i, l := range lines {
		res := &results[i]
		res.Line = l.Line
		if l.Err != nil {
			res.Status, res.Error = "invalid", l.Err.Error()
			continue
		}
		res.Proxy = maskedProxy(l.Proxy)
		id := proxypool.ID(l.Proxy.String())
		res.InPool = inPool[id]
		if prev, dup := firstLine[id]; dup {
			res.Status, res.Note = "duplicate", fmt.Sprintf("same proxy as line %d", prev)
			continue
		}
		firstLine[id] = l.Line
		toCheck = append(toCheck, i)
	}

	checkURL := a.proxyCheckURL
	if checkURL == "" {
		// Lets users point checks at their own IP-echo service instead of the public default.
		checkURL = strings.TrimSpace(os.Getenv("ANTIGRAVITY_PROXY_CHECK_URL"))
	}
	if checkURL == "" {
		checkURL = defaultProxyCheckURL
	}
	sem := make(chan struct{}, proxyCheckConcurrency)
	var wg sync.WaitGroup
	for _, i := range toCheck {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(r.Context(), proxyCheckTimeout)
			defer cancel()
			res := &results[i]
			got, err := egress.Check(ctx, lines[i].Proxy, checkURL)
			if err != nil {
				res.Status, res.Error = "failed", err.Error()
				return
			}
			res.Status, res.ExitIP, res.LatencyMS = "ok", got.ExitIP, got.Latency.Milliseconds()
		}(i)
	}
	wg.Wait()

	// Two proxies leaving from the same IP give their accounts the same identity to Google.
	exitLine := map[string]int{}
	summary := map[string]int{"total": len(results)}
	for i := range results {
		res := &results[i]
		summary[res.Status]++
		if res.Status != "ok" {
			continue
		}
		if prev, seen := exitLine[res.ExitIP]; seen {
			res.Note = fmt.Sprintf("same exit IP as line %d: accounts on both would share an identity", prev)
		} else {
			exitLine[res.ExitIP] = res.Line
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "summary": summary})
}

type omnirouteTarget struct {
	URL    string `json:"url"`
	Token  string `json:"token"`
	Region string `json:"region"`
}

// validateOmniRouteURL requires https, or plain http only to this machine: the management token
// must never cross the network in clear text.
func validateOmniRouteURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return errors.New("not a valid URL (e.g. https://router.example.com)")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if egress.IsLoopbackHost(u.Host) {
			return nil
		}
		return errors.New("use https: plain http would send the management token in clear text")
	default:
		return fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
}

type lineError struct {
	Line  int    `json:"line"`
	Error string `json:"error"`
}

func (a *APIHandler) importProxies(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text      string           `json:"text"`
		Lines     []int            `json:"lines"`
		OmniRoute *omnirouteTarget `json:"omniroute,omitempty"`
	}
	if !decodeProxyText(w, r, &req) {
		return
	}
	if len(req.Lines) == 0 {
		writeErrorJSON(w, http.StatusBadRequest, "nothing to import", errors.New("select at least one line"))
		return
	}
	send := req.OmniRoute != nil && strings.TrimSpace(req.OmniRoute.URL) != ""
	if send {
		if err := validateOmniRouteURL(req.OmniRoute.URL); err != nil {
			writeErrorJSON(w, http.StatusBadRequest, "invalid OmniRoute URL", err)
			return
		}
		if strings.TrimSpace(req.OmniRoute.Token) == "" {
			writeErrorJSON(w, http.StatusBadRequest, "missing OmniRoute token", errors.New("a management token is required"))
			return
		}
	}

	lines, err := parseProxyList(req.Text)
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, "invalid proxy list", err)
		return
	}
	wanted := make(map[int]bool, len(req.Lines))
	for _, n := range req.Lines {
		wanted[n] = true
	}
	var selected []proxyLine
	invalid := []lineError{}
	for _, l := range lines {
		if !wanted[l.Line] {
			continue
		}
		if l.Err != nil {
			invalid = append(invalid, lineError{Line: l.Line, Error: l.Err.Error()})
			continue
		}
		selected = append(selected, l)
	}
	if len(selected) == 0 {
		writeErrorJSON(w, http.StatusBadRequest, "nothing to import", errors.New("none of the selected lines is a valid proxy"))
		return
	}

	urls := make([]string, len(selected))
	for i, l := range selected {
		urls[i] = l.Proxy.String()
	}
	var added int
	if _, err := a.updateProxyPool(func(pool []string) ([]string, error) {
		merged, n := proxypool.Merge(pool, urls)
		added = n
		return merged, nil
	}); err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to update proxy pool", err)
		return
	}

	resp := map[string]any{
		"added":           added,
		"already_in_pool": len(selected) - added,
		"invalid":         invalid,
	}
	if send {
		resp["omniroute"] = a.pushToOmniRoute(r.Context(), req.OmniRoute, selected)
	}
	writeJSON(w, http.StatusOK, resp)
}

// pushToOmniRoute registers each proxy in OmniRoute's registry, reporting failures per line.
// It upserts, so a proxy OmniRoute already holds (same host, port and username) is reported as
// "updated" rather than duplicated.
func (a *APIHandler) pushToOmniRoute(ctx context.Context, target *omnirouteTarget, selected []proxyLine) map[string]any {
	errs := []lineError{}
	var items []omniroute.RegistryProxy
	var itemLines []int
	for _, l := range selected {
		p, err := omniroute.RegistryProxyFromURL(l.Proxy, target.Region)
		if err != nil {
			errs = append(errs, lineError{Line: l.Line, Error: err.Error()})
			continue
		}
		items = append(items, p)
		itemLines = append(itemLines, l.Line)
	}

	created, updated := 0, 0
	if len(items) > 0 {
		callCtx, cancel := context.WithTimeout(ctx, omnirouteCallTimeout)
		results, err := omnirouteClient(target).BulkImportProxies(callCtx, items)
		cancel()
		if err != nil {
			for _, line := range itemLines {
				errs = append(errs, lineError{Line: line, Error: err.Error()})
			}
		}
		for i, r := range results {
			switch {
			case !r.Success:
				errs = append(errs, lineError{Line: itemLines[i], Error: r.Error})
			case r.Action == "updated":
				updated++
			default:
				created++
			}
		}
	}
	return map[string]any{"sent": len(selected), "created": created, "updated": updated, "failed": len(errs), "errors": errs}
}

func omnirouteClient(target *omnirouteTarget) *omniroute.Client {
	return omniroute.NewClient(omniroute.WithBaseURL(target.URL), omniroute.WithToken(target.Token))
}

// resolvePoolProxy returns the pool proxy with the given ID for assignment to an account.
func (a *APIHandler) resolvePoolProxy(id string) (string, error) {
	pool, err := a.storedProxyPool()
	if err != nil {
		return "", err
	}
	proxy, ok := proxypool.Find(pool, id)
	if !ok {
		return "", fmt.Errorf("no proxy with id %q in the pool", id)
	}
	return proxy, nil
}
