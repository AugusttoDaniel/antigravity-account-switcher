package onboard

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/adspower"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/oauth"
)

type fakeProfiles struct {
	mu       sync.Mutex
	created  []adspower.CreateProfileRequest
	started  []string
	stopped  []string
	startErr error
}

func (f *fakeProfiles) CreateProfile(_ context.Context, req adspower.CreateProfileRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, req)
	return "profile-1", nil
}

func (f *fakeProfiles) StartBrowser(_ context.Context, id string, _ bool) (*adspower.BrowserStartData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.started = append(f.started, id)
	return &adspower.BrowserStartData{WS: adspower.BrowserWS{Puppeteer: "ws://127.0.0.1:9222/devtools/browser/abc"}, DebugPort: "9222"}, nil
}

func (f *fakeProfiles) StopBrowser(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, id)
	return nil
}

type fakeFlow struct {
	err       error
	proxySeen string
	acc       *domain.Account
}

func (f *fakeFlow) StartLoopbackFlowWithProxy(_ context.Context, opener oauth.BrowserOpener, urlLogger func(string), proxyURL string) (*domain.Account, error) {
	f.proxySeen = proxyURL
	const authURL = "https://accounts.google.com/o/oauth2/v2/auth?state=x"
	if urlLogger != nil {
		urlLogger(authURL)
	}
	if opener != nil {
		_ = opener(authURL)
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.acc, nil
}

type fakeStore struct {
	proxy, profile string
	proxyErr       error
}

func (s *fakeStore) UpdateProxyURL(_ context.Context, _, proxyURL string) error {
	if s.proxyErr != nil {
		return s.proxyErr
	}
	s.proxy = proxyURL
	return nil
}

func (s *fakeStore) UpdateAdsPowerProfileID(_ context.Context, _, profileID string) error {
	s.profile = profileID
	return nil
}

func newAccount() *domain.Account {
	return &domain.Account{ID: "acc-1", Email: "new@gmail.com"}
}

func TestCreateProfile_BindsTheProxyAndEngine(t *testing.T) {
	p := &fakeProfiles{}
	id, err := CreateProfile(context.Background(), p, "ag-1", "cloak", "http://alice:s3cret@proxy.example.com:3128")
	if err != nil || id != "profile-1" {
		t.Fatalf("CreateProfile = %q, %v", id, err)
	}
	req := p.created[0]
	if req.Name != "ag-1" || req.Browser != "cloak" {
		t.Errorf("request = %+v", req)
	}
	if req.ProxyConfig.ProxyHost != "proxy.example.com" || req.ProxyConfig.ProxyUser != "alice" || req.ProxyConfig.ProxyPassword != "s3cret" {
		t.Errorf("proxy config = %+v; the profile must reach Google only through the account's proxy", req.ProxyConfig)
	}
}

func TestStart_RefusesAnUnusableProxyBeforeLaunching(t *testing.T) {
	p := &fakeProfiles{}
	_, err := Start(context.Background(), p, "profile-1", "1.2.3.4:8080:alice:s3cretPW")
	if err == nil {
		t.Fatal("expected an error for a badly formatted proxy")
	}
	if strings.Contains(err.Error(), "s3cretPW") {
		t.Errorf("error leaks the proxy password: %v", err)
	}
	if len(p.started) != 0 {
		t.Errorf("the browser was launched despite the invalid proxy")
	}
}

func TestStart_ReportsALaunchFailure(t *testing.T) {
	p := &fakeProfiles{startErr: errors.New("boom")}
	if _, err := Start(context.Background(), p, "profile-1", "http://proxy.example.com:3128"); err == nil || !strings.Contains(err.Error(), "profile-1") {
		t.Errorf("expected an error naming the profile, got %v", err)
	}
}

func TestRun_NavigatesRecordsAndStopsTheBrowser(t *testing.T) {
	p := &fakeProfiles{}
	const proxy = "http://alice:s3cret@proxy.example.com:3128"
	sess, err := Start(context.Background(), p, "profile-1", proxy)
	if err != nil {
		t.Fatal(err)
	}

	flow := &fakeFlow{acc: newAccount()}
	store := &fakeStore{}
	var navWS, navPort, navURL string
	var logged []string
	acc, err := sess.Run(context.Background(), flow, store, Options{
		Navigate: func(_ context.Context, ws, port, target string) error {
			navWS, navPort, navURL = ws, port, target
			return nil
		},
		URLLogger: func(u string) { logged = append(logged, u) },
	})
	if err != nil || acc == nil {
		t.Fatalf("Run = %v, %v", acc, err)
	}
	if navWS != "ws://127.0.0.1:9222/devtools/browser/abc" || navPort != "9222" || !strings.HasPrefix(navURL, "https://accounts.google.com/") {
		t.Errorf("navigated ws=%q port=%q url=%q; the consent page must open inside the profile's browser", navWS, navPort, navURL)
	}
	if flow.proxySeen != proxy {
		t.Errorf("the code exchange used %q, want the account's proxy", flow.proxySeen)
	}
	if store.proxy != proxy || store.profile != "profile-1" || acc.ProxyURL != proxy {
		t.Errorf("recorded proxy=%q profile=%q acc.ProxyURL=%q", store.proxy, store.profile, acc.ProxyURL)
	}
	if len(logged) != 1 {
		t.Errorf("URL logger called %d times", len(logged))
	}
	if len(p.stopped) != 1 || p.stopped[0] != "profile-1" {
		t.Errorf("browser stopped %v, want exactly once", p.stopped)
	}
}

func TestRun_StopsTheBrowserWhenTheFlowFails(t *testing.T) {
	p := &fakeProfiles{}
	sess, _ := Start(context.Background(), p, "profile-1", "http://proxy.example.com:3128")
	store := &fakeStore{}
	_, err := sess.Run(context.Background(), &fakeFlow{err: errors.New("timed out")}, store, Options{
		Navigate: func(context.Context, string, string, string) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "OAuth authentication failed") {
		t.Errorf("error = %v", err)
	}
	if len(p.stopped) != 1 {
		t.Errorf("browser stopped %d times after a failed flow, want 1 (it must not stay open)", len(p.stopped))
	}
	if store.proxy != "" || store.profile != "" {
		t.Errorf("a failed flow recorded proxy=%q profile=%q", store.proxy, store.profile)
	}
}

func TestClose_IsIdempotent(t *testing.T) {
	p := &fakeProfiles{}
	sess, _ := Start(context.Background(), p, "profile-1", "")
	sess.Close()
	sess.Close()
	_, _ = sess.Run(context.Background(), &fakeFlow{acc: newAccount()}, &fakeStore{}, Options{
		Navigate: func(context.Context, string, string, string) error { return nil },
	})
	if len(p.stopped) != 1 {
		t.Errorf("browser stopped %d times, want 1", len(p.stopped))
	}
}

// An account that was added but whose proxy could not be saved must not be reported as a clean
// success: it would silently run without the isolation it was onboarded for.
func TestRun_ReportsAProxyThatCouldNotBeSaved(t *testing.T) {
	p := &fakeProfiles{}
	sess, _ := Start(context.Background(), p, "profile-1", "http://proxy.example.com:3128")
	acc, err := sess.Run(context.Background(), &fakeFlow{acc: newAccount()}, &fakeStore{proxyErr: errors.New("disk full")}, Options{
		Navigate: func(context.Context, string, string, string) error { return nil },
	})
	if err == nil || acc == nil {
		t.Fatalf("Run = %v, %v; want the account together with an error", acc, err)
	}
	if !strings.Contains(err.Error(), "proxy could not be saved") {
		t.Errorf("error = %v", err)
	}
}
