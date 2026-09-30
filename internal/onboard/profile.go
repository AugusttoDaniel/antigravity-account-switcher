// Package onboard adds a Google account through an isolated browser profile (ADS Power, or the
// AliasMode-compatible API): the profile's browser is launched, driven to the Google consent page,
// the loopback OAuth flow is completed through the account's proxy, and the proxy and profile are
// recorded on the account. The human signs in inside the profile window; nothing here stores
// credentials.
//
// It is shared by the add-account-adspower command and the dashboard, which need the same steps
// with different front ends.
package onboard

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/adspower"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/egress"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/oauth"
)

// stopTimeout bounds closing the profile's browser once the flow is over.
const stopTimeout = 15 * time.Second

// Profiles is the part of the browser-profile API onboarding uses; *adspower.Client implements it.
type Profiles interface {
	CreateProfile(ctx context.Context, req adspower.CreateProfileRequest) (string, error)
	StartBrowser(ctx context.Context, userID string, headless bool) (*adspower.BrowserStartData, error)
	StopBrowser(ctx context.Context, userID string) error
}

// Flow is the OAuth loopback flow; *oauth.OAuthService implements it.
type Flow interface {
	StartLoopbackFlowWithProxy(ctx context.Context, opener oauth.BrowserOpener, urlLogger func(string), proxyURL string) (*domain.Account, error)
}

// Store persists what onboarding binds to the account. A store that can also remember the profile
// an account was added through implements ProfileRecorder.
type Store interface {
	UpdateProxyURL(ctx context.Context, id, proxyURL string) error
}

// ProfileRecorder is implemented by stores that keep the profile id of an account.
type ProfileRecorder interface {
	UpdateAdsPowerProfileID(ctx context.Context, id, profileID string) error
}

// Navigator drives the profile's browser to a URL (adspower.Navigate in production).
type Navigator func(ctx context.Context, browserWSURL, debugPort, targetURL string) error

// Options tunes Run.
type Options struct {
	// Navigate opens the consent page in the profile's browser; it defaults to adspower.Navigate.
	Navigate Navigator
	// URLLogger receives the sign-in URL, in case the browser does not navigate on its own.
	URLLogger func(string)
	// Warn receives non-fatal problems (e.g. the profile id could not be remembered).
	Warn func(string)
}

// CreateProfile creates a profile of the given engine bound to proxyURL, so the browser reaches
// Google only through it.
func CreateProfile(ctx context.Context, p Profiles, name, engine, proxyURL string) (string, error) {
	pc, err := adspower.ProxyConfigFromURL(proxyURL)
	if err != nil {
		return "", fmt.Errorf("build the profile's proxy: %w", err)
	}
	return p.CreateProfile(ctx, adspower.CreateProfileRequest{Name: name, Browser: engine, ProxyConfig: pc})
}

// Session is a profile whose browser is running and ready for the sign-in.
type Session struct {
	// ProfileID is the profile the browser was launched from.
	ProfileID string

	profiles Profiles
	proxyURL string
	started  *adspower.BrowserStartData
	stopOnce sync.Once
}

// Start launches the profile's browser. proxyURL is what the code exchange will use (empty for a
// profile whose own proxy is enough); an unusable one is refused before anything is launched.
func Start(ctx context.Context, p Profiles, profileID, proxyURL string) (*Session, error) {
	if err := egress.ValidateProxyURL(proxyURL); err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	started, err := p.StartBrowser(ctx, profileID, false)
	if err != nil {
		return nil, fmt.Errorf("start the browser of profile %s: %w", profileID, err)
	}
	return &Session{ProfileID: profileID, profiles: p, proxyURL: proxyURL, started: started}, nil
}

// Close stops the profile's browser. It is safe to call more than once.
func (s *Session) Close() {
	s.stopOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()
		_ = s.profiles.StopBrowser(ctx, s.ProfileID)
	})
}

// Opener returns the function that drives the profile's browser to a consent URL, for flows other
// than the Google one Run handles (e.g. the Codex login). navigate defaults to adspower.Navigate.
// The caller closes the session.
func (s *Session) Opener(ctx context.Context, navigate Navigator) func(authURL string) error {
	if navigate == nil {
		navigate = adspower.Navigate
	}
	return func(authURL string) error {
		return navigate(ctx, s.started.WS.Puppeteer, s.started.DebugPort, authURL)
	}
}

// Run drives the browser to the consent page, completes the OAuth flow through the session's proxy
// and records the proxy and profile on the account. The browser is always stopped before it
// returns. If the account was added but its proxy could not be saved, the account is returned
// together with the error.
func (s *Session) Run(ctx context.Context, flow Flow, store Store, opts Options) (*domain.Account, error) {
	defer s.Close()

	acc, err := flow.StartLoopbackFlowWithProxy(ctx, s.Opener(ctx, opts.Navigate), opts.URLLogger, s.proxyURL)
	if err != nil {
		return nil, fmt.Errorf("OAuth authentication failed: %w", err)
	}

	if s.proxyURL != "" && acc.ProxyURL != s.proxyURL {
		if err := store.UpdateProxyURL(ctx, acc.ID, s.proxyURL); err != nil {
			return acc, fmt.Errorf("account %s was added but its proxy could not be saved: %w", acc.Email, err)
		}
		acc.ProxyURL = s.proxyURL
	}
	if rec, ok := store.(ProfileRecorder); ok {
		if err := rec.UpdateAdsPowerProfileID(ctx, acc.ID, s.ProfileID); err != nil && opts.Warn != nil {
			opts.Warn(fmt.Sprintf("could not record profile %s for %s: %v", s.ProfileID, acc.Email, err))
		}
	}
	return acc, nil
}
