// Package aliassync turns the proxies in an OmniRoute registry into browser profiles in AliasMode (or
// any ADS Power-compatible profile API): one empty profile per proxy, already bound to it, ready to be
// picked when an account is onboarded.
//
// The Local API has no proxy-registry endpoint, so a profile is the only way a proxy can be stored
// there through the API. And OmniRoute never lists a proxy's credentials: the only way to read one is
// to ask which proxy a connection uses, which works for proxies assigned to a connection. The others
// are matched, by host and port, against a pool of proxy URLs held locally.
package aliassync

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/adspower"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/egress"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/omniroute"
)

// Source is the part of the OmniRoute client Collect uses; *omniroute.Client implements it.
type Source interface {
	ListProxies(ctx context.Context) ([]omniroute.RegistryEntry, error)
	ListProxyAssignments(ctx context.Context) ([]omniroute.ProxyAssignment, error)
	ResolveConnectionProxy(ctx context.Context, connectionID string) (omniroute.ResolvedProxy, error)
}

// Candidate is a proxy with the credentials needed to build a profile. URL carries the password:
// never print or log it.
type Candidate struct {
	// Name is the proxy's name in the OmniRoute registry.
	Name     string
	HostPort string
	URL      string
	// Source is where the credentials came from: "omniroute" or "pool".
	Source string
}

// Missing is a registry proxy no credentials could be found for.
type Missing struct {
	Name     string
	HostPort string
	Reason   string
}

// ProfileName is the name given to a proxy's profile.
func ProfileName(hostPort string) string {
	return "proxy-" + strings.NewReplacer(":", "-", "[", "", "]", "").Replace(hostPort)
}

// Collect lists the registry and finds credentials for each proxy: from OmniRoute when it is
// assigned to a connection, otherwise from pool (proxy URLs matched by host and port).
func Collect(ctx context.Context, src Source, pool []string) ([]Candidate, []Missing, error) {
	registry, err := src.ListProxies(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list the OmniRoute proxies: %w", err)
	}
	assignments, err := src.ListProxyAssignments(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list the OmniRoute proxy assignments: %w", err)
	}
	connsOf := map[string][]string{}
	for _, a := range assignments {
		if a.Scope == "account" && a.ProxyID != "" && a.ScopeID != "" {
			connsOf[a.ProxyID] = append(connsOf[a.ProxyID], a.ScopeID)
		}
	}
	poolIdx := indexPool(pool)

	var cands []Candidate
	var missing []Missing
	seen := map[string]bool{}
	for _, e := range registry {
		hp := net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
		if seen[strings.ToLower(hp)] {
			continue
		}
		seen[strings.ToLower(hp)] = true

		if u := fromOmniRoute(ctx, src, e, connsOf[e.ID]); u != "" {
			cands = append(cands, Candidate{Name: e.Name, HostPort: hp, URL: u, Source: "omniroute"})
			continue
		}
		if u, ok := poolIdx[strings.ToLower(hp)]; ok {
			cands = append(cands, Candidate{Name: e.Name, HostPort: hp, URL: u, Source: "pool"})
			continue
		}
		reason := "not assigned to a connection in OmniRoute and not in the local pool"
		if len(connsOf[e.ID]) > 0 {
			reason = "OmniRoute did not return its credentials and it is not in the local pool"
		}
		missing = append(missing, Missing{Name: e.Name, HostPort: hp, Reason: reason})
	}
	return cands, missing, nil
}

// fromOmniRoute reads a proxy's real credentials through a connection it is assigned to. It returns
// "" when none of them yields a matching proxy.
func fromOmniRoute(ctx context.Context, src Source, e omniroute.RegistryEntry, connIDs []string) string {
	for _, id := range connIDs {
		r, err := src.ResolveConnectionProxy(ctx, id)
		if err != nil || r.Proxy == nil {
			continue
		}
		p := r.Proxy
		// The connection must really resolve to THIS proxy: another level may win for it.
		if !strings.EqualFold(p.Host, e.Host) || int(p.Port) != e.Port || (p.Username == "" && p.Password == "") {
			continue
		}
		scheme := firstNonEmpty(p.Type, e.Type, "http")
		u := &url.URL{Scheme: scheme, Host: net.JoinHostPort(e.Host, strconv.Itoa(e.Port))}
		if p.Password != "" {
			u.User = url.UserPassword(p.Username, p.Password)
		} else {
			u.User = url.User(p.Username)
		}
		return u.String()
	}
	return ""
}

func indexPool(pool []string) map[string]string {
	idx := map[string]string{}
	for _, raw := range pool {
		u, err := egress.ParseProxyURL(strings.TrimSpace(raw))
		if err != nil || u.Hostname() == "" {
			continue
		}
		port := u.Port()
		if port == "" {
			continue
		}
		key := strings.ToLower(net.JoinHostPort(u.Hostname(), port))
		if _, dup := idx[key]; !dup {
			idx[key] = raw
		}
	}
	return idx
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ProfileAPI is the part of the profile API Apply uses; *adspower.Client implements it.
type ProfileAPI interface {
	ListProfiles(ctx context.Context, page, pageSize int) ([]adspower.Profile, error)
	CreateProfile(ctx context.Context, req adspower.CreateProfileRequest) (string, error)
}

// Outcome of one proxy in Apply.
type Outcome string

const (
	Created     Outcome = "created"
	WouldCreate Outcome = "would create"
	Exists      Outcome = "exists"
	Failed      Outcome = "failed"
)

// Result is the outcome for one candidate.
type Result struct {
	Candidate
	Outcome     Outcome
	ProfileID   string
	ProfileName string
	// Err never carries the proxy's credentials.
	Err error
}

// Apply creates one profile per candidate, bound to its proxy. It is idempotent: a proxy that already
// has a profile (by name, or by the same host and port under any name) is left alone. With dryRun
// nothing is created.
func Apply(ctx context.Context, api ProfileAPI, cands []Candidate, engine string, dryRun bool) ([]Result, error) {
	existing, err := listAll(ctx, api)
	if err != nil {
		return nil, fmt.Errorf("list the profiles: %w", err)
	}
	byName := map[string]adspower.Profile{}
	byEndpoint := map[string]adspower.Profile{}
	for _, p := range existing {
		byName[strings.ToLower(p.Name)] = p
		if h, pt := strings.TrimSpace(p.ProxyConfig.ProxyHost), strings.TrimSpace(p.ProxyConfig.ProxyPort); h != "" && pt != "" {
			byEndpoint[strings.ToLower(net.JoinHostPort(h, pt))] = p
		}
	}

	results := make([]Result, 0, len(cands))
	for _, c := range cands {
		name := ProfileName(c.HostPort)
		r := Result{Candidate: c, ProfileName: name}

		if p, ok := byName[strings.ToLower(name)]; ok {
			r.Outcome, r.ProfileID, r.ProfileName = Exists, p.UserID, p.Name
		} else if p, ok := byEndpoint[strings.ToLower(c.HostPort)]; ok {
			r.Outcome, r.ProfileID, r.ProfileName = Exists, p.UserID, p.Name
		} else if dryRun {
			r.Outcome = WouldCreate
		} else {
			pc, perr := adspower.ProxyConfigFromURL(c.URL)
			if perr != nil {
				r.Outcome, r.Err = Failed, scrub(perr, c.URL)
			} else if id, cerr := api.CreateProfile(ctx, adspower.CreateProfileRequest{Name: name, Browser: engine, ProxyConfig: pc}); cerr != nil {
				r.Outcome, r.Err = Failed, scrub(cerr, c.URL)
			} else {
				r.Outcome, r.ProfileID = Created, id
			}
		}
		results = append(results, r)
	}
	return results, nil
}

func listAll(ctx context.Context, api ProfileAPI) ([]adspower.Profile, error) {
	const pageSize = 100
	var out []adspower.Profile
	for page := 1; ; page++ {
		batch, err := api.ListProfiles(ctx, page, pageSize)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < pageSize {
			return out, nil
		}
	}
}

// scrub removes a proxy's username and password from an error message.
func scrub(err error, proxyURL string) error {
	msg := err.Error()
	if u, perr := url.Parse(proxyURL); perr == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			msg = strings.ReplaceAll(msg, pw, "***")
		}
		if name := u.User.Username(); len(name) > 3 {
			msg = strings.ReplaceAll(msg, name, "***")
		}
	}
	return fmt.Errorf("%s", msg)
}
