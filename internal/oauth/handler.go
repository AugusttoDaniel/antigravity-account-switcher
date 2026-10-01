package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/egress"
	"github.com/google/uuid"
)

const (
	// Default Google OAuth2 endpoints
	DefaultGoogleAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	DefaultGoogleTokenURL    = "https://oauth2.googleapis.com/token"
	DefaultGoogleUserInfoURL = "https://www.googleapis.com/oauth2/v3/userinfo"

	DefaultStateTTL = 5 * time.Minute
	// InteractiveTimeout bounds a sign-in a human completes by hand (2-step verification, passkey
	// or phone prompts routinely take longer than the 5-minute default). The flow timeout and the
	// state TTL must move together: an expired state rejects the callback even if the flow waits.
	InteractiveTimeout = 15 * time.Minute
	DefaultFlowTimeout = 5 * time.Minute
	DefaultHTTPTimeout = 15 * time.Second
)

// ResolveCredentials returns the first discovered (client_id, client_secret) pair.
// It is kept for backward compatibility; callers that need to disambiguate between
// several candidates should use ResolveCredentialCandidates instead.
func ResolveCredentials() (string, string) {
	ids, secrets := ResolveCredentialCandidates()
	if len(ids) == 0 || len(secrets) == 0 {
		return "", ""
	}
	return ids[0], secrets[0]
}

// ResolveCredentialCandidates discovers every plausible Google OAuth client_id and
// client_secret on the local machine.
//
// The installed Antigravity binary embeds more than one client_id and more than one
// client_secret, and their byte offsets do not reveal which secret belongs to which id.
// Guessing a pairing yields "invalid_client" at token exchange, so all candidates are returned
// and the correct client_secret is probed against Google at runtime by ExchangeCodeVia and
// RefreshTokenVia (which retry on an "invalid_client" response and cache the secret that works).
//
// Precedence, highest first:
//  1. Environment variables ANTIGRAVITY_CLIENT_ID / ANTIGRAVITY_CLIENT_SECRET. When set,
//     that dimension is authoritative and its candidate list is reduced to the env value.
//  2. An existing Antigravity ACP token file.
//  3. The installed Antigravity 2.0 binary/bundle.
func ResolveCredentialCandidates() (ids []string, secrets []string) {
	return resolveCredentialCandidates(true)
}

// ResolveAllClientIDs lists every client id found on this machine, ignoring a pinned
// ANTIGRAVITY_CLIENT_ID: pinning chooses the client for NEW sign-ins, but an account issued by another
// client still has to be renewed with that one.
func ResolveAllClientIDs() []string {
	ids, _ := resolveCredentialCandidates(false)
	return ids
}

func resolveCredentialCandidates(honorEnvPin bool) (ids []string, secrets []string) {
	envID := os.Getenv("ANTIGRAVITY_CLIENT_ID")
	envSec := os.Getenv("ANTIGRAVITY_CLIENT_SECRET")

	seenID := map[string]bool{}
	seenSec := map[string]bool{}
	addID := func(v string) {
		if v != "" && !seenID[v] {
			seenID[v] = true
			ids = append(ids, v)
		}
	}
	addSec := func(v string) {
		if v != "" && !seenSec[v] {
			seenSec[v] = true
			secrets = append(secrets, v)
		}
	}

	addID(envID)
	addSec(envSec)

	if fileID, fileSec := discoverFromTokenFile(); fileID != "" && fileSec != "" {
		addID(fileID)
		addSec(fileSec)
	}

	bundleIDs, bundleSecs := discoverFromIDEBundle()
	for _, v := range bundleIDs {
		addID(v)
	}
	for _, v := range bundleSecs {
		addSec(v)
	}

	// A pinned environment value overrides everything else for its dimension.
	if envID != "" && honorEnvPin {
		ids = []string{envID}
	}
	if envSec != "" && honorEnvPin {
		secrets = []string{envSec}
	}

	return ids, secrets
}

func discoverFromTokenFile() (string, string) {
	p := FindExistingACPTokenFile()
	if p == "" {
		return "", ""
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", ""
	}
	var f ACPTokenFile
	if err := json.Unmarshal(data, &f); err == nil && f.ClientID != "" && f.ClientSecret != "" {
		return f.ClientID, f.ClientSecret
	}
	return "", ""
}

var (
	bundleScanOnce sync.Once
	bundleScanIDs  []string
	bundleScanSecs []string
)

// discoverFromIDEBundle returns all client_ids and client_secrets embedded in the
// installed Antigravity bundle. The scan reads a large binary (hundreds of MB) and is
// therefore memoized for the lifetime of the process.
func discoverFromIDEBundle() ([]string, []string) {
	bundleScanOnce.Do(func() {
		bundleScanIDs, bundleScanSecs = scanBundleForCredentials()
	})
	return bundleScanIDs, bundleScanSecs
}

func scanBundleForCredentials() ([]string, []string) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	candidates := []string{
		// Windows Antigravity paths
		filepath.Join(home, "AppData", "Local", "Programs", "Antigravity", "resources", "bin", "language_server.exe"),
		filepath.Join(home, "AppData", "Local", "Programs", "antigravity", "resources", "bin", "language_server.exe"),
		filepath.Join(home, "AppData", "Local", "Programs", "Antigravity", "resources", "app", "out", "main.js"),
		filepath.Join(home, "AppData", "Roaming", "Antigravity", "resources", "bin", "language_server.exe"),
		`C:\Program Files\Antigravity\resources\bin\language_server.exe`,
		`C:\Program Files\antigravity\resources\bin\language_server.exe`,
		`C:\Program Files (x86)\Antigravity\resources\bin\language_server.exe`,
		// Linux/XDG Antigravity paths
		filepath.Join(home, ".local", "share", "antigravity", "resources", "bin", "language_server"),
		filepath.Join(home, ".local", "share", "antigravity", "Antigravity-x64", "resources", "bin", "language_server"),
		filepath.Join(home, "tools", "Antigravity", "Antigravity-x64", "resources", "bin", "language_server"),
		filepath.Join(home, "tools", "Antigravity", "resources", "bin", "language_server"),
		"/opt/antigravity/resources/bin/language_server",
		"/opt/Antigravity/resources/bin/language_server",
		"/opt/antigravity/Antigravity-x64/resources/bin/language_server",
		// Preview bundle main.js paths
		filepath.Join(home, ".local", "share", "antigravity-ide", "resources", "app", "out", "main.js"),
		"/opt/Antigravity/resources/app/out/main.js",
		"/usr/share/antigravity-ide/resources/app/out/main.js",
		"/Applications/Antigravity.app/Contents/Resources/app/out/main.js",
	}

	reID := regexp.MustCompile(`(?i)(\d+-[a-z0-9_-]+\.apps\.googleusercontent\.com)`)
	prefix := string([]byte{0x47, 0x4f, 0x43, 0x53, 0x50, 0x58, 0x2d}) // native client secret prefix bytes ("GOCSPX-")
	reSec := regexp.MustCompile(regexp.QuoteMeta(prefix) + `[A-Za-z0-9_-]{28}`)

	var ids, secs []string
	seenID := map[string]bool{}
	seenSec := map[string]bool{}

	for _, c := range candidates {
		data, err := os.ReadFile(c)
		if err != nil {
			continue
		}
		for _, m := range reID.FindAll(data, -1) {
			if s := string(m); !seenID[s] {
				seenID[s] = true
				ids = append(ids, s)
			}
		}
		for _, m := range reSec.FindAll(data, -1) {
			if s := string(m); !seenSec[s] {
				seenSec[s] = true
				secs = append(secs, s)
			}
		}
		// Stop at the first bundle that yields at least one id and one secret.
		if len(ids) > 0 && len(secs) > 0 {
			break
		}
	}
	return ids, secs
}

// DefaultScopes defines the OAuth2 scopes required by Antigravity.
var DefaultScopes = []string{
	"openid",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/cloud-platform",
}

// Config holds configuration parameters for the OAuth2 subsystem.
type Config struct {
	ClientID         string
	ClientSecret     string
	IDCandidates     []string
	SecretCandidates []string
	AuthURL          string
	TokenURL         string
	UserInfoURL      string
	Scopes           []string
	HTTPClient       *http.Client
	StateTTL         time.Duration
	FlowTimeout      time.Duration
}

// Option modifies Config.
type Option func(*Config)

// WithClientID pins the client_id, reducing the candidate list to that single value.
func WithClientID(id string) Option {
	return func(c *Config) {
		c.ClientID = id
		if id != "" {
			c.IDCandidates = []string{id}
		}
	}
}

// WithClientSecret pins the client_secret, reducing the candidate list to that single value.
func WithClientSecret(secret string) Option {
	return func(c *Config) {
		c.ClientSecret = secret
		if secret != "" {
			c.SecretCandidates = []string{secret}
		}
	}
}

// WithCredentialCandidates supplies explicit candidate lists to probe.
func WithCredentialCandidates(ids, secrets []string) Option {
	return func(c *Config) {
		c.IDCandidates = ids
		c.SecretCandidates = secrets
	}
}

func WithAuthURL(url string) Option {
	return func(c *Config) { c.AuthURL = url }
}

func WithTokenURL(url string) Option {
	return func(c *Config) { c.TokenURL = url }
}

func WithUserInfoURL(url string) Option {
	return func(c *Config) { c.UserInfoURL = url }
}

func WithScopes(scopes []string) Option {
	return func(c *Config) { c.Scopes = scopes }
}

func WithHTTPClient(client *http.Client) Option {
	return func(c *Config) { c.HTTPClient = client }
}

func WithStateTTL(ttl time.Duration) Option {
	return func(c *Config) { c.StateTTL = ttl }
}

func WithFlowTimeout(timeout time.Duration) Option {
	return func(c *Config) { c.FlowTimeout = timeout }
}

// PKCEPair holds code_verifier and code_challenge generated per RFC 7636.
type PKCEPair struct {
	Verifier  string
	Challenge string
	Method    string // "S256"
}

// GeneratePKCE creates an RFC 7636 S256 code verifier and challenge.
func GeneratePKCE() (*PKCEPair, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("failed to generate random bytes for code verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(b)
	hash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(hash[:])
	return &PKCEPair{
		Verifier:  verifier,
		Challenge: challenge,
		Method:    "S256",
	}, nil
}

// GenerateState creates a high-entropy CSRF state token.
func GenerateState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate random bytes for state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// PendingAuth stores state and code_verifier during an in-flight authorization flow.
type PendingAuth struct {
	State        string
	CodeVerifier string
	RedirectURI  string
	ProxyURL     string // Outbound proxy to egress the code exchange and userinfo through, if any.
	CreatedAt    time.Time
}

// StateStore is a thread-safe in-memory store for pending authorizations with single-use eviction.
type StateStore struct {
	mu      sync.Mutex
	entries map[string]*PendingAuth
	ttl     time.Duration
}

// NewStateStore creates a new StateStore with the given TTL.
func NewStateStore(ttl time.Duration) *StateStore {
	if ttl <= 0 {
		ttl = DefaultStateTTL
	}
	return &StateStore{
		entries: make(map[string]*PendingAuth),
		ttl:     ttl,
	}
}

// Put adds a pending authorization to the store.
func (s *StateStore) Put(auth *PendingAuth) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked()
	s.entries[auth.State] = auth
}

// GetAndRemove validates and atomically evicts the state to prevent replay attacks.
func (s *StateStore) GetAndRemove(state string) (*PendingAuth, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked()

	auth, ok := s.entries[state]
	if !ok {
		return nil, false
	}
	delete(s.entries, state)
	if time.Since(auth.CreatedAt) > s.ttl {
		return nil, false
	}
	return auth, true
}

func (s *StateStore) cleanupLocked() {
	now := time.Now()
	for k, v := range s.entries {
		if now.Sub(v.CreatedAt) > s.ttl {
			delete(s.entries, k)
		}
	}
}

// TokenResponse represents credentials returned by Google token endpoint.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"` // seconds
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	Error        string `json:"error,omitempty"`
	ErrorDesc    string `json:"error_description,omitempty"`
	// ClientID is the OAuth client that issued this response (set by a refresh; not part of the wire format).
	ClientID string `json:"-"`
}

// UserInfoResponse represents Google OAuth2 userinfo payload.
type UserInfoResponse struct {
	ID            string `json:"id"`
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	VerifiedEmail bool   `json:"verified_email"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
}

// BrowserOpener abstracts launching the web browser.
type BrowserOpener func(url string) error

// DefaultBrowserOpener opens the URL in the operating system's default browser.
func DefaultBrowserOpener(targetURL string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.Command("xdg-open", targetURL)
	case "darwin":
		cmd = exec.Command("open", targetURL)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", targetURL)
	default:
		return fmt.Errorf("unsupported operating system: %s", runtime.GOOS)
	}
	return cmd.Start()
}

// OAuthEngine defines the full interface for the OAuth2 subsystem.
type OAuthEngine interface {
	StartLoopbackFlow(ctx context.Context, opener BrowserOpener, urlLogger func(string)) (*domain.Account, error)
	StartLoopbackFlowWithProxy(ctx context.Context, opener BrowserOpener, urlLogger func(string), proxyURL string) (*domain.Account, error)
	BuildAuthURL(redirectURI, state, codeChallenge string) string
	HandleCallbackRequest(r *http.Request) (*domain.Account, error)
	RefreshToken(ctx context.Context, refreshToken string) (*TokenResponse, error)
	RefreshTokenVia(ctx context.Context, refreshToken, proxyURL string) (*TokenResponse, error)
	EnsureValidToken(ctx context.Context, acc *domain.Account, safetyMargin time.Duration) (*domain.Account, error)
}

// OAuthService coordinates the loopback authentication flow, token refreshing, and persistence.
type OAuthService struct {
	cfg         Config
	accountRepo domain.AccountRepository
	stateStore  *StateStore
	client      *http.Client

	// proxyClients caches one *http.Client per distinct outbound proxy URL so that every
	// OAuth exchange (code exchange, userinfo, token refresh) can egress through the same
	// proxy as the account's data-plane traffic. Without this, token exchange and background
	// refresh leak the operator's real IP even when the account has a proxy configured.
	proxyMu      sync.RWMutex
	proxyClients map[string]*http.Client

	// cachedSecret holds the client_secret the pairing probe (ExchangeCodeVia / RefreshTokenVia)
	// discovered at runtime. It is guarded by secretMu because concurrent onboarding and background
	// refresh calls share one OAuthService, so the cfg is never mutated after construction.
	secretMu     sync.RWMutex
	cachedSecret string

	// idCandidates and secCandidates hold every client_id / client_secret discovered on the
	// machine. The installed binary can embed more than one of each and their byte offsets do
	// not reveal the correct pairing, so when more than one candidate exists the pairing is
	// ambiguous and the user must pin it explicitly (see errAmbiguousCredentials).
	idCandidates  []string
	secCandidates []string
	// allClientIDs is every client id on this machine, ignoring a pinned ANTIGRAVITY_CLIENT_ID: an account
	// issued by another client is renewed with that one.
	allClientIDs []string
}

// NewOAuthService constructs a new OAuthService.
func NewOAuthService(accountRepo domain.AccountRepository, opts ...Option) *OAuthService {
	cfg := Config{
		AuthURL:     DefaultGoogleAuthURL,
		TokenURL:    DefaultGoogleTokenURL,
		UserInfoURL: DefaultGoogleUserInfoURL,
		Scopes:      DefaultScopes,
		StateTTL:    DefaultStateTTL,
		FlowTimeout: DefaultFlowTimeout,
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	// Build the candidate lists: explicit options win, otherwise discover from the machine.
	ids := cfg.IDCandidates
	secs := cfg.SecretCandidates
	if len(ids) == 0 || len(secs) == 0 {
		discoveredIDs, discoveredSecs := ResolveCredentialCandidates()
		if len(ids) == 0 {
			ids = discoveredIDs
		}
		if len(secs) == 0 {
			secs = discoveredSecs
		}
	}

	// Mock fallback for local/loopback token endpoints used in tests.
	isLocal := isLoopbackTokenEndpoint(cfg.TokenURL) || isLoopbackTokenEndpoint(cfg.AuthURL)
	if len(ids) == 0 && isLocal {
		ids = []string{"test-mock-client-id"}
	}
	if len(secs) == 0 && isLocal {
		secs = []string{"test-mock-client-secret"}
	}

	if cfg.ClientID == "" && len(ids) > 0 {
		cfg.ClientID = ids[0]
	}
	if cfg.ClientSecret == "" && len(secs) > 0 {
		cfg.ClientSecret = secs[0]
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: DefaultHTTPTimeout,
		}
	}

	allIDs := append([]string(nil), ids...)
	if len(cfg.IDCandidates) == 0 {
		allIDs = append(allIDs, ResolveAllClientIDs()...)
	}

	return &OAuthService{
		cfg:           cfg,
		accountRepo:   accountRepo,
		stateStore:    NewStateStore(cfg.StateTTL),
		client:        client,
		proxyClients:  make(map[string]*http.Client),
		idCandidates:  ids,
		secCandidates: secs,
		allClientIDs:  allIDs,
	}
}

// clientForProxy returns an *http.Client whose transport egresses through proxyURL. An empty
// proxyURL yields the default direct client; an invalid one yields a fail-closed client that
// refuses every request, never the direct client. This mirrors the data-plane proxy handler so
// OAuth egress and API egress stay consistent. Clients are cached per distinct proxy URL for the
// lifetime of the service.
func (s *OAuthService) clientForProxy(proxyURL string) *http.Client {
	proxyStr := strings.TrimSpace(proxyURL)
	if proxyStr == "" {
		return s.client
	}

	s.proxyMu.RLock()
	if c, ok := s.proxyClients[proxyStr]; ok {
		s.proxyMu.RUnlock()
		return c
	}
	s.proxyMu.RUnlock()

	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	if c, ok := s.proxyClients[proxyStr]; ok {
		return c
	}

	timeout := DefaultHTTPTimeout
	if s.client != nil && s.client.Timeout > 0 {
		timeout = s.client.Timeout
	}

	var c *http.Client
	if parsedProxy, err := egress.ParseProxyURL(proxyStr); err != nil {
		// Never fall back to the direct client: a token exchange or refresh from the operator's
		// real IP links the account to it. The fail-closed client makes the call error out instead.
		c = egress.FailClosedClient(err)
	} else {
		c = &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy:               http.ProxyURL(parsedProxy),
				MaxIdleConns:        50,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	}

	if s.proxyClients == nil {
		s.proxyClients = make(map[string]*http.Client)
	}
	s.proxyClients[proxyStr] = c
	return c
}

func isLoopbackTokenEndpoint(u string) bool {
	return strings.Contains(u, "127.0.0.1") || strings.Contains(u, "localhost") || strings.Contains(u, "[::1]")
}

// CredentialsAmbiguous reports whether more than one client_id or client_secret was
// discovered, meaning the correct pairing cannot be determined automatically and the user
// must pin it via ANTIGRAVITY_CLIENT_ID / ANTIGRAVITY_CLIENT_SECRET.
func (s *OAuthService) CredentialsAmbiguous() bool {
	return len(s.idCandidates) > 1 || len(s.secCandidates) > 1
}

// CredentialCandidateCounts returns how many distinct client_ids and client_secrets were
// discovered, for diagnostics and user-facing guidance.
func (s *OAuthService) CredentialCandidateCounts() (ids int, secrets int) {
	return len(s.idCandidates), len(s.secCandidates)
}

// BuildAuthURL constructs the Google authorization URL with PKCE and CSRF state parameters.
func (s *OAuthService) BuildAuthURL(redirectURI, state, codeChallenge string) string {
	q := url.Values{}
	q.Set("client_id", s.cfg.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", strings.Join(s.cfg.Scopes, " "))
	q.Set("access_type", "offline")
	q.Set("prompt", "consent")
	q.Set("state", state)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")

	return fmt.Sprintf("%s?%s", s.cfg.AuthURL, q.Encode())
}

// StartLoopbackFlow initiates an ephemeral local listener on 127.0.0.1:0, generates PKCE & state,
// launches the browser, exchanges code for credentials on callback, and returns the saved account.
// The code exchange and userinfo lookup egress directly (operator IP).
func (s *OAuthService) StartLoopbackFlow(ctx context.Context, opener BrowserOpener, urlLogger func(string)) (*domain.Account, error) {
	return s.StartLoopbackFlowWithProxy(ctx, opener, urlLogger, "")
}

// StartLoopbackFlowWithProxy behaves like StartLoopbackFlow but routes the server-to-server code
// exchange and userinfo lookup through proxyURL, so Google observes the proxy IP rather than the
// operator's real IP for those calls. Note: the interactive consent screen still loads in the
// browser opened by opener, which is not proxied here; isolating the browser itself is the job of
// the antidetect-browser integration.
func (s *OAuthService) StartLoopbackFlowWithProxy(ctx context.Context, opener BrowserOpener, urlLogger func(string), proxyURL string) (*domain.Account, error) {
	// Reject an unusable proxy before the consent screen: otherwise the single-use authorization
	// code is spent and only the exchange fails.
	if err := egress.ValidateProxyURL(proxyURL); err != nil {
		return nil, err
	}
	if s.cfg.ClientID == "" || s.cfg.ClientSecret == "" {
		return nil, errors.New("google oauth client credentials not found; please ensure Antigravity 2.0 is installed or set ANTIGRAVITY_CLIENT_ID and ANTIGRAVITY_CLIENT_SECRET")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("failed to bind loopback listener on 127.0.0.1:0: %w", err)
	}
	defer listener.Close()

	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/oauth/callback", port)

	pkce, err := GeneratePKCE()
	if err != nil {
		return nil, err
	}

	state, err := GenerateState()
	if err != nil {
		return nil, err
	}

	s.stateStore.Put(&PendingAuth{
		State:        state,
		CodeVerifier: pkce.Verifier,
		RedirectURI:  redirectURI,
		ProxyURL:     strings.TrimSpace(proxyURL),
		CreatedAt:    time.Now(),
	})

	authURL := s.BuildAuthURL(redirectURI, state, pkce.Challenge)
	if urlLogger != nil {
		urlLogger(authURL)
	}

	type authResult struct {
		account *domain.Account
		err     error
	}
	resultChan := make(chan authResult, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/callback", func(w http.ResponseWriter, r *http.Request) {
		acc, err := s.HandleCallbackRequest(r)
		if err != nil {
			s.renderErrorHTML(w, err.Error())
			select {
			case resultChan <- authResult{err: err}:
			default:
			}
			return
		}
		s.renderSuccessHTML(w, acc.Email)
		select {
		case resultChan <- authResult{account: acc}:
		default:
		}
	})

	server := &http.Server{
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	}

	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			select {
			case resultChan <- authResult{err: serveErr}:
			default:
			}
		}
	}()

	// Launch browser (or fallback to manual copy in headless)
	if opener == nil {
		opener = DefaultBrowserOpener
	}
	if openErr := opener(authURL); openErr != nil {
		// Log warning but proceed: user can copy URL in headless environments
		fmt.Printf("Notice: Could not automatically open browser (%v).\nPlease open this URL in your browser:\n%s\n", openErr, authURL)
	}

	flowTimeout := s.cfg.FlowTimeout
	if flowTimeout <= 0 {
		flowTimeout = DefaultFlowTimeout
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, flowTimeout)
	defer cancel()

	select {
	case res := <-resultChan:
		shutdownCtx, sCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer sCancel()
		_ = server.Shutdown(shutdownCtx)
		return res.account, res.err

	case <-timeoutCtx.Done():
		shutdownCtx, sCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer sCancel()
		_ = server.Shutdown(shutdownCtx)
		return nil, errors.New("OAuth2 loopback authorization timed out or was cancelled")
	}
}

// HandleCallbackRequest processes a callback HTTP request, validates CSRF state,
// exchanges code for tokens, retrieves userinfo, and upserts the account into SQLite.
func (s *OAuthService) HandleCallbackRequest(r *http.Request) (*domain.Account, error) {
	ctx := r.Context()

	// Check if upstream returned an error (e.g. access_denied)
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		errDesc := r.URL.Query().Get("error_description")
		return nil, fmt.Errorf("OAuth error from provider: %s (%s)", errParam, errDesc)
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		return nil, errors.New("missing authorization code in callback")
	}

	state := r.URL.Query().Get("state")
	if state == "" {
		return nil, errors.New("missing state parameter in callback")
	}

	pending, ok := s.stateStore.GetAndRemove(state)
	if !ok || pending == nil {
		return nil, errors.New("invalid, expired, or already consumed OAuth state parameter")
	}

	tokenResp, err := s.ExchangeCodeVia(ctx, code, pending.CodeVerifier, pending.RedirectURI, pending.ProxyURL)
	if err != nil {
		return nil, fmt.Errorf("token exchange failed: %w", err)
	}

	userInfo, err := s.FetchUserInfoVia(ctx, tokenResp.AccessToken, pending.ProxyURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch userinfo: %w", err)
	}

	if userInfo.Email == "" {
		return nil, errors.New("userInfo did not contain an email address")
	}

	expiry := time.Now().UTC().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	account, err := s.UpsertAccount(ctx, userInfo.Email, tokenResp.AccessToken, tokenResp.RefreshToken, expiry)
	if err != nil {
		return nil, fmt.Errorf("failed to store authenticated account: %w", err)
	}

	return account, nil
}

// ExchangeCode exchanges an authorization code and PKCE verifier for OAuth2 credentials,
// egressing through the operator's direct connection.
func (s *OAuthService) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI string) (*TokenResponse, error) {
	return s.ExchangeCodeVia(ctx, code, codeVerifier, redirectURI, "")
}

// ExchangeCodeVia is ExchangeCode but egresses through proxyURL when non-empty.
//
// The installed Antigravity bundle can embed several client_secrets, and their byte offsets do not
// reveal which one pairs with the client_id used in the authorization request. Rather than guess,
// this tries each discovered secret in turn: an "invalid_client" response is a client-authentication
// failure that Google rejects before redeeming the authorization code, so the code survives the
// probe and the next candidate can be tried. The secret that works is cached on the service for
// subsequent refreshes.
func (s *OAuthService) ExchangeCodeVia(ctx context.Context, code, codeVerifier, redirectURI, proxyURL string) (*TokenResponse, error) {
	if s.cfg.ClientID == "" {
		return nil, errors.New("google oauth client credentials not found; please ensure Antigravity 2.0 is installed or set ANTIGRAVITY_CLIENT_ID and ANTIGRAVITY_CLIENT_SECRET")
	}
	secrets := s.candidateSecrets()
	if len(secrets) == 0 {
		return nil, errors.New("google oauth client credentials not found; please ensure Antigravity 2.0 is installed or set ANTIGRAVITY_CLIENT_ID and ANTIGRAVITY_CLIENT_SECRET")
	}

	var lastErr error
	for _, secret := range secrets {
		tokenResp, invalidClient, err := s.exchangeOnce(ctx, code, codeVerifier, redirectURI, secret, proxyURL)
		if err == nil {
			s.rememberSecret(secret) // remember the working pair for refreshes
			return tokenResp, nil
		}
		lastErr = err
		if invalidClient {
			continue // wrong secret for this client_id; try the next candidate
		}
		return nil, err // a different failure (e.g. invalid_grant) — the credential pairing is fine
	}
	return nil, lastErr
}

// candidateSecrets returns every discovered client_secret to try, with the currently-configured
// one first so a known-good pairing is not re-probed.
func (s *OAuthService) candidateSecrets() []string {
	s.secretMu.RLock()
	cached := s.cachedSecret
	s.secretMu.RUnlock()

	seen := map[string]bool{}
	out := make([]string, 0, len(s.secCandidates)+2)
	add := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	add(cached)             // the probe's last known-good secret, tried first
	add(s.cfg.ClientSecret) // cfg is write-once at construction, so this read is race-free
	for _, sec := range s.secCandidates {
		add(sec)
	}
	return out
}

// rememberSecret caches the client_secret that the pairing probe found to work, so subsequent
// exchanges and refreshes try it first. Guarded because the OAuthService is shared across goroutines.
func (s *OAuthService) rememberSecret(secret string) {
	s.secretMu.Lock()
	s.cachedSecret = secret
	s.secretMu.Unlock()
}

// exchangeOnce performs a single authorization-code exchange with the given client_secret. It
// reports whether a failure was an "invalid_client" client-authentication error, meaning the secret
// is wrong for this client_id and another candidate should be tried.
func (s *OAuthService) exchangeOnce(ctx context.Context, code, codeVerifier, redirectURI, clientSecret, proxyURL string) (*TokenResponse, bool, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {s.cfg.ClientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"code_verifier": {codeVerifier},
		"redirect_uri":  {redirectURI},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, false, fmt.Errorf("failed to create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.clientForProxy(proxyURL).Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("token HTTP exchange failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("failed to read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, isInvalidClient(bodyBytes), fmt.Errorf("token request returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var tokenResp TokenResponse
	if err := json.Unmarshal(bodyBytes, &tokenResp); err != nil {
		return nil, false, fmt.Errorf("failed to decode token response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return nil, false, errors.New("received empty access_token from token endpoint")
	}
	return &tokenResp, false, nil
}

// isInvalidClient reports whether a token-endpoint error body is an OAuth2 "invalid_client" error.
func isInvalidClient(body []byte) bool {
	var errResp struct {
		Error string `json:"error"`
	}
	return json.Unmarshal(body, &errResp) == nil && errResp.Error == "invalid_client"
}

// isUnauthorizedClient reports Google's answer when the client is real but did not issue the token: the
// refresh token is bound to another client.
func isUnauthorizedClient(body []byte) bool {
	var errResp struct {
		Error string `json:"error"`
	}
	return json.Unmarshal(body, &errResp) == nil && errResp.Error == "unauthorized_client"
}

// FetchUserInfo queries the userinfo endpoint to obtain the primary email address,
// egressing through the operator's direct connection.
func (s *OAuthService) FetchUserInfo(ctx context.Context, accessToken string) (*UserInfoResponse, error) {
	return s.FetchUserInfoVia(ctx, accessToken, "")
}

// FetchUserInfoVia is FetchUserInfo but egresses through proxyURL when non-empty.
func (s *OAuthService) FetchUserInfoVia(ctx context.Context, accessToken, proxyURL string) (*UserInfoResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.UserInfoURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create userinfo request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := s.clientForProxy(proxyURL).Do(req)
	if err != nil {
		return nil, fmt.Errorf("userinfo HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("userinfo request returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var info UserInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("failed to decode userinfo response: %w", err)
	}

	return &info, nil
}

// UpsertAccount updates an existing account or creates a new account in SQLite.
func (s *OAuthService) UpsertAccount(ctx context.Context, email, accessToken, refreshToken string, expiry time.Time) (*domain.Account, error) {
	if s.accountRepo == nil {
		return nil, errors.New("account repository is nil")
	}

	// 1. Check if account already exists
	existing, err := s.accountRepo.GetByEmail(ctx, email)
	if err == nil && existing != nil {
		if err := s.accountRepo.UpdateToken(ctx, existing.ID, accessToken, expiry); err != nil {
			return nil, fmt.Errorf("failed to update access token: %w", err)
		}
		if refreshToken != "" {
			if err := s.accountRepo.UpdateRefreshToken(ctx, existing.ID, refreshToken); err != nil {
				return nil, fmt.Errorf("failed to update refresh token: %w", err)
			}
			existing.RefreshToken = refreshToken
		}
		// The sign-in that just ran issued these tokens through the configured client.
		if s.cfg.ClientID != "" && existing.OAuthClientID != s.cfg.ClientID && refreshToken != "" {
			s.recordClientID(ctx, existing.ID, s.cfg.ClientID)
			existing.OAuthClientID = s.cfg.ClientID
		}
		if existing.Status != domain.AccountStatusActive {
			_ = s.accountRepo.UpdateStatus(ctx, existing.ID, domain.AccountStatusActive)
			existing.Status = domain.AccountStatusActive
		}
		existing.AccessToken = accessToken
		existing.TokenExpiry = expiry

		// Ensure an active account exists
		if active, actErr := s.accountRepo.GetActive(ctx); actErr != nil || active == nil {
			_ = s.accountRepo.SetActive(ctx, existing.ID)
			existing.IsActive = true
		}
		return existing, nil
	}

	if !errors.Is(err, domain.ErrAccountNotFound) {
		return nil, fmt.Errorf("database query error: %w", err)
	}

	// 2. Account does not exist: create new account
	if refreshToken == "" {
		return nil, errors.New("cannot create new account without offline refresh_token")
	}

	hasActive := true
	if active, actErr := s.accountRepo.GetActive(ctx); actErr != nil || active == nil {
		hasActive = false
	}

	newAcc := &domain.Account{
		ID:            uuid.NewString(),
		Email:         email,
		RefreshToken:  refreshToken,
		AccessToken:   accessToken,
		TokenExpiry:   expiry,
		OAuthClientID: s.cfg.ClientID,
		IsActive:      false,
		Status:        domain.AccountStatusActive,
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}

	if err := s.accountRepo.Create(ctx, newAcc); err != nil {
		return nil, fmt.Errorf("failed to persist new account: %w", err)
	}

	if !hasActive {
		if err := s.accountRepo.SetActive(ctx, newAcc.ID); err != nil {
			return nil, fmt.Errorf("failed to set active account: %w", err)
		}
		newAcc.IsActive = true
	}

	return newAcc, nil
}

// RefreshToken exchanges a refresh token for fresh access credentials with Google,
// egressing through the operator's direct connection.
func (s *OAuthService) RefreshToken(ctx context.Context, refreshToken string) (*TokenResponse, error) {
	return s.RefreshTokenVia(ctx, refreshToken, "")
}

// RefreshTokenVia is RefreshToken but egresses through proxyURL when non-empty. Background token
// renewal (proxy handler, quota poller) passes the account's proxy so refreshes never leak the
// operator's real IP for an account that is otherwise fully proxied.
func (s *OAuthService) RefreshTokenVia(ctx context.Context, refreshToken, proxyURL string) (*TokenResponse, error) {
	if refreshToken == "" {
		return nil, errors.New("empty refresh token")
	}
	if s.cfg.ClientID == "" {
		return nil, errors.New("google oauth client credentials not found; please ensure Antigravity 2.0 is installed or set ANTIGRAVITY_CLIENT_ID and ANTIGRAVITY_CLIENT_SECRET")
	}
	secrets := s.candidateSecrets()
	if len(secrets) == 0 {
		return nil, errors.New("google oauth client credentials not found; please ensure Antigravity 2.0 is installed or set ANTIGRAVITY_CLIENT_ID and ANTIGRAVITY_CLIENT_SECRET")
	}

	// Google binds a refresh token to the OAuth client that issued it, so the renewal must use that same
	// client. An account that remembers its client uses it, and only it. One that does not (it predates
	// the binding, or was imported) is tried against every client found on this machine, and the one that
	// works is recorded: that is how a token issued by another Antigravity client keeps renewing.
	acc := s.accountForRefreshToken(ctx, refreshToken)
	bound := acc != nil && acc.OAuthClientID != ""
	clientIDs := s.learnClientIDs()
	if bound {
		clientIDs = []string{acc.OAuthClientID}
	}

	var lastErr error
	for _, clientID := range clientIDs {
		tokenResp, mismatch, unpaired, err := s.refreshWithClient(ctx, clientID, refreshToken, secrets, proxyURL)
		if err == nil {
			tokenResp.ClientID = clientID
			if acc != nil && acc.OAuthClientID != clientID {
				s.recordClientID(ctx, acc.ID, clientID)
			}
			return tokenResp, nil
		}
		lastErr = err
		if (mismatch || unpaired) && !bound {
			continue // not this client's token (or no secret pairs with it): try the next one
		}
		return nil, err // invalid_grant (revoked token), a bound client that refuses, or a transport error
	}
	return nil, lastErr
}

// refreshWithClient renews a token with one client id, probing the candidate secrets like
// ExchangeCodeVia does on "invalid_client". mismatch reports that Google says this client did not issue
// the token (unauthorized_client); unpaired that none of the secrets pair with the client id, which
// settles nothing about the token.
func (s *OAuthService) refreshWithClient(ctx context.Context, clientID, refreshToken string, secrets []string, proxyURL string) (resp *TokenResponse, mismatch, unpaired bool, err error) {
	var lastErr error
	sawInvalidClient := false
	for _, secret := range secrets {
		tokenResp, invalidClient, unauthorized, rerr := s.refreshOnce(ctx, clientID, refreshToken, secret, proxyURL)
		if rerr == nil {
			s.rememberSecret(secret)
			return tokenResp, false, false, nil
		}
		lastErr = rerr
		switch {
		case invalidClient:
			sawInvalidClient = true
			continue // wrong secret for this client id: try the next candidate
		case unauthorized:
			return nil, true, false, rerr
		}
		return nil, false, false, rerr // invalid_grant (revoked token) or a transport error: do not keep probing
	}
	return nil, false, sawInvalidClient, lastErr
}

// learnClientIDs lists the clients an account of unknown origin may have been issued by: the
// configured one first, then every other found on this machine.
func (s *OAuthService) learnClientIDs() []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	add(s.cfg.ClientID)
	for _, id := range s.allClientIDs {
		add(id)
	}
	return out
}

// accountForRefreshToken finds the stored account a refresh token belongs to, or nil.
func (s *OAuthService) accountForRefreshToken(ctx context.Context, refreshToken string) *domain.Account {
	if s.accountRepo == nil {
		return nil
	}
	accs, err := s.accountRepo.List(ctx)
	if err != nil {
		return nil
	}
	for _, a := range accs {
		if a != nil && a.RefreshToken == refreshToken {
			return a
		}
	}
	return nil
}

// recordClientID remembers which client an account's token belongs to. Best effort: the renewal
// already worked, and the next one would simply learn it again.
func (s *OAuthService) recordClientID(ctx context.Context, accountID, clientID string) {
	if rec, ok := s.accountRepo.(interface {
		UpdateOAuthClientID(ctx context.Context, id, clientID string) error
	}); ok {
		_ = rec.UpdateOAuthClientID(ctx, accountID, clientID)
	}
}

// ProbeResult is what a trial renewal says about a token and a client.
type ProbeResult int

const (
	// ProbeOK: the client can renew the token.
	ProbeOK ProbeResult = iota
	// ProbeMismatch: Google says the token was issued by a different client.
	ProbeMismatch
	// ProbeRevoked: the token itself was revoked or expired (invalid_grant).
	ProbeRevoked
	// ProbeInconclusive: no answer that settles it (network, or no secret pairs with the client id).
	ProbeInconclusive
)

// ProbeRefresh tries to renew refreshToken with a given client id, through proxyURL, and says what
// Google answered. It is how a hand-over to a service that renews with its own client (OmniRoute) is
// checked before it breaks. The new access token is discarded.
func (s *OAuthService) ProbeRefresh(ctx context.Context, refreshToken, clientID, proxyURL string) ProbeResult {
	if refreshToken == "" || clientID == "" {
		return ProbeInconclusive
	}
	_, mismatch, _, err := s.refreshWithClient(ctx, clientID, refreshToken, s.candidateSecrets(), proxyURL)
	switch {
	case err == nil:
		return ProbeOK
	case errors.Is(err, domain.ErrInvalidRefreshToken):
		return ProbeRevoked
	case mismatch:
		return ProbeMismatch
	}
	return ProbeInconclusive
}

// refreshOnce performs a single refresh-token grant with the given client id and secret, reporting
// whether a failure was "invalid_client" (the secret does not pair with the id: try the next one) or
// "unauthorized_client" (the id is real but did not issue the token).
func (s *OAuthService) refreshOnce(ctx context.Context, clientID, refreshToken, clientSecret, proxyURL string) (*TokenResponse, bool, bool, error) {
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, false, false, fmt.Errorf("failed to create refresh token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.clientForProxy(proxyURL).Do(req)
	if err != nil {
		return nil, false, false, fmt.Errorf("token refresh HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, false, fmt.Errorf("failed to read token refresh response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		if isInvalidGrant(bodyBytes) {
			return nil, false, false, domain.ErrInvalidRefreshToken
		}
		return nil, isInvalidClient(bodyBytes), isUnauthorizedClient(bodyBytes), fmt.Errorf("token refresh rejected with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var tokenResp TokenResponse
	if err := json.Unmarshal(bodyBytes, &tokenResp); err != nil {
		return nil, false, false, fmt.Errorf("failed to decode token refresh response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return nil, false, false, errors.New("received empty access_token on refresh")
	}
	return &tokenResp, false, false, nil
}

// isInvalidGrant reports whether a token-endpoint error body is an OAuth2 "invalid_grant" error.
func isInvalidGrant(body []byte) bool {
	var errResp struct {
		Error string `json:"error"`
	}
	return json.Unmarshal(body, &errResp) == nil && errResp.Error == "invalid_grant"
}

// EnsureValidToken checks if an account's token is valid. If expiring, refreshes it and updates SQLite.
func (s *OAuthService) EnsureValidToken(ctx context.Context, acc *domain.Account, safetyMargin time.Duration) (*domain.Account, error) {
	if acc == nil {
		return nil, domain.ErrAccountNotFound
	}

	if !acc.IsTokenExpired(safetyMargin) {
		return acc, nil
	}

	tokenResp, err := s.RefreshTokenVia(ctx, acc.RefreshToken, acc.ProxyURL)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidRefreshToken) {
			_ = s.accountRepo.UpdateStatus(ctx, acc.ID, domain.AccountStatusError)
		}
		return nil, fmt.Errorf("failed to refresh token for account %s: %w", acc.Email, err)
	}

	newExpiry := time.Now().UTC().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	if err := s.accountRepo.UpdateToken(ctx, acc.ID, tokenResp.AccessToken, newExpiry); err != nil {
		return nil, fmt.Errorf("failed to update access token in database: %w", err)
	}

	acc.AccessToken = tokenResp.AccessToken
	acc.TokenExpiry = newExpiry

	if tokenResp.RefreshToken != "" && tokenResp.RefreshToken != acc.RefreshToken {
		_ = s.accountRepo.UpdateRefreshToken(ctx, acc.ID, tokenResp.RefreshToken)
		acc.RefreshToken = tokenResp.RefreshToken
	}

	return acc, nil
}

var successTemplate = template.Must(template.New("success").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>Authentication Successful — Antigravity Account Switcher</title>
  <style>
    body {
      background-color: #090d16;
      color: #f8fafc;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      display: flex;
      align-items: center;
      justify-content: center;
      height: 100vh;
      margin: 0;
    }
    .card {
      background: #131b2e;
      border: 1px solid #1e293b;
      border-radius: 12px;
      padding: 32px 40px;
      text-align: center;
      max-width: 440px;
      box-shadow: 0 10px 25px -5px rgba(0, 0, 0, 0.5);
    }
    .icon {
      width: 56px;
      height: 56px;
      background: rgba(16, 185, 129, 0.15);
      border-radius: 50%;
      display: flex;
      align-items: center;
      justify-content: center;
      margin: 0 auto 20px;
      color: #10b981;
    }
    h2 { margin: 0 0 10px; font-size: 20px; font-weight: 600; }
    p { margin: 0 0 16px; color: #94a3b8; font-size: 14px; line-height: 1.5; }
    .email { color: #38bdf8; font-weight: 500; }
    .footer { font-size: 12px; color: #64748b; margin-top: 24px; }
  </style>
</head>
<body>
  <div class="card">
    <div class="icon">
      <svg width="28" height="28" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
        <polyline points="20 6 9 17 4 12"></polyline>
      </svg>
    </div>
    <h2>Account Connected!</h2>
    <p>Successfully authenticated Google account <br><span class="email">{{.Email}}</span></p>
    <p>You can close this tab and return to Antigravity 2.0 or your terminal.</p>
    <div class="footer">This window will close automatically.</div>
  </div>
  <script>
    if (window.opener) {
      window.opener.postMessage({ type: "oauth_success", email: "{{.Email}}" }, "*");
      setTimeout(() => window.close(), 2500);
    }
  </script>
</body>
</html>`))

var errorTemplate = template.Must(template.New("error").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>Authentication Failed — Antigravity Account Switcher</title>
  <style>
    body {
      background-color: #090d16;
      color: #f8fafc;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      display: flex;
      align-items: center;
      justify-content: center;
      height: 100vh;
      margin: 0;
    }
    .card {
      background: #131b2e;
      border: 1px solid #1e293b;
      border-radius: 12px;
      padding: 32px 40px;
      text-align: center;
      max-width: 440px;
      box-shadow: 0 10px 25px -5px rgba(0, 0, 0, 0.5);
    }
    .icon {
      width: 56px;
      height: 56px;
      background: rgba(244, 63, 94, 0.15);
      border-radius: 50%;
      display: flex;
      align-items: center;
      justify-content: center;
      margin: 0 auto 20px;
      color: #f43f5e;
    }
    h2 { margin: 0 0 10px; font-size: 20px; font-weight: 600; }
    p { margin: 0 0 16px; color: #94a3b8; font-size: 14px; line-height: 1.5; }
    .err-msg { color: #fb7185; font-family: monospace; font-size: 12px; background: rgba(0,0,0,0.3); padding: 8px; border-radius: 6px; }
  </style>
</head>
<body>
  <div class="card">
    <div class="icon">
      <svg width="28" height="28" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
        <line x1="18" y1="6" x2="6" y2="18"></line>
        <line x1="6" y1="6" x2="18" y2="18"></line>
      </svg>
    </div>
    <h2>Authentication Failed</h2>
    <p>An error occurred while connecting your Google account:</p>
    <div class="err-msg">{{.ErrorMessage}}</div>
    <p style="margin-top: 16px;">Please return to the application and try again.</p>
  </div>
  <script>
    if (window.opener) {
      window.opener.postMessage({ type: "oauth_error", error: "{{.ErrorMessage}}" }, "*");
    }
  </script>
</body>
</html>`))

func (s *OAuthService) renderSuccessHTML(w http.ResponseWriter, email string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = successTemplate.Execute(w, struct{ Email string }{Email: email})
}

func (s *OAuthService) renderErrorHTML(w http.ResponseWriter, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_ = errorTemplate.Execute(w, struct{ ErrorMessage string }{ErrorMessage: errMsg})
}
