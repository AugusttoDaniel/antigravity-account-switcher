package web

// Adding an account inside an isolated browser profile from the dashboard: AliasMode (or ADS Power)
// creates the profile bound to the account's proxy, its browser opens the Google consent page, and
// the account is saved with its proxy and profile. The human signs in inside that window.
//
// The account's proxy credentials are sent to the profile API in the profile's proxy settings, so
// the API must run on this machine.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/adspower"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/config"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/egress"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/onboard"
)

const (
	// aliasModeDefaultURL is where AliasMode serves its ADS Power-compatible Local API.
	aliasModeDefaultURL = "http://127.0.0.1:50400"
	// defaultProfileEngine is AliasMode's Chromium engine, the one that exposes the CDP endpoint the
	// browser is driven through.
	defaultProfileEngine = "cloak"
	profilePingTimeout   = 3 * time.Second
	profilePrepareBudget = 60 * time.Second
)

// profileAPI is the browser-profile API: what onboarding needs plus a liveness check.
type profileAPI interface {
	onboard.Profiles
	Ping(ctx context.Context) error
	ListProfiles(ctx context.Context, page, pageSize int) ([]adspower.Profile, error)
}

func newProfileAPI(baseURL, apiKey string) profileAPI {
	return adspower.NewClient(adspower.WithBaseURL(baseURL), adspower.WithAPIKey(apiKey))
}

// profileSettings returns the configured profile API URL, key and engine (URL and key may be empty).
func (a *APIHandler) profileSettings() (apiURL, apiKey, engine string) {
	a.cfgMu.RLock()
	cfg := a.appConfig
	if cfg != nil {
		apiURL, apiKey, engine = cfg.AdsPowerAPIURL, cfg.AdsPowerAPIKey, cfg.AdsPowerEngine
	}
	a.cfgMu.RUnlock()
	if cfg == nil {
		if loaded, err := config.Load(); err == nil && loaded != nil {
			apiURL, apiKey, engine = loaded.AdsPowerAPIURL, loaded.AdsPowerAPIKey, loaded.AdsPowerEngine
		}
	}
	apiURL, apiKey, engine = strings.TrimSpace(apiURL), strings.TrimSpace(apiKey), strings.TrimSpace(engine)
	if engine == "" {
		engine = defaultProfileEngine
	}
	return apiURL, apiKey, engine
}

// validateProfileAPIURL requires the profile API to be on this machine: the account's proxy
// credentials are sent to it.
func validateProfileAPIURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("not a valid URL (e.g. http://127.0.0.1:50400)")
	}
	host := u.Hostname()
	if egress.IsLoopbackHost(host) || strings.EqualFold(host, "local.adspower.net") {
		return nil
	}
	return fmt.Errorf("the profile API must run on this machine (got host %q): the account's proxy credentials are sent to it", host)
}

// profileConnection is a reachable profile API.
type profileConnection struct {
	api    profileAPI
	url    string
	engine string
}

// connectProfileAPI finds a profile API that answers: the configured one, or (when none is
// configured) AliasMode's default address and then ADS Power's. It returns the addresses tried.
func (a *APIHandler) connectProfileAPI(ctx context.Context) (*profileConnection, []string, error) {
	configured, key, engine := a.profileSettings()
	candidates := []string{aliasModeDefaultURL, adspower.DefaultBaseURL}
	if configured != "" {
		if err := validateProfileAPIURL(configured); err != nil {
			return nil, []string{configured}, fmt.Errorf("adspower_api_url: %w", err)
		}
		candidates = []string{configured}
	}
	factory := a.profileAPIFactory
	if factory == nil {
		factory = newProfileAPI
	}

	var lastErr error
	for _, candidate := range candidates {
		api := factory(candidate, key)
		pingCtx, cancel := context.WithTimeout(ctx, profilePingTimeout)
		err := api.Ping(pingCtx)
		cancel()
		if err == nil {
			return &profileConnection{api: api, url: candidate, engine: engine}, candidates, nil
		}
		lastErr = err
	}
	return nil, candidates, fmt.Errorf("no browser-profile API answered (%s): start AliasMode with its Local API enabled, or set adspower_api_url: %w",
		strings.Join(candidates, ", "), lastErr)
}

// HandleOnboardingStatus serves GET /api/onboarding/status: whether adding an account through an
// isolated browser profile is available right now.
func (a *APIHandler) HandleOnboardingStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	conn, tried, err := a.connectProfileAPI(r.Context())
	resp := map[string]any{"available": conn != nil, "tried": tried}
	if conn != nil {
		resp["url"], resp["engine"] = conn.url, conn.engine
	} else if err != nil {
		resp["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// scrubProxyCredentials removes the proxy's username and password from err's text: the profile API
// receives them in the request body and some errors quote what they were sent.
func scrubProxyCredentials(err error, proxyURL string) error {
	u, perr := url.Parse(proxyURL)
	if err == nil || perr != nil || u.User == nil {
		return err
	}
	msg := err.Error()
	if pass, ok := u.User.Password(); ok && pass != "" {
		msg = strings.ReplaceAll(msg, pass, "***")
	}
	if user := u.User.Username(); user != "" {
		msg = strings.ReplaceAll(msg, user, "***")
	}
	return errors.New(msg)
}

// startProfileOnboarding creates (or reuses) the profile, opens its browser and completes the
// sign-in in the background. Creating the profile and launching the browser happen before the
// response, so a profile API problem reaches the user instead of a flow that never starts.
func (a *APIHandler) startProfileOnboarding(w http.ResponseWriter, r *http.Request, req oauthStartRequest, proxyURL, maskedProxy string) {
	conn, _, err := a.connectProfileAPI(r.Context())
	if err != nil {
		writeErrorJSON(w, http.StatusBadGateway, "the browser-profile API is not available", scrubProxyCredentials(err, proxyURL))
		return
	}

	prepCtx, cancel := context.WithTimeout(r.Context(), profilePrepareBudget)
	defer cancel()

	profileID := strings.TrimSpace(req.ProfileID)
	if profileID == "" {
		name := "ag-account-" + time.Now().UTC().Format("20060102-150405")
		profileID, err = onboard.CreateProfile(prepCtx, conn.api, name, conn.engine, proxyURL)
		if err != nil {
			writeErrorJSON(w, http.StatusBadGateway, "could not create the browser profile", scrubProxyCredentials(err, proxyURL))
			return
		}
	}
	sess, err := onboard.Start(prepCtx, conn.api, profileID, proxyURL)
	if err != nil {
		writeErrorJSON(w, http.StatusBadGateway, "could not open the profile's browser", scrubProxyCredentials(err, proxyURL))
		return
	}

	urlChan := make(chan string, 1)
	go func() {
		acc, err := sess.Run(context.Background(), a.oauthEngine, a.accountRepo, onboard.Options{
			Navigate: a.profileNavigate,
			URLLogger: func(authURL string) {
				select {
				case urlChan <- authURL:
				default:
				}
				a.broadcastOAuth("oauth_started", "", fmt.Sprintf("Profile %s opened through %s: sign in inside its window", profileID, maskedProxy))
			},
			Warn: func(msg string) { a.broadcastOAuthError(msg) },
		})
		a.finishOAuth(acc, err, maskedProxy)
	}()

	var generatedAuthURL string
	select {
	case generatedAuthURL = <-urlChan:
	case <-time.After(2 * time.Second):
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "started",
		"mode":       "profile",
		"profile_id": profileID,
		"proxy":      maskedProxy,
		"auth_url":   generatedAuthURL,
	})
}

// finishOAuth reports how a background sign-in ended.
func (a *APIHandler) finishOAuth(acc *domain.Account, err error, maskedProxy string) {
	if err != nil {
		a.broadcastOAuthError(fmt.Sprintf("OAuth flow failed: %v", err))
		return
	}
	if acc != nil {
		a.broadcastOAuth("oauth_completed", acc.ID, fmt.Sprintf("Account %s added through %s", acc.Email, maskedProxy))
	}
}

func (a *APIHandler) broadcastOAuth(kind, accountID, message string) {
	if a.broadcaster == nil {
		return
	}
	a.broadcaster.Broadcast(&domain.ProxyEvent{
		Type:      domain.EventType(kind),
		AccountID: accountID,
		Message:   message,
		Timestamp: time.Now().UTC(),
	})
}

func (a *APIHandler) broadcastOAuthError(message string) {
	if a.broadcaster == nil {
		return
	}
	a.broadcaster.Broadcast(&domain.ProxyEvent{
		Type:      domain.EventTypeError,
		Message:   message,
		Timestamp: time.Now().UTC(),
	})
}
