package main

import (
	"bufio"
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
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/codex"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/egress"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/oauth"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/onboard"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/store/sqlite"
)

// openCodex opens the database and builds the Codex service for the configured Codex home.
func openCodex(dbPath string) (*sqlite.DB, *codex.Service) {
	db, err := sqlite.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening SQLite database: %v\n", err)
		os.Exit(1)
	}
	home, err := codex.Home()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	return db, codex.NewService(sqlite.NewCodexAccountRepository(db), home)
}

func codexFatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

// runCodexAdd signs a ChatGPT/Codex account in and stores it. The token exchange goes through the
// account's proxy; the browser only does so when it is an isolated profile (--adspower). By default
// the sign-in URL is printed and no browser is opened, so nothing leaves from the real IP.
func runCodexAdd(args []string) {
	fs := flag.NewFlagSet("codex-add", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "Path to SQLite database file")
	proxyURL := fs.String("proxy", "", "Proxy the token exchange and every later refresh use (required unless --allow-direct)")
	allowDirect := fs.Bool("allow-direct", false, "Allow contacting OpenAI from this machine's real IP when no proxy is given")
	useProfile := fs.Bool("adspower", false, "Sign in inside an isolated AliasMode/ADS Power profile bound to the proxy (recommended)")
	openBrowser := fs.Bool("open-browser", false, "Open the sign-in page in the DEFAULT browser (it reaches OpenAI from the real IP)")
	profileID := fs.String("profile", "", "Reuse this existing profile id with --adspower")
	apiURL := fs.String("api-url", adspower.DefaultBaseURL, "Browser-profile Local API URL (with --adspower)")
	apiKey := fs.String("api-key", "", "Browser-profile Local API key (with --adspower)")
	engine := fs.String("engine", "cloak", "Browser engine for a new profile (with --adspower)")
	timeout := fs.Duration("timeout", codex.DefaultLoginTimeout, "How long to wait for the sign-in")
	_ = fs.Parse(args)

	proxy := strings.TrimSpace(*proxyURL)
	if proxy == "" && !*allowDirect {
		codexFatal("Error: --proxy is required: the login would otherwise reach OpenAI from your real IP (pass --allow-direct to accept that).")
	}
	if proxy != "" {
		if err := egress.ValidateProxyURL(proxy); err != nil {
			codexFatal("Error: proxy %s: %v", maskProxy(proxy), err)
		}
	}
	if *useProfile && *openBrowser {
		codexFatal("Error: --adspower and --open-browser are mutually exclusive.")
	}

	db, svc := openCodex(*dbPath)
	defer db.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	client, err := svc.NewClient(proxy)
	if err != nil {
		codexFatal("Error: %v", err)
	}

	opts := codex.LoginOptions{
		Client: client, Port: codex.CallbackPort, Timeout: *timeout,
		URLLogger: func(u string) { fmt.Printf("\nSign-in URL:\n%s\n\n", u) },
	}
	opts.Pasted, opts.OnManual = manualFallback()
	var usedProfile string
	switch {
	case *useProfile:
		ads := adspower.NewClient(adspower.WithBaseURL(*apiURL), adspower.WithAPIKey(*apiKey))
		id := strings.TrimSpace(*profileID)
		if id == "" {
			name := "codex-" + time.Now().UTC().Format("20060102-150405")
			id, err = onboard.CreateProfile(ctx, ads, name, *engine, proxy)
			if err != nil {
				codexFatal("Error creating the browser profile: %v", err)
			}
			fmt.Printf("Created profile %s bound to %s\n", id, maskProxy(proxy))
		}
		sess, err := onboard.Start(ctx, ads, id, proxy)
		if err != nil {
			codexFatal("Error: %v", err)
		}
		defer sess.Close()
		opts.Opener = sess.Opener(ctx, nil)
		usedProfile = id
		fmt.Println("Launching the isolated profile. Sign in to ChatGPT inside its window...")
	case *openBrowser:
		fmt.Fprintln(os.Stderr, "Warning: the default browser is not behind the proxy; OpenAI sees your real IP during the sign-in.")
		opts.Opener = oauth.DefaultBrowserOpener
	default:
		fmt.Println("Open the URL below in a browser that already uses this account's proxy, then sign in.")
	}

	tr, err := codex.Login(ctx, opts)
	if err != nil {
		codexFatal("\nSign-in failed: %v", err)
	}
	acc, err := svc.AddFromTokens(ctx, tr, proxy, usedProfile)
	if err != nil {
		codexFatal("Error: %v", err)
	}
	fmt.Printf("\nSuccess! Codex account %s added (plan: %s).\n", acc.Email, orDash(acc.PlanType))
	fmt.Printf("Account ID: %s\nProxy:      %s\n", acc.ID, maskProxy(acc.ProxyURL))
	fmt.Println("Use 'codex-switch' to make it the account the Codex CLI uses.")
}

func runCodexImport(args []string) {
	fs := flag.NewFlagSet("codex-import", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "Path to SQLite database file")
	proxyURL := fs.String("proxy", "", "Proxy to bind to the imported account (used by codex-refresh)")
	from := fs.String("file", "", "auth.json to import (default: the Codex CLI's current auth.json)")
	_ = fs.Parse(args)

	proxy := strings.TrimSpace(*proxyURL)
	if proxy != "" {
		if err := egress.ValidateProxyURL(proxy); err != nil {
			codexFatal("Error: proxy %s: %v", maskProxy(proxy), err)
		}
	}
	db, svc := openCodex(*dbPath)
	defer db.Close()
	path := *from
	if path == "" {
		path = codex.AuthPath(svc.Home)
	}
	acc, err := svc.ImportAuthFile(context.Background(), path, proxy)
	if err != nil {
		codexFatal("Error: %v", err)
	}
	fmt.Printf("Imported %s (plan: %s) as account %s.\n", acc.Email, orDash(acc.PlanType), acc.ID)
	if proxy == "" {
		fmt.Println("No proxy bound: set one with 'codex-set-proxy' before 'codex-refresh'.")
	}
}

func runCodexList(args []string) {
	fs := flag.NewFlagSet("codex-list", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "Path to SQLite database file")
	_ = fs.Parse(args)

	db, svc := openCodex(*dbPath)
	defer db.Close()
	accs, err := svc.Repo.List(context.Background())
	if err != nil {
		codexFatal("Failed to list Codex accounts: %v", err)
	}
	if len(accs) == 0 {
		fmt.Println("No Codex accounts yet. Run 'codex-add' (or 'codex-import').")
		return
	}
	fmt.Printf("%-36s  %-32s  %-8s  %-8s  %-7s  %-18s  %s\n", "ID", "EMAIL", "PLAN", "STATUS", "ACTIVE", "LAST REFRESH", "OUTBOUND PROXY")
	for _, a := range accs {
		mark := ""
		if a.IsActive {
			mark = "*"
		}
		last := "-"
		if !a.LastRefresh.IsZero() && a.LastRefresh.Year() > 1970 {
			last = a.LastRefresh.Local().Format("2006-01-02 15:04")
		}
		fmt.Printf("%-36s  %-32s  %-8s  %-8s  %-7s  %-18s  %s\n", a.ID, a.Email, orDash(a.PlanType), a.Status, mark, last, maskProxy(a.ProxyURL))
	}
}

func runCodexSwitch(args []string) {
	fs := flag.NewFlagSet("codex-switch", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "Path to SQLite database file")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		codexFatal("Usage: antigravity-account-switcher codex-switch [flags] <account_id|email>")
	}
	db, svc := openCodex(*dbPath)
	defer db.Close()
	ctx := context.Background()
	acc, err := svc.Resolve(ctx, fs.Arg(0))
	if err != nil {
		codexFatal("Error: %v", err)
	}
	if acc.Status == domain.AccountStatusError {
		fmt.Fprintf(os.Stderr, "Warning: %s is marked as errored (its refresh token was rejected); sign in again with codex-add.\n", acc.Email)
	}
	acc, err = svc.Switch(ctx, acc.ID)
	if err != nil {
		codexFatal("Error: %v", err)
	}
	fmt.Printf("Codex CLI now uses %s (%s). Restart any running Codex session to pick it up.\n", acc.Email, codex.AuthPath(svc.Home))
}

func runCodexRefresh(args []string) {
	fs := flag.NewFlagSet("codex-refresh", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "Path to SQLite database file")
	allowDirect := fs.Bool("allow-direct", false, "Refresh accounts that have no proxy from this machine's real IP")
	all := fs.Bool("all", false, "Refresh every account")
	_ = fs.Parse(args)

	db, svc := openCodex(*dbPath)
	defer db.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var targets []*domain.CodexAccount
	switch {
	case *all:
		list, err := svc.Repo.List(ctx)
		if err != nil {
			codexFatal("Error: %v", err)
		}
		targets = list
	case fs.NArg() == 1:
		a, err := svc.Resolve(ctx, fs.Arg(0))
		if err != nil {
			codexFatal("Error: %v", err)
		}
		targets = []*domain.CodexAccount{a}
	default:
		codexFatal("Usage: antigravity-account-switcher codex-refresh [flags] <account_id|email>   (or --all)")
	}

	failed := 0
	for _, a := range targets {
		_, err := svc.Refresh(ctx, a.ID, codex.RefreshOptions{AllowDirect: *allowDirect})
		switch {
		case err == nil:
			fmt.Printf("  %s: refreshed\n", a.Email)
		case errors.Is(err, codex.ErrInvalidGrant):
			failed++
			fmt.Printf("  %s: REJECTED (needs a new sign-in: codex-add)\n", a.Email)
		default:
			failed++
			fmt.Printf("  %s: failed: %v\n", a.Email, err)
		}
	}
	if failed > 0 {
		os.Exit(1)
	}
}

func runCodexSetProxy(args []string) {
	fs := flag.NewFlagSet("codex-set-proxy", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "Path to SQLite database file")
	_ = fs.Parse(args)
	if fs.NArg() != 2 {
		codexFatal("Usage: antigravity-account-switcher codex-set-proxy [flags] <account_id|email> <proxy_url>")
	}
	proxy := strings.TrimSpace(fs.Arg(1))
	if err := egress.ValidateProxyURL(proxy); err != nil {
		codexFatal("Error: %v", err)
	}
	db, svc := openCodex(*dbPath)
	defer db.Close()
	ctx := context.Background()
	acc, err := svc.Resolve(ctx, fs.Arg(0))
	if err != nil {
		codexFatal("Error: %v", err)
	}
	if err := svc.Repo.UpdateProxyURL(ctx, acc.ID, proxy); err != nil {
		codexFatal("Failed to update the proxy: %v", err)
	}
	fmt.Printf("Proxy for %s set to %s\n", acc.Email, maskProxy(proxy))
}

func runCodexRemove(args []string) {
	fs := flag.NewFlagSet("codex-remove", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "Path to SQLite database file")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		codexFatal("Usage: antigravity-account-switcher codex-remove [flags] <account_id|email>")
	}
	db, svc := openCodex(*dbPath)
	defer db.Close()
	ctx := context.Background()
	acc, err := svc.Resolve(ctx, fs.Arg(0))
	if err != nil {
		codexFatal("Error: %v", err)
	}
	if err := svc.Repo.Delete(ctx, acc.ID); err != nil {
		codexFatal("Error: %v", err)
	}
	fmt.Printf("Removed %s from the switcher. The Codex CLI's auth.json was not touched.\n", acc.Email)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// manualFallback lets codex-add finish when the OAuth callback port cannot be opened (Windows
// reserves 1374-1473, which contains 1455, for Hyper-V/WSL/Docker): the user pastes the address of
// the page that fails to load after signing in. The reader only starts in that case.
func manualFallback() (<-chan codex.Paste, func(error)) {
	pasted := make(chan codex.Paste, 1)
	start := func(reason error) {
		fmt.Fprintf(os.Stderr, "Note: the OAuth callback port %d cannot be opened on this machine (%v).\n", codex.CallbackPort, reason)
		fmt.Fprintln(os.Stderr, "Sign in with the URL above. The browser will then land on a page that fails to load:")
		fmt.Fprintln(os.Stderr, "copy its FULL address from the address bar and paste it here, then press Enter.")
		go func() {
			sc := bufio.NewScanner(os.Stdin)
			sc.Buffer(make([]byte, 64*1024), 1<<20)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if line == "" {
					continue
				}
				res := make(chan error, 1)
				pasted <- codex.Paste{URL: line, Result: res}
				if err := <-res; err != nil {
					fmt.Fprintf(os.Stderr, "  %v\nPaste it again: ", err)
					continue
				}
				return
			}
		}()
	}
	return pasted, start
}
