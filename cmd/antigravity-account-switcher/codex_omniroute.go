package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/codex"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/omniroute"
)

// runCodexExportOmniRoute hands Codex accounts to OmniRoute. A Codex refresh token is single-use, so
// the tokens can live in only one place: once OmniRoute has them this switcher stops renewing,
// reading and switching to the account (unless --force). Without --yes it only prints the plan.
func runCodexExportOmniRoute(args []string) {
	fs := flag.NewFlagSet("codex-export-omniroute", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "Path to SQLite database file")
	apiURL := fs.String("url", omniroute.DefaultBaseURL, "OmniRoute base URL")
	token := fs.String("token", os.Getenv("OMNIROUTE_TOKEN"), "OmniRoute management Bearer token (or env OMNIROUTE_TOKEN)")
	insecure := fs.Bool("insecure", false, "Skip TLS verification (OmniRoute's self-signed localhost cert)")
	assignProxies := fs.Bool("assign-proxies", true, "Bind each account's proxy on its OmniRoute connection, including accounts OmniRoute already has; only changes it when it differs, never removes one")
	overwrite := fs.Bool("overwrite", false, "Replace a connection OmniRoute already has for the same account with our tokens (never for an account already handed over)")
	filter := fs.String("filter", "", "Only export accounts whose email contains this substring")
	yes := fs.Bool("yes", false, "Actually hand the accounts over (without it, only the plan is printed)")
	_ = fs.Parse(args)

	if strings.TrimSpace(*token) == "" {
		codexFatal("Error: no OmniRoute token: pass --token or set OMNIROUTE_TOKEN (a management token with the manage scope).")
	}
	db, svc := openCodex(*dbPath)
	defer db.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	all, err := svc.Repo.List(ctx)
	if err != nil {
		codexFatal("Error: %v", err)
	}
	sub := strings.ToLower(strings.TrimSpace(*filter))
	var accs []*domain.CodexAccount
	for _, a := range all {
		if sub == "" || strings.Contains(strings.ToLower(a.Email), sub) {
			accs = append(accs, a)
		}
	}
	if len(accs) == 0 {
		fmt.Println("No Codex accounts to export.")
		return
	}

	client := omniroute.NewClient(
		omniroute.WithBaseURL(*apiURL),
		omniroute.WithToken(*token),
		omniroute.WithInsecureTLS(*insecure),
	)
	results, err := svc.ExportToOmniRoute(ctx, client, accs, codex.ExportOptions{
		Overwrite: *overwrite, AssignProxies: *assignProxies && *yes, DryRun: !*yes,
	})
	if err != nil {
		codexFatal("Error: %v", err)
	}

	if !*yes {
		fmt.Println("PLAN (nothing was sent):")
	}
	counts := map[codex.ExportOutcome]int{}
	for _, r := range results {
		counts[r.Outcome]++
		line := fmt.Sprintf("  %s: %s", r.Account.Email, r.Outcome)
		if r.Detail != "" {
			line += " - " + r.Detail
		}
		if r.Proxy != "" {
			line += " [proxy " + r.Proxy + "]"
		}
		fmt.Println(line)
	}

	if !*yes {
		n := counts[codex.ExportExported]
		if n == 0 {
			fmt.Println("\nNothing would be handed over.")
			return
		}
		fmt.Printf("\n%d account(s) would be handed to OmniRoute.\n", n)
		fmt.Println("A Codex refresh token is single-use, so the tokens can live in only one place: after the hand-over")
		fmt.Println("OmniRoute renews them, and this switcher stops renewing, reading limits for, or switching the Codex CLI")
		fmt.Println("to those accounts (use --force on those commands to override, which breaks OmniRoute's session).")
		fmt.Println("Taking an account back means signing in again with codex-add.")
		fmt.Println("Re-run with --yes to proceed.")
		return
	}

	fmt.Printf("\nCodex export: %d handed over, %d already in OmniRoute, %d skipped, %d failed.\n",
		counts[codex.ExportExported], counts[codex.ExportExists], counts[codex.ExportSkipped], counts[codex.ExportFailed])
	if counts[codex.ExportFailed] > 0 {
		os.Exit(1)
	}
}
