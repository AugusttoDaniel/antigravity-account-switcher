package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/config"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/oauth"
)

//go:embed dist/*
var embeddedDistFS embed.FS

// ServerConfig holds configuration options for Server.
type ServerConfig struct {
	Port                 int
	BindAddr             string
	Version              string
	ProxyHandler         http.Handler
	Poller               QuotaPoller
	AppConfig            *config.Config
	FallbackConfigSetter FallbackConfigSetter
	ReadTimeout          time.Duration
	WriteTimeout         time.Duration
}

// Option configures ServerConfig.
type Option func(*ServerConfig)

// WithPort sets the listening port.
func WithPort(p int) Option {
	return func(c *ServerConfig) { c.Port = p }
}

// WithBindAddr sets the IP/host to bind to (e.g. 127.0.0.1 or 0.0.0.0).
func WithBindAddr(addr string) Option {
	return func(c *ServerConfig) { c.BindAddr = addr }
}

// WithVersion sets the server version string.
func WithVersion(v string) Option {
	return func(c *ServerConfig) { c.Version = v }
}

// WithProxyHandler mounts a reverse proxy handler to intercept Cloud Code traffic on the same port.
func WithProxyHandler(h http.Handler) Option {
	return func(c *ServerConfig) { c.ProxyHandler = h }
}

// WithReadTimeout sets the HTTP read timeout.
func WithReadTimeout(d time.Duration) Option {
	return func(c *ServerConfig) { c.ReadTimeout = d }
}

// WithWriteTimeout sets the HTTP write timeout.
func WithWriteTimeout(d time.Duration) Option {
	return func(c *ServerConfig) { c.WriteTimeout = d }
}

// WithPoller configures the quota poller for on-demand quota refreshes.
func WithPoller(p QuotaPoller) Option {
	return func(c *ServerConfig) { c.Poller = p }
}

// WithConfig sets the app configuration instance.
func WithConfig(cfg *config.Config) Option {
	return func(c *ServerConfig) { c.AppConfig = cfg }
}

// WithFallbackConfigSetter sets the dynamic fallback setter for live proxy updates.
func WithFallbackConfigSetter(setter FallbackConfigSetter) Option {
	return func(c *ServerConfig) { c.FallbackConfigSetter = setter }
}

// Server serves both the Web UI/REST API dashboard and the local reverse proxy.
type Server struct {
	cfg          ServerConfig
	api          *APIHandler
	proxyHandler http.Handler
	distFS       fs.FS

	mu         sync.Mutex
	httpServer *http.Server
	listener   net.Listener
	addr       string
}

// NewServer constructs an initialized Server.
func NewServer(
	accountRepo domain.AccountRepository,
	quotaRepo domain.QuotaRepository,
	metricsService domain.MetricsService,
	broadcaster domain.EventBroadcaster,
	eventRepo domain.EventRepository,
	oauthEngine oauth.OAuthEngine,
	opts ...Option,
) (*Server, error) {
	cfg := ServerConfig{
		Port:         8080,
		BindAddr:     "127.0.0.1",
		Version:      "1.1.0",
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // 0 allows indefinite SSE and streaming connections
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	sub, err := fs.Sub(embeddedDistFS, "dist")
	if err != nil {
		return nil, fmt.Errorf("failed to locate embedded dist filesystem: %w", err)
	}

	api := NewAPIHandler(
		accountRepo,
		quotaRepo,
		metricsService,
		broadcaster,
		eventRepo,
		oauthEngine,
		cfg.Version,
	)

	if cfg.Poller != nil {
		api.SetPoller(cfg.Poller)
	}
	if cfg.AppConfig != nil {
		api.SetConfig(cfg.AppConfig)
	}
	if cfg.FallbackConfigSetter != nil {
		api.SetFallbackConfigSetter(cfg.FallbackConfigSetter)
	}

	return &Server{
		cfg:          cfg,
		api:          api,
		proxyHandler: cfg.ProxyHandler,
		distFS:       sub,
	}, nil
}

// ServeHTTP routes incoming requests to API, Proxy, or Static UI assets.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 0. Upstream Cloud Code proxy traffic is identified by host/path and legitimately carries
	// a non-local Host header, so it is handled before the loopback/CSRF guard below.
	if s.proxyHandler != nil && s.isProxyRequest(r) {
		s.proxyHandler.ServeHTTP(w, r)
		return
	}

	// 0b. Everything else is the local dashboard and its management API. When bound to a
	// loopback address (the default), reject requests whose Host header is not itself a
	// loopback name. This defends against DNS-rebinding, where a page on a malicious domain
	// that resolves to 127.0.0.1 drives the local API from the victim's browser.
	if s.loopbackBound() && !isLocalHostHeader(r.Host) {
		http.Error(w, "forbidden: non-local Host header", http.StatusForbidden)
		return
	}

	// 0c. CSRF: reject state-changing requests carrying a cross-origin Origin header. Same-origin
	// browser requests either omit Origin or send a loopback origin; a cross-site attacker's
	// Origin is its own domain.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if origin := r.Header.Get("Origin"); origin != "" && !isLocalOrigin(origin) {
			http.Error(w, "forbidden: cross-origin request", http.StatusForbidden)
			return
		}
	}

	path := r.URL.Path

	// 1. API Endpoints
	if path == "/api/status" {
		s.api.HandleStatus(w, r)
		return
	}
	if strings.HasPrefix(path, "/api/accounts") {
		s.api.HandleAccounts(w, r)
		return
	}
	if path == "/api/proxies" || strings.HasPrefix(path, "/api/proxies/") {
		s.api.HandleProxies(w, r)
		return
	}
	if strings.HasPrefix(path, "/api/omniroute/") {
		s.api.HandleOmniRoute(w, r)
		return
	}
	if path == "/api/onboarding/status" {
		s.api.HandleOnboardingStatus(w, r)
		return
	}
	if path == "/api/config" {
		s.api.HandleConfig(w, r)
		return
	}
	if path == "/api/models" {
		s.api.HandleModels(w, r)
		return
	}
	if path == "/api/quota/refresh" {
		s.api.HandleQuotaRefresh(w, r)
		return
	}
	if path == "/api/metrics" {
		s.api.HandleMetrics(w, r)
		return
	}
	if path == "/api/events" {
		s.api.HandleEvents(w, r)
		return
	}
	if path == "/oauth/start" || path == "/api/oauth/start" {
		s.api.HandleOAuthStart(w, r)
		return
	}

	// 2. Embedded Web Dashboard Static Files
	s.serveStatic(w, r)
}

// loopbackBound reports whether the server is bound to a loopback address, in which case the
// dashboard/API is intended to be reachable only from this machine.
func (s *Server) loopbackBound() bool {
	addr := strings.TrimSpace(s.cfg.BindAddr)
	if addr == "" {
		return true // default bind is 127.0.0.1
	}
	if ip := net.ParseIP(addr); ip != nil {
		return ip.IsLoopback()
	}
	return addr == "localhost"
}

// isLocalHostHeader reports whether the request Host header refers to a loopback name.
func isLocalHostHeader(host string) bool {
	if host == "" {
		return false
	}
	h := host
	if hostname, _, err := net.SplitHostPort(host); err == nil {
		h = hostname
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]") // strip IPv6 brackets
	if strings.EqualFold(h, "localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// isLocalOrigin reports whether an Origin header points at a loopback host.
func isLocalOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return isLocalHostHeader(u.Host)
}

func (s *Server) isProxyRequest(r *http.Request) bool {
	// Google Cloud Code PA hosts
	if strings.Contains(r.Host, "googleapis.com") {
		return true
	}

	// Standard Cloud Code endpoint paths
	p := r.URL.Path
	if strings.HasPrefix(p, "/v1") || strings.HasPrefix(p, "/v1internal") {
		return true
	}
	if strings.Contains(p, "streamGenerateContent") ||
		strings.Contains(p, "generateContent") ||
		strings.Contains(p, "retrieveUserQuota") {
		return true
	}

	// Non-GET requests that are not API/OAuth endpoints are upstream proxy calls
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if !strings.HasPrefix(p, "/api/") && !strings.HasPrefix(p, "/oauth/") {
			return true
		}
	}

	return false
}

func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	reqPath := strings.TrimPrefix(r.URL.Path, "/")
	reqPath = strings.TrimPrefix(reqPath, "dist/")

	if reqPath == "" || reqPath == "index.html" {
		s.serveEmbeddedFile(w, r, "index.html")
		return
	}

	// Attempt to open the requested file from embedded FS
	f, err := s.distFS.Open(reqPath)
	if err == nil {
		_ = f.Close()
		s.serveEmbeddedFile(w, r, reqPath)
		return
	}

	// Single Page Application fallback: serve index.html
	s.serveEmbeddedFile(w, r, "index.html")
}

func (s *Server) serveEmbeddedFile(w http.ResponseWriter, r *http.Request, filename string) {
	f, err := s.distFS.Open(filename)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	ext := filepath.Ext(filename)
	ctype := mime.TypeByExtension(ext)
	if ctype == "" {
		switch ext {
		case ".html":
			ctype = "text/html; charset=utf-8"
		case ".js":
			ctype = "application/javascript; charset=utf-8"
		case ".css":
			ctype = "text/css; charset=utf-8"
		case ".json":
			ctype = "application/json; charset=utf-8"
		case ".svg":
			ctype = "image/svg+xml"
		default:
			ctype = "application/octet-stream"
		}
	}

	w.Header().Set("Content-Type", ctype)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// Start launches the HTTP server listening on the configured address.
func (s *Server) Start() error {
	s.mu.Lock()
	if s.httpServer != nil {
		s.mu.Unlock()
		return errors.New("server is already running")
	}

	addr := fmt.Sprintf("%s:%d", s.cfg.BindAddr, s.cfg.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	s.listener = listener
	s.addr = listener.Addr().String()

	httpServer := &http.Server{
		Handler:      s,
		ReadTimeout:  s.cfg.ReadTimeout,
		WriteTimeout: s.cfg.WriteTimeout,
	}
	s.httpServer = httpServer
	s.mu.Unlock()

	go func() {
		_ = httpServer.Serve(listener)
	}()

	return nil
}

// Stop gracefully shuts down the HTTP server.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.httpServer == nil {
		s.mu.Unlock()
		return nil
	}
	srv := s.httpServer
	s.httpServer = nil
	s.listener = nil
	s.mu.Unlock()

	return srv.Shutdown(ctx)
}

// Addr returns the bound address string (useful when port 0 is used).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Port returns the bound port number.
func (s *Server) Port() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return s.cfg.Port
	}
	if tcpAddr, ok := s.listener.Addr().(*net.TCPAddr); ok {
		return tcpAddr.Port
	}
	return s.cfg.Port
}
