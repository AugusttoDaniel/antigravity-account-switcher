package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/adspower"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/config"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/oauth"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/proxypool"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/store/sqlite"
)

// runAddAccountAdsPower onboards a Google account through an isolated ADS Power browser profile.
//
// Assisted flow: the switcher chooses/creates the profile and its proxy, launches the profile's
// browser, drives it to the Google consent URL, and the human completes sign-in inside that
// isolated window. The consent, the code exchange and every later token refresh all egress through
// the profile's proxy, so the operator's real IP and fingerprint never touch the account.
func runAddAccountAdsPower(args []string) {
	fs := flag.NewFlagSet("add-account-adspower", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "Path to SQLite database file")
	email := fs.String("email", "", "Known Google account email (lets the switcher reuse the account's existing proxy/profile on re-auth)")
	proxyOverride := fs.String("proxy", "", "Force a specific outbound proxy URL, bypassing the pool/reuse policy")
	profileName := fs.String("profile-name", "", "Name for the ADS Power profile to create (default derived from the email/time)")
	apiURL := fs.String("api-url", adspower.DefaultBaseURL, "ADS Power Local API base URL")
	apiKey := fs.String("api-key", "", "ADS Power Local API key (if enabled in ADS Power settings)")
	_ = fs.Parse(args)

	db, err := sqlite.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening SQLite database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	cfg, _ := config.Load()
	if cfg == nil {
		cfg = config.DefaultConfig()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	accRepo := sqlite.NewAccountRepository(db)
	oauthService := oauth.NewOAuthService(accRepo)
	ads := adspower.NewClient(adspower.WithBaseURL(*apiURL), adspower.WithAPIKey(*apiKey))

	// Look up any existing account for this email to apply the reuse policy.
	var existingProxy, existingProfileID string
	if e := strings.TrimSpace(*email); e != "" {
		if acc, gErr := accRepo.GetByEmail(ctx, e); gErr == nil && acc != nil {
			existingProxy = acc.ProxyURL
			existingProfileID = acc.AdsPowerProfileID
		}
	}

	// 1. Decide the proxy: explicit override > existing account's proxy > a never-used pool proxy.
	proxyURL, err := decideProxy(ctx, *proxyOverride, existingProxy, cfg.Proxies, accRepo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error selecting proxy: %v\n", err)
		os.Exit(1)
	}

	// 2. Decide the ADS Power profile: reuse the account's profile, else create one bound to proxy.
	profileID := existingProfileID
	if profileID == "" {
		name := strings.TrimSpace(*profileName)
		if name == "" {
			name = deriveProfileName(*email)
		}
		pc, pErr := adspower.ProxyConfigFromURL(proxyURL)
		if pErr != nil {
			fmt.Fprintf(os.Stderr, "Error building profile proxy config: %v\n", pErr)
			os.Exit(1)
		}
		fmt.Printf("Creating ADS Power profile %q bound to proxy %s...\n", name, maskProxy(proxyURL))
		profileID, err = ads.CreateProfile(ctx, adspower.CreateProfileRequest{Name: name, ProxyConfig: pc})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating ADS Power profile: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Created profile: %s\n", profileID)
	} else {
		fmt.Printf("Reusing existing ADS Power profile: %s\n", profileID)
	}

	// 3. Launch the profile browser (visible, so the human can sign in).
	fmt.Println("Launching isolated profile browser...")
	started, err := ads.StartBrowser(ctx, profileID, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error starting ADS Power browser: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer stopCancel()
		if sErr := ads.StopBrowser(stopCtx, profileID); sErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to stop ADS Power browser %s: %v\n", profileID, sErr)
		}
	}()

	// 4. Opener that drives the isolated browser to the consent URL.
	opener := func(authURL string) error {
		return adspower.Navigate(ctx, started.WS.Puppeteer, started.DebugPort, authURL)
	}

	fmt.Println("Complete the Google sign-in inside the ADS Power window. Waiting for authorization...")

	// 5. Run the loopback flow; consent + code exchange egress through the profile's proxy.
	acc, err := oauthService.StartLoopbackFlowWithProxy(ctx, opener, func(authURL string) {
		fmt.Printf("\nIf the profile browser did not navigate automatically, open this URL inside it:\n\n%s\n\n", authURL)
	}, proxyURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nOAuth authentication failed: %v\n", err)
		os.Exit(1)
	}

	// 6. Persist the proxy + profile binding on the account.
	if proxyURL != "" && acc.ProxyURL != proxyURL {
		_ = accRepo.UpdateProxyURL(ctx, acc.ID, proxyURL)
		acc.ProxyURL = proxyURL
	}
	if err := accRepo.UpdateAdsPowerProfileID(ctx, acc.ID, profileID); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to record ADS Power profile id: %v\n", err)
	}

	if e := strings.TrimSpace(*email); e != "" && !strings.EqualFold(e, acc.Email) {
		fmt.Fprintf(os.Stderr, "Note: signed-in account (%s) differs from --email (%s).\n", acc.Email, e)
	}

	fmt.Printf("\nSuccess! Google account %s onboarded through ADS Power.\n", acc.Email)
	fmt.Printf("Account ID:       %s\n", acc.ID)
	fmt.Printf("ADS Power profile: %s\n", profileID)
	fmt.Printf("Outbound proxy:   %s\n", maskProxy(proxyURL))
}

// decideProxy applies the allocation policy: explicit override wins, then an existing account's
// proxy is reused, otherwise a never-used proxy is drawn from the configured pool.
func decideProxy(ctx context.Context, override, existing string, pool []string, repo *sqlite.AccountRepository) (string, error) {
	if o := strings.TrimSpace(override); o != "" {
		return o, nil
	}
	if e := strings.TrimSpace(existing); e != "" {
		return e, nil
	}
	allocated, err := proxypool.NewStaticPool(pool, repo).Allocate(ctx)
	if err != nil {
		if errors.Is(err, proxypool.ErrEmpty) {
			return "", fmt.Errorf("%w (add a \"proxies\" list to the config, or pass --proxy)", err)
		}
		return "", err
	}
	return allocated, nil
}

// deriveProfileName produces a stable-ish profile name from the email (or the time when unknown).
func deriveProfileName(email string) string {
	if e := strings.TrimSpace(email); e != "" {
		return "ag-" + e
	}
	return "ag-account-" + time.Now().UTC().Format("20060102-150405")
}

// maskProxy hides credentials in a proxy URL for display.
func maskProxy(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "(direct)"
	}
	at := strings.LastIndex(raw, "@")
	scheme := strings.Index(raw, "://")
	if at > 0 && scheme > 0 && at > scheme {
		return raw[:scheme+3] + "***@" + raw[at+1:]
	}
	return raw
}
