package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/egress"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/oauth"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/omniroute"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/store/sqlite"
)

// runExportOmniRoute exports the switcher's accounts into OmniRoute (OmniRouter) as `agy`
// connections. It can write agy token files to a folder (for manual paste/upload/zip import),
// push them to OmniRoute's management API directly, or both, and can bind each account's proxy
// inside OmniRoute after import.
func runExportOmniRoute(args []string) {
	fs := flag.NewFlagSet("export-omniroute", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "Path to SQLite database file")
	outDir := fs.String("out", "", "Write one <email>.json (agy token) per account into this directory")
	useAPI := fs.Bool("api", false, "Push accounts to the OmniRoute management API")
	apiURL := fs.String("url", omniroute.DefaultBaseURL, "OmniRoute base URL (API mode)")
	token := fs.String("token", os.Getenv("OMNIROUTE_TOKEN"), "OmniRoute management Bearer token (or env OMNIROUTE_TOKEN)")
	insecure := fs.Bool("insecure", false, "Skip TLS verification (OmniRoute's self-signed localhost cert)")
	assignProxies := fs.Bool("assign-proxies", true, "Bind each account's proxy on its OmniRoute connection (API mode), including accounts OmniRoute already has; only changes it when it differs, never removes one")
	overwrite := fs.Bool("overwrite", false, "Overwrite an existing OmniRoute connection for the same account")
	allowDuplicate := fs.Bool("allow-duplicate", false, "Import an account even if OmniRoute already has it through its own Antigravity login (creates a second connection)")
	noRefresh := fs.Bool("no-refresh", false, "Do not refresh tokens before export (export may carry an expired access_token)")
	filter := fs.String("filter", "", "Only export accounts whose email contains this substring")
	_ = fs.Parse(args)

	if strings.TrimSpace(*outDir) == "" && !*useAPI {
		fmt.Println("Nothing to do: pass --out <dir> to write files and/or --api to push to OmniRoute.")
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
	accounts, err := accRepo.List(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing accounts: %v\n", err)
		os.Exit(1)
	}

	sub := strings.ToLower(strings.TrimSpace(*filter))
	oauthService := oauth.NewOAuthService(accRepo)

	// In API mode, look up what OmniRoute already has first, so accounts it holds are skipped
	// instead of duplicated. Without that list there is no safe way to import.
	var client *omniroute.Client
	var existing existingOmniRouteAccounts
	if *useAPI {
		client = omniroute.NewClient(
			omniroute.WithBaseURL(*apiURL),
			omniroute.WithToken(*token),
			omniroute.WithInsecureTLS(*insecure),
		)
		existing, err = fetchExistingOmniRouteAccounts(ctx, client)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: could not check which accounts OmniRoute already has, nothing imported: %v\n", err)
			os.Exit(1)
		}
	}
	alreadyThere := 0

	type exportItem struct {
		email    string
		proxyURL string
		token    map[string]any
		skipAPI  bool // OmniRoute already has it; still written in files mode
	}
	var items []exportItem

	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		if sub != "" && !strings.Contains(strings.ToLower(acc.Email), sub) {
			continue
		}

		skipAPI := false
		if *useAPI {
			if skip, reason := skipExport(acc.Email, existing, *overwrite, *allowDuplicate); skip {
				fmt.Printf("  %s: %s\n", acc.Email, reason)
				alreadyThere++
				skipAPI = true
				if strings.TrimSpace(*outDir) == "" {
					// Nothing to import or write, so do not refresh its token; it is kept only so
					// its proxy can still be bound below.
					items = append(items, exportItem{email: acc.Email, proxyURL: acc.ProxyURL, skipAPI: true})
					continue
				}
			}
		}

		// Refresh through the account's own proxy so the exported access_token is fresh.
		if !*noRefresh {
			if refreshed, rErr := oauthService.EnsureValidToken(ctx, acc, 60*time.Second); rErr != nil {
				fmt.Fprintf(os.Stderr, "Skipping %s: token refresh failed: %v\n", acc.Email, rErr)
				continue
			} else if refreshed != nil {
				acc = refreshed
			}
		}
		if strings.TrimSpace(acc.AccessToken) == "" {
			fmt.Fprintf(os.Stderr, "Skipping %s: no access_token (run without --no-refresh, or re-authenticate)\n", acc.Email)
			continue
		}

		items = append(items, exportItem{
			email:    acc.Email,
			proxyURL: acc.ProxyURL,
			token:    omniroute.BuildAgyTokenJSON(acc.AccessToken, acc.RefreshToken, acc.TokenExpiry),
			skipAPI:  skipAPI,
		})
	}

	if len(items) == 0 {
		fmt.Println("No accounts to export.")
		return
	}

	// ---- Files mode ----
	if dir := strings.TrimSpace(*outDir); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			fmt.Fprintf(os.Stderr, "Error creating output directory: %v\n", err)
			os.Exit(1)
		}
		written := 0
		for _, it := range items {
			data, mErr := json.MarshalIndent(it.token, "", "  ")
			if mErr != nil {
				fmt.Fprintf(os.Stderr, "  %s: marshal failed: %v\n", it.email, mErr)
				continue
			}
			path := filepath.Join(dir, sanitizeFilename(it.email)+".json")
			if wErr := os.WriteFile(path, data, 0o600); wErr != nil {
				fmt.Fprintf(os.Stderr, "  %s: write failed: %v\n", it.email, wErr)
				continue
			}
			written++
		}
		fmt.Printf("Wrote %d agy token file(s) to %s (import them in OmniRoute via agy-auth import / zip-extract).\n", written, dir)
	}

	// ---- API mode ----
	if *useAPI {
		// Map email -> proxy, for every account: the ones OmniRoute already has still get their
		// proxy bound below.
		proxyByEmail := make(map[string]string, len(items))
		var entries []omniroute.AgyEntry
		for _, it := range items {
			proxyByEmail[strings.ToLower(it.email)] = it.proxyURL
			if it.skipAPI {
				continue
			}
			entries = append(entries, omniroute.AgyEntry{JSON: it.token, Name: it.email, Email: it.email})
		}

		var allCreated []omniroute.Connection
		imported, failed := 0, 0
		for start := 0; start < len(entries); start += omniroute.MaxBulkEntries {
			end := start + omniroute.MaxBulkEntries
			if end > len(entries) {
				end = len(entries)
			}
			res, iErr := client.ImportBulkAgy(ctx, entries[start:end], *overwrite)
			if iErr != nil {
				fmt.Fprintf(os.Stderr, "Bulk import (batch %d-%d) failed: %v\n", start+1, end, iErr)
				failed += end - start
				continue
			}
			imported += res.Success
			failed += res.Failed
			allCreated = append(allCreated, res.Created...)
			for _, e := range res.Errors {
				// OmniRoute refusing a second agy connection means the account is already there
				// (e.g. added between the check above and this import): skipped, not failed.
				if isAlreadyExistsError(e.Message) {
					fmt.Printf("  %s: already in OmniRoute (pass --overwrite to refresh its tokens there)\n", e.Name)
					alreadyThere++
					failed--
					continue
				}
				fmt.Fprintf(os.Stderr, "  import error [%s]: %s\n", e.Name, e.Message)
			}
		}
		fmt.Printf("OmniRoute import: %d imported, %d already in OmniRoute (skipped), %d failed.\n", imported, alreadyThere, failed)

		// ---- Per-connection proxy binding ----
		if *assignProxies {
			var targets []bindTarget
			// Connections this import just created.
			for _, conn := range allCreated {
				key := strings.ToLower(conn.Email)
				if key == "" {
					key = strings.ToLower(conn.Name)
				}
				if conn.ID == "" || strings.TrimSpace(proxyByEmail[key]) == "" {
					continue // no proxy to bind for this account
				}
				targets = append(targets, bindTarget{email: key, connectionID: conn.ID, proxyURL: proxyByEmail[key]})
			}
			// Connections OmniRoute already had: import never touched them, so bind them here.
			for _, it := range items {
				if !it.skipAPI || strings.TrimSpace(it.proxyURL) == "" {
					continue
				}
				conns := existing.connectionsFor(it.email)
				if len(conns) != 1 {
					fmt.Fprintf(os.Stderr, "  %s: proxy not bound: OmniRoute holds %d connections for it, remove the duplicate first\n", it.email, len(conns))
					continue
				}
				targets = append(targets, bindTarget{email: it.email, connectionID: conns[0].ID, proxyURL: it.proxyURL})
			}
			bound, unchanged, skipped := bindProxies(ctx, client, targets, os.Stdout, os.Stderr)
			fmt.Printf("Proxy binding: %d bound, %d already in effect, %d skipped.\n", bound, unchanged, skipped)
		}
	}
}

// existingOmniRouteAccounts holds the connections OmniRoute already has, by lowercase email, per
// Google provider.
type existingOmniRouteAccounts struct {
	agy         map[string][]omniroute.Connection // imported from here (agy-auth)
	antigravity map[string][]omniroute.Connection // connected through OmniRoute's own Antigravity login
}

// fetchExistingOmniRouteAccounts lists the Google accounts OmniRoute already holds under both
// providers it keeps them in.
func fetchExistingOmniRouteAccounts(ctx context.Context, client *omniroute.Client) (existingOmniRouteAccounts, error) {
	existing := existingOmniRouteAccounts{agy: map[string][]omniroute.Connection{}, antigravity: map[string][]omniroute.Connection{}}
	for provider, byEmail := range map[string]map[string][]omniroute.Connection{"agy": existing.agy, "antigravity": existing.antigravity} {
		conns, err := client.ListConnections(ctx, provider)
		if err != nil {
			return existing, fmt.Errorf("list %s accounts: %w", provider, err)
		}
		for _, c := range conns {
			keys := map[string]bool{}
			for _, key := range []string{c.Email, c.Name} {
				if k := strings.ToLower(strings.TrimSpace(key)); k != "" && !keys[k] {
					keys[k] = true
					byEmail[k] = append(byEmail[k], c)
				}
			}
		}
	}
	return existing, nil
}

// connectionsFor returns the OmniRoute connections of an account, preferring the agy ones.
func (e existingOmniRouteAccounts) connectionsFor(email string) []omniroute.Connection {
	key := strings.ToLower(strings.TrimSpace(email))
	if cs := e.agy[key]; len(cs) > 0 {
		return cs
	}
	return e.antigravity[key]
}

// skipExport decides whether exporting email would duplicate an account OmniRoute already has,
// and why. OmniRoute itself only guards against a second agy connection; a Google account already
// connected through its own Antigravity login would silently get a second (agy) connection, and
// OmniRoute would then route through the same account twice.
func skipExport(email string, existing existingOmniRouteAccounts, overwrite, allowDuplicate bool) (bool, string) {
	key := strings.ToLower(strings.TrimSpace(email))
	switch {
	case len(existing.antigravity[key]) > 0 && !allowDuplicate:
		return true, "already connected in OmniRoute through its own Antigravity login; importing would duplicate it (pass --allow-duplicate to import anyway)"
	case len(existing.agy[key]) > 0 && !overwrite:
		return true, "already in OmniRoute (pass --overwrite to refresh its tokens there)"
	default:
		return false, ""
	}
}

// bindTarget is one account whose local proxy should be in effect on its OmniRoute connection.
type bindTarget struct {
	email        string
	connectionID string
	proxyURL     string
}

// bindProxies makes each target's OmniRoute connection use the account's local proxy. It only
// writes when they differ, so running it again changes nothing, and it never removes a proxy:
// an account without a local proxy is not a target.
func bindProxies(ctx context.Context, client *omniroute.Client, targets []bindTarget, out, errOut io.Writer) (bound, unchanged, skipped int) {
	for _, t := range targets {
		u, err := egress.ParseProxyURL(t.proxyURL)
		if err != nil {
			fmt.Fprintf(errOut, "  %s: proxy not bound: %v\n", t.email, err)
			skipped++
			continue
		}
		res, err := client.BindConnectionProxy(ctx, t.connectionID, u)
		if err != nil {
			fmt.Fprintf(errOut, "  %s: proxy not bound: %v\n", t.email, err)
			skipped++
			continue
		}
		if !res.Changed {
			fmt.Fprintf(out, "  %s: proxy already in effect in OmniRoute (%s)\n", t.email, res.Was)
			unchanged++
			continue
		}
		fmt.Fprintf(out, "  %s: proxy bound in OmniRoute (was %s)\n", t.email, res.Was)
		bound++
	}
	return bound, unchanged, skipped
}

// isAlreadyExistsError recognises OmniRoute's refusal to create a second agy connection for an
// account, which means "skipped", not a failure.
func isAlreadyExistsError(message string) bool {
	return strings.Contains(strings.ToLower(message), "already exists")
}

// sanitizeFilename makes an email safe to use as a filename.
func sanitizeFilename(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "account"
	}
	return b.String()
}
