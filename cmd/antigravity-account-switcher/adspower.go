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
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
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
	profileFlag := fs.String("profile", "", "Use an existing ADS Power profile id directly (skips profile creation; works on the free plan)")
	profileName := fs.String("profile-name", "", "Name for the ADS Power profile to create (default derived from the email/time)")
	apiURL := fs.String("api-url", adspower.DefaultBaseURL, "ADS Power Local API base URL")
	apiKey := fs.String("api-key", "", "ADS Power Local API key (if enabled in ADS Power settings)")
	timeout := fs.Duration("timeout", 10*time.Minute, "How long to wait for the assisted Google sign-in to complete")
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
	oauthService := oauth.NewOAuthService(accRepo, oauth.WithFlowTimeout(*timeout))
	ads := adspower.NewClient(adspower.WithBaseURL(*apiURL), adspower.WithAPIKey(*apiKey))

	// Look up any existing account for this email to apply the reuse policy.
	var existingProxy, existingProfileID string
	if e := strings.TrimSpace(*email); e != "" {
		if acc, gErr := accRepo.GetByEmail(ctx, e); gErr == nil && acc != nil {
			existingProxy = acc.ProxyURL
			existingProfileID = acc.AdsPowerProfileID
		}
	}

	explicitProfile := strings.TrimSpace(*profileFlag)

	// 1. Decide the proxy.
	var proxyURL string
	if explicitProfile != "" {
		// Free-plan path: an existing profile already owns its proxy inside ADS Power, so we do not
		// draw from the pool. Route the server-side code exchange through --proxy (or the account's
		// stored proxy) so it egresses from the same IP as the profile browser.
		proxyURL = strings.TrimSpace(*proxyOverride)
		if proxyURL == "" {
			proxyURL = existingProxy
		}
		if proxyURL == "" {
			fmt.Fprintln(os.Stderr, "Warning: no --proxy and no stored proxy; the OAuth code exchange will egress directly (the profile browser still uses its own proxy). Pass --proxy to match the profile's proxy.")
		}
	} else {
		// explicit override > existing account's proxy > a never-used pool proxy.
		proxyURL, err = decideProxy(ctx, *proxyOverride, existingProxy, cfg.Proxies, accRepo)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error selecting proxy: %v\n", err)
			os.Exit(1)
		}
	}

	// 2. Decide the ADS Power profile: explicit --profile > the account's stored profile > create one.
	profileID := explicitProfile
	if profileID == "" {
		profileID = existingProfileID
	}
	switch {
	case explicitProfile != "":
		fmt.Printf("Using ADS Power profile: %s\n", profileID)
	case profileID != "":
		fmt.Printf("Reusing existing ADS Power profile: %s\n", profileID)
	default:
		// Create a new profile (requires a paid ADS Power plan).
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
	}

	// 3. Launch the profile, drive it to consent, complete the flow, and persist the bindings.
	fmt.Println("Launching isolated profile browser. Complete the Google sign-in inside it. Waiting for authorization...")
	acc, err := onboardViaProfile(ctx, ads, oauthService, accRepo, profileID, proxyURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n%v\n", err)
		os.Exit(1)
	}

	if e := strings.TrimSpace(*email); e != "" && !strings.EqualFold(e, acc.Email) {
		fmt.Fprintf(os.Stderr, "Note: signed-in account (%s) differs from --email (%s).\n", acc.Email, e)
	}

	fmt.Printf("\nSuccess! Google account %s onboarded through ADS Power.\n", acc.Email)
	fmt.Printf("Account ID:       %s\n", acc.ID)
	fmt.Printf("ADS Power profile: %s\n", profileID)
	fmt.Printf("Outbound proxy:   %s\n", maskProxy(proxyURL))
}

// runImportAdsPower onboards every ADS Power profile (or those matching --filter) as an account,
// one at a time. Each account reuses its own profile's existing proxy (import semantics), and the
// human completes the Google sign-in in each profile window in turn.
func runImportAdsPower(args []string) {
	fs := flag.NewFlagSet("import-adspower", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "Path to SQLite database file")
	apiURL := fs.String("api-url", adspower.DefaultBaseURL, "ADS Power Local API base URL")
	apiKey := fs.String("api-key", "", "ADS Power Local API key (if enabled in ADS Power settings)")
	filter := fs.String("filter", "", "Only import profiles whose name contains this substring (case-insensitive)")
	all := fs.Bool("all", false, "Confirm importing every matching profile (required)")
	_ = fs.Parse(args)

	if !*all {
		fmt.Println("Refusing to import without --all. Re-run with --all to onboard every matching ADS Power profile.")
		os.Exit(1)
	}

	db, err := sqlite.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening SQLite database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	accRepo := sqlite.NewAccountRepository(db)
	oauthService := oauth.NewOAuthService(accRepo)
	ads := adspower.NewClient(adspower.WithBaseURL(*apiURL), adspower.WithAPIKey(*apiKey))

	profiles, err := listAllProfiles(ctx, ads)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing ADS Power profiles: %v\n", err)
		os.Exit(1)
	}
	profiles = filterProfiles(profiles, *filter)
	if len(profiles) == 0 {
		fmt.Println("No matching ADS Power profiles to import.")
		return
	}

	fmt.Printf("Importing %d ADS Power profile(s). You will sign in to each account in turn.\n", len(profiles))

	var onboarded, failed int
	for i, p := range profiles {
		if ctx.Err() != nil {
			fmt.Println("Interrupted; stopping.")
			break
		}
		proxyURL := p.ProxyConfig.URL()
		fmt.Printf("\n[%d/%d] Profile %q (%s) proxy=%s\n", i+1, len(profiles), p.Name, p.UserID, maskProxy(proxyURL))

		acc, err := onboardViaProfile(ctx, ads, oauthService, accRepo, p.UserID, proxyURL)
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  failed: %v\n", err)
			continue
		}
		onboarded++
		fmt.Printf("  onboarded %s (proxy %s)\n", acc.Email, maskProxy(acc.ProxyURL))
	}

	fmt.Printf("\nDone. %d onboarded, %d failed, of %d profile(s).\n", onboarded, failed, len(profiles))
}

// onboardViaProfile launches an ADS Power profile, drives it to the Google consent URL, completes
// the loopback OAuth flow through proxyURL, and records the proxy + profile binding on the account.
// The profile browser is stopped before returning. Shared by the single and batch commands.
func onboardViaProfile(ctx context.Context, ads *adspower.Client, oauthService *oauth.OAuthService, accRepo *sqlite.AccountRepository, profileID, proxyURL string) (*domain.Account, error) {
	started, err := ads.StartBrowser(ctx, profileID, false)
	if err != nil {
		return nil, fmt.Errorf("start ADS Power browser %s: %w", profileID, err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer stopCancel()
		if sErr := ads.StopBrowser(stopCtx, profileID); sErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to stop ADS Power browser %s: %v\n", profileID, sErr)
		}
	}()

	opener := func(authURL string) error {
		return adspower.Navigate(ctx, started.WS.Puppeteer, started.DebugPort, authURL)
	}

	acc, err := oauthService.StartLoopbackFlowWithProxy(ctx, opener, func(authURL string) {
		fmt.Printf("\nIf the profile browser did not navigate automatically, open this URL inside it:\n\n%s\n\n", authURL)
	}, proxyURL)
	if err != nil {
		return nil, fmt.Errorf("OAuth authentication failed: %w", err)
	}

	if proxyURL != "" && acc.ProxyURL != proxyURL {
		_ = accRepo.UpdateProxyURL(ctx, acc.ID, proxyURL)
		acc.ProxyURL = proxyURL
	}
	if err := accRepo.UpdateAdsPowerProfileID(ctx, acc.ID, profileID); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to record ADS Power profile id for %s: %v\n", acc.Email, err)
	}
	return acc, nil
}

// listAllProfiles pages through every ADS Power profile.
func listAllProfiles(ctx context.Context, ads *adspower.Client) ([]adspower.Profile, error) {
	const pageSize = 100
	var out []adspower.Profile
	for page := 1; ; page++ {
		batch, err := ads.ListProfiles(ctx, page, pageSize)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < pageSize {
			break
		}
	}
	return out, nil
}

// filterProfiles keeps only profiles whose name contains sub (case-insensitive); empty sub keeps all.
func filterProfiles(profiles []adspower.Profile, sub string) []adspower.Profile {
	sub = strings.TrimSpace(sub)
	if sub == "" {
		return profiles
	}
	needle := strings.ToLower(sub)
	out := make([]adspower.Profile, 0, len(profiles))
	for _, p := range profiles {
		if strings.Contains(strings.ToLower(p.Name), needle) {
			out = append(out, p)
		}
	}
	return out
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
