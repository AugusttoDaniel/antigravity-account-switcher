// Package adspower is a pure-Go client for the ADS Power Local API
// (https://localapi-doc-en.adspower.com/). It lets the switcher create isolated browser
// profiles bound to a proxy + antidetect fingerprint, launch them, and obtain a CDP
// endpoint so the Google OAuth consent screen can be driven inside the isolated profile
// instead of the operator's real browser.
//
// The client is transport-only: it performs no browser automation itself (that is the job
// of the CDP opener) and has zero non-stdlib dependencies, preserving the CGO_ENABLED=0
// invariant.
package adspower

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is where the ADS Power desktop app exposes its Local API.
const DefaultBaseURL = "http://local.adspower.net:50325"

// DefaultMinInterval throttles calls: the Local API rejects bursts faster than ~1 req/s.
const DefaultMinInterval = 1100 * time.Millisecond

// DefaultTimeout bounds a single Local API call. Browser start can be slow, so callers
// that need longer should pass their own context deadline.
const DefaultTimeout = 30 * time.Second

// Client talks to the ADS Power Local API. It is safe for concurrent use; calls are
// serialized through an internal rate gate to respect the Local API throttle.
type Client struct {
	baseURL     string
	apiKey      string
	http        *http.Client
	minInterval time.Duration

	gate     sync.Mutex
	lastCall time.Time
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL overrides the Local API base URL (default DefaultBaseURL).
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if s := strings.TrimSpace(u); s != "" {
			c.baseURL = strings.TrimRight(s, "/")
		}
	}
}

// WithAPIKey sets the api_key sent with every request (required when the user has enabled
// "API key" in ADS Power settings; harmless otherwise).
func WithAPIKey(k string) Option {
	return func(c *Client) { c.apiKey = strings.TrimSpace(k) }
}

// WithHTTPClient injects a custom *http.Client (e.g. for tests).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// WithMinInterval overrides the inter-call throttle. A zero or negative value disables it.
func WithMinInterval(d time.Duration) Option {
	return func(c *Client) { c.minInterval = d }
}

// NewClient constructs a Client with sane defaults.
func NewClient(opts ...Option) *Client {
	c := &Client{
		baseURL:     DefaultBaseURL,
		http:        &http.Client{Timeout: DefaultTimeout},
		minInterval: DefaultMinInterval,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// envelope is the common ADS Power Local API response wrapper. code 0 means success.
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// APIError is returned when the Local API responds with a non-zero code.
type APIError struct {
	Code int
	Msg  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("adspower api error (code %d): %s", e.Code, e.Msg)
}

// ProxyConfig mirrors the ADS Power user_proxy_config object.
type ProxyConfig struct {
	// ProxySoft is the provider tag ADS Power uses; "other" for a raw proxy and
	// "no_proxy" for a direct profile.
	ProxySoft string `json:"proxy_soft,omitempty"`
	// ProxyType is the scheme: http, https, socks5.
	ProxyType     string `json:"proxy_type,omitempty"`
	ProxyHost     string `json:"proxy_host,omitempty"`
	ProxyPort     string `json:"proxy_port,omitempty"`
	ProxyUser     string `json:"proxy_user,omitempty"`
	ProxyPassword string `json:"proxy_password,omitempty"`
}

// URL renders the proxy config as a standard proxy URL (scheme://user:pass@host:port),
// or "" when the profile is direct. This is the exact form the switcher stores in
// Account.ProxyURL, so a profile's proxy and an account's proxy stay identical.
func (p ProxyConfig) URL() string {
	host := strings.TrimSpace(p.ProxyHost)
	port := strings.TrimSpace(p.ProxyPort)
	if host == "" || port == "" || strings.EqualFold(p.ProxySoft, "no_proxy") {
		return ""
	}
	scheme := strings.TrimSpace(p.ProxyType)
	if scheme == "" {
		scheme = "http"
	}
	u := &url.URL{Scheme: scheme, Host: net.JoinHostPort(host, port)}
	if p.ProxyUser != "" {
		if p.ProxyPassword != "" {
			u.User = url.UserPassword(p.ProxyUser, p.ProxyPassword)
		} else {
			u.User = url.User(p.ProxyUser)
		}
	}
	return u.String()
}

// ProxyConfigFromURL parses a proxy URL (as stored in Account.ProxyURL) into the shape the
// Local API expects. An empty string yields a no_proxy (direct) config.
func ProxyConfigFromURL(raw string) (ProxyConfig, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ProxyConfig{ProxySoft: "no_proxy"}, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ProxyConfig{}, fmt.Errorf("invalid proxy url: %w", err)
	}
	if u.Host == "" {
		return ProxyConfig{}, fmt.Errorf("proxy url missing host:port")
	}
	pc := ProxyConfig{
		ProxySoft: "other",
		ProxyType: u.Scheme,
		ProxyHost: u.Hostname(),
		ProxyPort: u.Port(),
	}
	if u.User != nil {
		pc.ProxyUser = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			pc.ProxyPassword = pw
		}
	}
	return pc, nil
}

// Profile is a subset of an ADS Power profile record.
type Profile struct {
	UserID      string      `json:"user_id"`
	Name        string      `json:"name"`
	GroupID     string      `json:"group_id"`
	ProxyConfig ProxyConfig `json:"user_proxy_config"`
}

// CreateProfileRequest describes a new isolated profile to create.
type CreateProfileRequest struct {
	Name    string `json:"name"`
	GroupID string `json:"group_id"`
	// Browser selects the engine. The AliasMode-compatible API requires it ("cloak" = Chromium /
	// CloakBrowser, which exposes a CDP endpoint; "firefox" = AliasMode Firefox). ADS Power ignores
	// the field (it uses SunBrowser). Empty is omitted.
	Browser     string      `json:"browser,omitempty"`
	ProxyConfig ProxyConfig `json:"user_proxy_config"`
	// FingerprintConfig is passed through as-is; an empty map lets the browser auto-generate a
	// random fingerprint, which is what we want for a fresh account.
	FingerprintConfig map[string]any `json:"fingerprint_config,omitempty"`
}

// BrowserWS holds the endpoints returned when a profile's browser is launched.
type BrowserWS struct {
	Selenium  string `json:"selenium"`
	Puppeteer string `json:"puppeteer"` // CDP websocket URL used by the chromedp opener
}

// BrowserStartData is the payload of a successful browser/start.
type BrowserStartData struct {
	WS        BrowserWS `json:"ws"`
	DebugPort string    `json:"debug_port"`
	WebDriver string    `json:"webdriver"`
}

// ListProfiles returns profiles, one page at a time (ADS Power paginates).
func (c *Client) ListProfiles(ctx context.Context, page, pageSize int) ([]Profile, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 50
	}
	q := url.Values{}
	q.Set("page", strconv.Itoa(page))
	q.Set("page_size", strconv.Itoa(pageSize))

	var out struct {
		List []Profile `json:"list"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/user/list", q, nil, &out); err != nil {
		return nil, err
	}
	return out.List, nil
}

// GetProfile returns a single profile by its user_id, including its proxy configuration. On the
// ADS Power free plan the underlying user/list endpoint is gated and this returns an APIError.
func (c *Client) GetProfile(ctx context.Context, userID string) (*Profile, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("adspower: empty user_id")
	}
	q := url.Values{}
	q.Set("user_id", userID)
	q.Set("page_size", "1")

	var out struct {
		List []Profile `json:"list"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/user/list", q, nil, &out); err != nil {
		return nil, err
	}
	if len(out.List) == 0 {
		return nil, fmt.Errorf("adspower: profile %s not found", userID)
	}
	return &out.List[0], nil
}

// CreateProfile creates a new isolated profile and returns its user_id.
func (c *Client) CreateProfile(ctx context.Context, req CreateProfileRequest) (string, error) {
	if strings.TrimSpace(req.GroupID) == "" {
		req.GroupID = "0"
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/user/create", nil, req, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("adspower: create returned empty profile id")
	}
	return out.ID, nil
}

// StartBrowser launches the profile's browser and returns its CDP/Selenium endpoints.
// headless controls whether ADS Power opens a visible window; for the assisted login flow
// the window must be visible so the human can complete Google sign-in, so pass false.
func (c *Client) StartBrowser(ctx context.Context, userID string, headless bool) (*BrowserStartData, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, fmt.Errorf("adspower: empty user_id")
	}
	q := url.Values{}
	q.Set("user_id", userID)
	if headless {
		q.Set("headless", "1")
	} else {
		q.Set("headless", "0")
	}
	var out BrowserStartData
	if err := c.do(ctx, http.MethodGet, "/api/v1/browser/start", q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StopBrowser closes the profile's browser.
func (c *Client) StopBrowser(ctx context.Context, userID string) error {
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("adspower: empty user_id")
	}
	q := url.Values{}
	q.Set("user_id", userID)
	return c.do(ctx, http.MethodGet, "/api/v1/browser/stop", q, nil, nil)
}

// Ping reports whether the Local API answers. ADS Power serves /status; the AliasMode-compatible
// API serves it under /api/v1/status, so both are tried.
func (c *Client) Ping(ctx context.Context) error {
	var lastErr error
	for _, path := range []string{"/status", "/api/v1/status"} {
		err := c.do(ctx, http.MethodGet, path, nil, nil, nil)
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return lastErr
}

// BrowserActive reports whether the profile's browser is currently running.
func (c *Client) BrowserActive(ctx context.Context, userID string) (bool, error) {
	if strings.TrimSpace(userID) == "" {
		return false, fmt.Errorf("adspower: empty user_id")
	}
	q := url.Values{}
	q.Set("user_id", userID)
	var out struct {
		Status string `json:"status"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/browser/active", q, nil, &out); err != nil {
		return false, err
	}
	return strings.EqualFold(out.Status, "Active"), nil
}

// throttle blocks until at least minInterval has elapsed since the previous call, or the
// context is cancelled. It holds the gate for the whole call so requests never overlap,
// which is what the Local API's ~1 req/s limit requires.
func (c *Client) throttle(ctx context.Context) error {
	if c.minInterval <= 0 {
		return nil
	}
	wait := time.Until(c.lastCall.Add(c.minInterval))
	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	c.gate.Lock()
	defer c.gate.Unlock()
	if err := c.throttle(ctx); err != nil {
		return err
	}
	defer func() { c.lastCall = time.Now() }()

	if query == nil {
		query = url.Values{}
	}
	if c.apiKey != "" {
		query.Set("api_key", c.apiKey)
	}

	endpoint := c.baseURL + path
	if enc := query.Encode(); enc != "" {
		endpoint += "?" + enc
	}

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("adspower: marshal request: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("adspower: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// The transport error quotes the whole URL, and the API key travels in its query string.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("adspower: request failed (is ADS Power / AliasMode running with the Local API enabled?): %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("adspower: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("adspower: http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("adspower: decode envelope: %w", err)
	}
	if env.Code != 0 {
		return &APIError{Code: env.Code, Msg: env.Msg}
	}
	if out != nil && len(env.Data) > 0 && !bytes.Equal(bytes.TrimSpace(env.Data), []byte("null")) {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("adspower: decode data: %w", err)
		}
	}
	return nil
}
