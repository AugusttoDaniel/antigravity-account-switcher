package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/adspower"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/egress"
)

// Profiles made by sync-proxies-to-aliasmode are named proxy-<host>-<port>, which is how the proxy
// of an existing profile is known: the Local API does not report a profile's proxy.
const profileProxyPrefix = "proxy-"

// endpointFromProfileName parses "proxy-<host>-<port>" into "host:port".
func endpointFromProfileName(name string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(name), profileProxyPrefix)
	if !ok {
		return "", false
	}
	i := strings.LastIndex(rest, "-")
	if i <= 0 || i == len(rest)-1 {
		return "", false
	}
	host, port := rest[:i], rest[i+1:]
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", false
	}
	return net.JoinHostPort(host, port), true
}

// poolProxyByEndpoint finds the pool proxy URL for a host:port.
func poolProxyByEndpoint(pool []string, endpoint string) (string, bool) {
	for _, raw := range pool {
		u, err := egress.ParseProxyURL(strings.TrimSpace(raw))
		if err != nil || u.Port() == "" {
			continue
		}
		if strings.EqualFold(net.JoinHostPort(u.Hostname(), u.Port()), endpoint) {
			return raw, true
		}
	}
	return "", false
}

func listAllProfiles(ctx context.Context, api profileAPI) ([]adspower.Profile, error) {
	const pageSize = 100
	var out []adspower.Profile
	for page := 1; page <= 10; page++ { // up to 1000 profiles: far beyond any realistic pool
		batch, err := api.ListProfiles(ctx, page, pageSize)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < pageSize {
			break
		}
	}
	return out, nil
}

// A browser profile may be shared by accounts of DIFFERENT services (a Google account and an OpenAI
// account see nothing of each other), but never by two accounts of the same one: that is what keeps
// the accounts of a service apart.
const (
	providerGoogle = "google"
	providerCodex  = "codex"
)

// profileOwnersFor maps a profile id to the email of the account of that service onboarded through it.
func (a *APIHandler) profileOwnersFor(ctx context.Context, provider string) map[string]string {
	if provider == providerCodex {
		owners := map[string]string{}
		if a.codexSvc == nil {
			return owners
		}
		accs, err := a.codexSvc.Repo.List(ctx)
		if err != nil {
			return owners
		}
		for _, acc := range accs {
			if acc != nil && acc.AdsPowerProfileID != "" {
				owners[acc.AdsPowerProfileID] = acc.Email
			}
		}
		return owners
	}
	return a.profileOwners(ctx)
}

// profileOwners maps a profile id to the email of the Google account onboarded through it.
func (a *APIHandler) profileOwners(ctx context.Context) map[string]string {
	owners := map[string]string{}
	if a.accountRepo == nil {
		return owners
	}
	accs, err := a.accountRepo.List(ctx)
	if err != nil {
		return owners
	}
	for _, acc := range accs {
		if acc != nil && acc.AdsPowerProfileID != "" {
			owners[acc.AdsPowerProfileID] = acc.Email
		}
	}
	return owners
}

// profileChoice is a browser profile as the add-account dialog offers it. It never carries a
// credential: the profile API does not return one, and the proxy is only an endpoint.
type profileChoice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Proxy is the host:port parsed from the profile's name ("" for a profile not made by the sync).
	Proxy string `json:"proxy,omitempty"`
	// ProxyInPool says the credentials for that endpoint are in the Proxy Pool, so it can be used.
	ProxyInPool bool `json:"proxy_in_pool"`
	// LinkedTo is the email of the Google account already onboarded through the profile, and
	// CodexLinkedTo that of the OpenAI (Codex) one: each service allows one account per profile, but a
	// profile can hold one of each.
	LinkedTo      string `json:"linked_to,omitempty"`
	CodexLinkedTo string `json:"codex_linked_to,omitempty"`
}

// HandleOnboardingProfiles serves GET /api/onboarding/profiles: the existing browser profiles, with
// the account each is linked to and whether its proxy can be used.
func (a *APIHandler) HandleOnboardingProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	conn, _, err := a.connectProfileAPI(r.Context())
	if err != nil {
		writeErrorJSON(w, http.StatusBadGateway, "the browser-profile API is not available", err)
		return
	}
	profiles, err := listAllProfiles(r.Context(), conn.api)
	if err != nil {
		writeErrorJSON(w, http.StatusBadGateway, "could not list the browser profiles", err)
		return
	}
	pool, _ := a.storedProxyPool()
	owners := a.profileOwners(r.Context())
	codexOwners := a.profileOwnersFor(r.Context(), providerCodex)

	out := make([]profileChoice, 0, len(profiles))
	for _, p := range profiles {
		c := profileChoice{ID: p.UserID, Name: p.Name, LinkedTo: owners[p.UserID], CodexLinkedTo: codexOwners[p.UserID]}
		if ep, ok := endpointFromProfileName(p.Name); ok {
			c.Proxy = ep
			_, c.ProxyInPool = poolProxyByEndpoint(pool, ep)
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"profiles": out})
}

// existingProfileProxy is existingProfileProxyFor for a Google account.
func (a *APIHandler) existingProfileProxy(ctx context.Context, id, chosenProxy string) (string, int, error) {
	return a.existingProfileProxyFor(ctx, id, chosenProxy, providerGoogle)
}

// existingProfileProxyFor checks that profile id can be used for a new account of the given service
// and returns the proxy to onboard through. The proxy always comes from the pool, matched to the profile's own endpoint: the
// token exchange must leave from the same IP as the profile's browser, so a different proxy chosen
// alongside is refused. It returns an HTTP status with the error.
func (a *APIHandler) existingProfileProxyFor(ctx context.Context, id, chosenProxy, provider string) (string, int, error) {
	conn, _, err := a.connectProfileAPI(ctx)
	if err != nil {
		return "", http.StatusBadGateway, fmt.Errorf("the browser-profile API is not available: %w", err)
	}
	profiles, err := listAllProfiles(ctx, conn.api)
	if err != nil {
		return "", http.StatusBadGateway, fmt.Errorf("could not list the browser profiles: %w", err)
	}
	var profile *adspower.Profile
	for i := range profiles {
		if profiles[i].UserID == id {
			profile = &profiles[i]
			break
		}
	}
	if profile == nil {
		return "", http.StatusNotFound, fmt.Errorf("no browser profile with id %q", id)
	}
	if owner := a.profileOwnersFor(ctx, provider)[id]; owner != "" {
		return "", http.StatusConflict, fmt.Errorf("the profile %q already belongs to %s: one %s account per profile keeps the accounts of a service apart", profile.Name, owner, provider)
	}
	endpoint, ok := endpointFromProfileName(profile.Name)
	if !ok {
		return "", http.StatusBadRequest, fmt.Errorf("the profile %q does not say which proxy it uses: only profiles named %s<host>-<port> can be reused (create them with sync-proxies-to-aliasmode)", profile.Name, profileProxyPrefix)
	}
	pool, err := a.storedProxyPool()
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	proxyURL, ok := poolProxyByEndpoint(pool, endpoint)
	if !ok {
		return "", http.StatusConflict, fmt.Errorf("the proxy %s of this profile is not in the Proxy Pool: add it there first", endpoint)
	}
	if chosenProxy != "" {
		if u, perr := egress.ParseProxyURL(chosenProxy); perr != nil || !strings.EqualFold(net.JoinHostPort(u.Hostname(), u.Port()), endpoint) {
			return "", http.StatusBadRequest, errors.New("the proxy you chose is not the one this profile uses: the sign-in and the token exchange must leave from the same IP")
		}
	}
	return proxyURL, http.StatusOK, nil
}
