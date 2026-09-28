// Package omniroute is a pure-Go client for the local OmniRoute (OmniRouter) AI gateway's
// management API. It imports Antigravity (`agy`) accounts exported from this switcher and binds
// each account's outbound proxy inside OmniRoute.
//
// Contract verified against OmniRoute source:
//   - POST /api/providers/agy-auth/import-bulk  {entries:[{json,name,email}...], overwriteExisting?}
//     where json requires access_token + refresh_token (client_id/secret/project_id NOT needed;
//     OmniRoute enriches project/email/tier from the Antigravity backend).
//   - PUT  /api/settings/proxy  {level:"key", id:"<connectionId>", proxy:{type,host,port,username?,password?}}
//
// Zero non-stdlib dependencies, preserving the CGO_ENABLED=0 invariant.
package omniroute

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is where OmniRoute exposes its management API by default.
const DefaultBaseURL = "https://localhost:20128"

// DefaultTimeout bounds a single management API call. Imports enrich against the Antigravity
// backend, so allow generous time.
const DefaultTimeout = 60 * time.Second

// MaxBulkEntries is OmniRoute's per-request cap for agy bulk import.
const MaxBulkEntries = 50

// Client talks to the OmniRoute management API.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL overrides the OmniRoute base URL (default DefaultBaseURL).
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if s := strings.TrimSpace(u); s != "" {
			c.baseURL = strings.TrimRight(s, "/")
		}
	}
}

// WithToken sets the Bearer token used for management auth (from POST /api/auth/login). Empty is
// allowed for instances running with REQUIRE_API_KEY=false.
func WithToken(t string) Option {
	return func(c *Client) { c.token = strings.TrimSpace(t) }
}

// WithInsecureTLS disables TLS verification, needed for OmniRoute's self-signed localhost cert.
func WithInsecureTLS(insecure bool) Option {
	return func(c *Client) {
		if insecure {
			c.http.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // localhost self-signed cert
		}
	}
}

// WithHTTPClient injects a custom *http.Client (e.g. for tests).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// NewClient constructs a Client with sane defaults.
func NewClient(opts ...Option) *Client {
	c := &Client{
		baseURL: DefaultBaseURL,
		http:    &http.Client{Timeout: DefaultTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// AgyEntry is one account to import: the agy token object plus a display name and email.
type AgyEntry struct {
	JSON  map[string]any `json:"json"`
	Name  string         `json:"name,omitempty"`
	Email string         `json:"email,omitempty"`
}

// Connection is the subset of an imported provider connection returned by the import.
type Connection struct {
	Provider string `json:"provider,omitempty"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	Email    string `json:"email"`
}

// ImportError is one failed bulk entry.
type ImportError struct {
	Index   int    `json:"index"`
	Name    string `json:"name"`
	Message string `json:"message"`
}

// ImportResult is the response of a bulk agy import.
type ImportResult struct {
	Success int           `json:"success"`
	Failed  int           `json:"failed"`
	Total   int           `json:"total"`
	Created []Connection  `json:"created"`
	Errors  []ImportError `json:"errors"`
}

// BuildAgyTokenJSON assembles the agy token object OmniRoute requires from the credentials this
// switcher stores. access_token and refresh_token are mandatory; expiry is optional.
func BuildAgyTokenJSON(accessToken, refreshToken string, expiry time.Time) map[string]any {
	tok := map[string]any{
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"token_type":    "Bearer",
	}
	if !expiry.IsZero() {
		tok["expiry"] = expiry.UTC().Format(time.RFC3339)
	}
	return tok
}

// ImportBulkAgy imports up to MaxBulkEntries agy accounts in one call.
func (c *Client) ImportBulkAgy(ctx context.Context, entries []AgyEntry, overwriteExisting bool) (*ImportResult, error) {
	if len(entries) == 0 {
		return &ImportResult{}, nil
	}
	if len(entries) > MaxBulkEntries {
		return nil, fmt.Errorf("omniroute: %d entries exceeds the per-request cap of %d", len(entries), MaxBulkEntries)
	}
	body := map[string]any{
		"entries":           entries,
		"overwriteExisting": overwriteExisting,
	}
	var out ImportResult
	if err := c.do(ctx, http.MethodPost, "/api/providers/agy-auth/import-bulk", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ProxyConfig is OmniRoute's inline proxy object.
type ProxyConfig struct {
	Type     string `json:"type"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// ProxyConfigFromURL parses a proxy URL (scheme://user:pass@host:port) into OmniRoute's proxy
// object. It returns ok=false for an empty URL (direct connection).
func ProxyConfigFromURL(raw string) (cfg ProxyConfig, ok bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ProxyConfig{}, false, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ProxyConfig{}, false, fmt.Errorf("omniroute: invalid proxy url: %w", err)
	}
	if u.Hostname() == "" || u.Port() == "" {
		return ProxyConfig{}, false, fmt.Errorf("omniroute: proxy url must be scheme://host:port")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return ProxyConfig{}, false, fmt.Errorf("omniroute: invalid proxy port %q: %w", u.Port(), err)
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "http"
	}
	cfg = ProxyConfig{Type: scheme, Host: u.Hostname(), Port: port}
	if u.User != nil {
		cfg.Username = u.User.Username()
		if pw, has := u.User.Password(); has {
			cfg.Password = pw
		}
	}
	return cfg, true, nil
}

// AssignConnectionProxy binds a proxy to a single imported connection (account-level scope).
func (c *Client) AssignConnectionProxy(ctx context.Context, connectionID string, proxy ProxyConfig) error {
	if strings.TrimSpace(connectionID) == "" {
		return fmt.Errorf("omniroute: empty connection id")
	}
	body := map[string]any{
		"level": "key",
		"id":    connectionID,
		"proxy": proxy,
	}
	return c.do(ctx, http.MethodPut, "/api/settings/proxy", body, nil)
}

// RegistryProxy is one entry for OmniRoute's proxy registry.
type RegistryProxy struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Region   string `json:"region,omitempty"`
}

// RegistryProxyFromURL maps a validated proxy URL onto a registry entry. Names follow the
// "ws-<host>" convention of the existing import script; OmniRoute upserts by host and port.
func RegistryProxyFromURL(u *url.URL, region string) (RegistryProxy, error) {
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return RegistryProxy{}, fmt.Errorf("omniroute: proxy needs an explicit port")
	}
	typ := u.Scheme
	if typ == "socks5h" {
		typ = "socks5"
	}
	p := RegistryProxy{Name: "ws-" + u.Hostname(), Type: typ, Host: u.Hostname(), Port: port, Region: strings.TrimSpace(region)}
	if u.User != nil {
		p.Username = u.User.Username()
		p.Password, _ = u.User.Password()
	}
	return p, nil
}

// MaxBulkProxies is OmniRoute's per-request cap for the proxy bulk import.
const MaxBulkProxies = 100

// BulkProxyResult is the outcome for one proxy of a bulk import.
type BulkProxyResult struct {
	Success bool   `json:"success"`
	Action  string `json:"action,omitempty"` // "created" or "updated"
	ID      string `json:"id,omitempty"`
	Error   string `json:"error,omitempty"`
}

// BulkImportProxies upserts proxies into OmniRoute's registry via POST
// /api/settings/proxies/bulk-import, keyed by host+port+username: an existing proxy is updated
// instead of duplicated, and its result says so ("updated"). (POST /api/v1/management/proxies
// always inserts, so re-sending through it duplicates entries.) Results come back one per input,
// in order; the token needs management scope. On error, the results of the chunks that already
// succeeded are still returned, since OmniRoute has applied them.
func (c *Client) BulkImportProxies(ctx context.Context, proxies []RegistryProxy) ([]BulkProxyResult, error) {
	out := make([]BulkProxyResult, 0, len(proxies))
	for start := 0; start < len(proxies); start += MaxBulkProxies {
		chunk := proxies[start:min(start+MaxBulkProxies, len(proxies))]
		var resp struct {
			Results []BulkProxyResult `json:"results"`
		}
		if err := c.do(ctx, http.MethodPost, "/api/settings/proxies/bulk-import", map[string]any{"items": chunk}, &resp); err != nil {
			return out, scrubPasswords(err, chunk)
		}
		if len(resp.Results) != len(chunk) {
			return out, fmt.Errorf("omniroute: bulk import returned %d results for %d proxies", len(resp.Results), len(chunk))
		}
		for i := range resp.Results {
			resp.Results[i].Error = scrubPasswords(errors.New(resp.Results[i].Error), chunk).Error()
		}
		out = append(out, resp.Results...)
	}
	return out, nil
}

// scrubPasswords removes the proxies' passwords from err's text: do() quotes response bodies,
// which may echo the request back.
func scrubPasswords(err error, proxies []RegistryProxy) error {
	msg := err.Error()
	for _, p := range proxies {
		if p.Password != "" {
			msg = strings.ReplaceAll(msg, p.Password, "***")
		}
	}
	return errors.New(msg)
}

// RegistryEntry is a proxy as OmniRoute lists it. The server redacts credentials (username and
// password come back as "***"), so entries can only be matched by host and port.
type RegistryEntry struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Status string `json:"status"`
}

// listPageSize is the largest page OmniRoute's list endpoints serve.
const listPageSize = 200

// ListProxies returns every entry of OmniRoute's proxy registry (GET /api/v1/management/proxies).
func (c *Client) ListProxies(ctx context.Context) ([]RegistryEntry, error) {
	var all []RegistryEntry
	for offset := 0; ; {
		var resp struct {
			Items []RegistryEntry `json:"items"`
			Page  struct {
				Total int `json:"total"`
			} `json:"page"`
		}
		path := fmt.Sprintf("/api/v1/management/proxies?limit=%d&offset=%d", listPageSize, offset)
		if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Items...)
		offset += len(resp.Items)
		if len(resp.Items) == 0 || offset >= resp.Page.Total {
			return all, nil
		}
	}
}

// ListConnections returns the provider connections of one provider (GET /api/providers). The
// server strips tokens; only identity fields are decoded here.
func (c *Client) ListConnections(ctx context.Context, provider string) ([]Connection, error) {
	var all []Connection
	for offset := 0; ; {
		var resp struct {
			Connections []Connection `json:"connections"`
			Total       int          `json:"total"`
		}
		path := fmt.Sprintf("/api/providers?provider=%s&limit=%d&offset=%d", url.QueryEscape(provider), listPageSize, offset)
		if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Connections...)
		offset += len(resp.Connections)
		if len(resp.Connections) == 0 || offset >= resp.Total {
			return all, nil
		}
	}
}

// ResolvedProxy is the proxy OmniRoute effectively uses for a connection, and the level it comes
// from: "account"/"key" (bound to that connection), "provider" or "global" (inherited, shared with
// other connections), "apiKey", or "direct" (no proxy). The password is not decoded; the username is, since proxies sharing a gateway
// endpoint differ only by it.
type ResolvedProxy struct {
	Level string `json:"level"`
	Proxy *struct {
		Type     string   `json:"type"`
		Host     string   `json:"host"`
		Port     flexPort `json:"port"`
		Username string   `json:"username"`
	} `json:"proxy"`
}

// ResolveConnectionProxy asks OmniRoute which proxy a connection would use right now
// (GET /api/v1/management/proxies/assignments?resolve_connection_id=...), accounting for the
// registry and the legacy per-key settings alike.
func (c *Client) ResolveConnectionProxy(ctx context.Context, connectionID string) (ResolvedProxy, error) {
	var out ResolvedProxy
	path := "/api/v1/management/proxies/assignments?resolve_connection_id=" + url.QueryEscape(connectionID)
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// flexPort accepts a port encoded as a JSON number or string (legacy settings store either).
type flexPort int

func (p *flexPort) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*p = 0
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("omniroute: invalid port %q", s)
	}
	*p = flexPort(n)
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("omniroute: marshal request: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("omniroute: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("omniroute: request failed (is OmniRoute running at %s?): %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("omniroute: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("omniroute: http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("omniroute: decode response: %w", err)
		}
	}
	return nil
}
