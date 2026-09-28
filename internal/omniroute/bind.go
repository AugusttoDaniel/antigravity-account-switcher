package omniroute

// Binding a proxy to an OmniRoute connection that already exists.
//
// Import time only covers connections created by the same call. This binds afterwards, through the
// registry: the proxy is upserted by host+port+username (which yields its exact id even on a gateway
// endpoint shared by several credentials) and assigned to the connection at "account" scope, which
// replaces whatever the connection used before. The result is checked by asking OmniRoute what the
// connection now resolves to, since an accepted request is not proof the proxy is in effect.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// BindResult describes what BindConnectionProxy did.
type BindResult struct {
	// Changed is false when the connection already used this proxy and nothing was written.
	Changed bool
	// ProxyID is the registry entry now bound (empty when nothing changed).
	ProxyID string
	// Was is what the connection used before: "direct", or "type://host:port (level level)".
	Was string
}

// AssignRegistryProxy binds a registry proxy to one connection (PUT
// /api/v1/management/proxies/assignments, scope "account"). It replaces the connection's previous
// proxy and, unlike the legacy per-key setting, is what OmniRoute's proxy health stats follow.
func (c *Client) AssignRegistryProxy(ctx context.Context, connectionID, proxyID string) error {
	if strings.TrimSpace(connectionID) == "" || strings.TrimSpace(proxyID) == "" {
		return fmt.Errorf("omniroute: connection id and proxy id are required")
	}
	body := map[string]any{"scope": "account", "scopeId": connectionID, "proxyId": proxyID}
	return c.do(ctx, http.MethodPut, "/api/v1/management/proxies/assignments", body, nil)
}

// BindConnectionProxy makes the connection use proxy. It is idempotent: a connection that already
// resolves to the same proxy (host, port and username, at any level) is left untouched. After
// writing, it re-resolves the connection and fails if OmniRoute does not report the new proxy.
func (c *Client) BindConnectionProxy(ctx context.Context, connectionID string, proxy *url.URL) (BindResult, error) {
	proxy = withDefaultPort(proxy)

	before, err := c.ResolveConnectionProxy(ctx, connectionID)
	if err != nil {
		return BindResult{}, fmt.Errorf("check the connection's current proxy: %w", err)
	}
	if sameProxy(before, proxy) {
		return BindResult{Was: describeResolved(before)}, nil
	}

	entry, err := RegistryProxyFromURL(proxy, "")
	if err != nil {
		return BindResult{}, err
	}
	// An upsert rewrites the name of an entry it matches. Keep the one already at this endpoint
	// (when it is unambiguous) so binding does not rename what the user set up.
	if listed, lerr := c.ListProxies(ctx); lerr == nil {
		var atEndpoint []RegistryEntry
		for _, e := range listed {
			if strings.EqualFold(e.Host, entry.Host) && e.Port == entry.Port {
				atEndpoint = append(atEndpoint, e)
			}
		}
		if len(atEndpoint) == 1 && atEndpoint[0].Name != "" {
			entry.Name = atEndpoint[0].Name
		}
	}

	results, err := c.BulkImportProxies(ctx, []RegistryProxy{entry})
	if err != nil {
		return BindResult{}, fmt.Errorf("register the proxy in OmniRoute: %w", err)
	}
	if len(results) != 1 || !results[0].Success || results[0].ID == "" {
		msg := "no id returned"
		if len(results) == 1 && results[0].Error != "" {
			msg = results[0].Error
		}
		return BindResult{}, fmt.Errorf("register the proxy in OmniRoute: %s", msg)
	}
	proxyID := results[0].ID

	if err := c.AssignRegistryProxy(ctx, connectionID, proxyID); err != nil {
		return BindResult{}, fmt.Errorf("assign the proxy to the connection: %w", err)
	}

	after, err := c.ResolveConnectionProxy(ctx, connectionID)
	if err != nil {
		return BindResult{}, fmt.Errorf("verify the binding: %w", err)
	}
	if !sameProxy(after, proxy) {
		hint := ""
		if after.Proxy == nil {
			hint = " (are proxies enabled in OmniRoute's settings?)"
		}
		return BindResult{}, fmt.Errorf("OmniRoute accepted the assignment but the connection resolves to %s%s", describeResolved(after), hint)
	}
	return BindResult{Changed: true, ProxyID: proxyID, Was: describeResolved(before)}, nil
}

// sameProxy reports whether OmniRoute's resolution is exactly proxy: same host, port and username.
// Passwords are not compared (OmniRoute may not return them, and rotating one is not a rebind).
func sameProxy(r ResolvedProxy, proxy *url.URL) bool {
	if r.Proxy == nil || r.Proxy.Host == "" {
		return false
	}
	port := proxy.Port()
	return strings.EqualFold(r.Proxy.Host, proxy.Hostname()) &&
		fmt.Sprint(int(r.Proxy.Port)) == port &&
		r.Proxy.Username == proxy.User.Username()
}

// describeResolved renders a resolution without credentials, e.g. "http://1.2.3.4:8080 (account level)".
func describeResolved(r ResolvedProxy) string {
	if r.Proxy == nil || r.Proxy.Host == "" {
		return "direct"
	}
	typ := r.Proxy.Type
	if typ == "" {
		typ = "http"
	}
	return fmt.Sprintf("%s://%s:%d (%s level)", typ, r.Proxy.Host, int(r.Proxy.Port), r.Level)
}

// withDefaultPort returns u with its scheme's default port when it has none, since the registry
// needs an explicit port.
func withDefaultPort(u *url.URL) *url.URL {
	if u.Port() != "" {
		return u
	}
	port := "80"
	switch u.Scheme {
	case "https":
		port = "443"
	case "socks5", "socks5h":
		port = "1080"
	}
	c := *u
	c.Host = net.JoinHostPort(u.Hostname(), port)
	return &c
}
