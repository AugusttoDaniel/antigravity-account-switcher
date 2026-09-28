package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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
	assignProxies := fs.Bool("assign-proxies", true, "Bind each account's proxy in OmniRoute after import (API mode)")
	overwrite := fs.Bool("overwrite", false, "Overwrite an existing OmniRoute connection for the same account")
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

	type exportItem struct {
		email    string
		proxyURL string
		token    map[string]any
	}
	var items []exportItem

	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		if sub != "" && !strings.Contains(strings.ToLower(acc.Email), sub) {
			continue
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
		client := omniroute.NewClient(
			omniroute.WithBaseURL(*apiURL),
			omniroute.WithToken(*token),
			omniroute.WithInsecureTLS(*insecure),
		)

		// Map email -> proxy for post-import proxy assignment.
		proxyByEmail := make(map[string]string, len(items))
		var entries []omniroute.AgyEntry
		for _, it := range items {
			proxyByEmail[strings.ToLower(it.email)] = it.proxyURL
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
				fmt.Fprintf(os.Stderr, "  import error [%s]: %s\n", e.Name, e.Message)
			}
		}
		fmt.Printf("OmniRoute import: %d imported, %d failed.\n", imported, failed)

		// ---- Per-connection proxy assignment ----
		if *assignProxies {
			assigned, skipped := 0, 0
			for _, conn := range allCreated {
				key := strings.ToLower(conn.Email)
				if key == "" {
					key = strings.ToLower(conn.Name)
				}
				proxyURL := proxyByEmail[key]
				if strings.TrimSpace(proxyURL) == "" {
					continue // account has no proxy; nothing to bind
				}
				if conn.ID == "" {
					skipped++
					continue
				}
				cfg, ok, pErr := omniroute.ProxyConfigFromURL(proxyURL)
				if pErr != nil || !ok {
					fmt.Fprintf(os.Stderr, "  %s: bad proxy, not assigned: %v\n", conn.Email, pErr)
					skipped++
					continue
				}
				if aErr := client.AssignConnectionProxy(ctx, conn.ID, cfg); aErr != nil {
					fmt.Fprintf(os.Stderr, "  %s: proxy assign failed: %v\n", conn.Email, aErr)
					skipped++
					continue
				}
				assigned++
			}
			fmt.Printf("Proxy assignment: %d bound, %d skipped.\n", assigned, skipped)
		}
	}
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
