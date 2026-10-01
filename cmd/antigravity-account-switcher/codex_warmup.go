package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/codex"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
)

// runCodexWarmup sends the account one minimal request through its own proxy so its rate-limit window
// starts now. It spends a little of the account's quota, which is the point.
func runCodexWarmup(args []string) {
	fs := flag.NewFlagSet("codex-warmup", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "Path to SQLite database file")
	all := fs.Bool("all", false, "Every account (default: the one named, or the active one)")
	model := fs.String("model", "", "Model to use (default: the one the account's model list ranks first)")
	allowDirect := fs.Bool("allow-direct", false, "Warm accounts that have no proxy from this machine's real IP")
	force := fs.Bool("force", false, "Also warm accounts handed to OmniRoute (breaks OmniRoute's session for them)")
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
		a, err := svc.Repo.GetActive(ctx)
		if err != nil {
			codexFatal("No account given and none is active: codex-warmup [flags] <account_id|email>   (or --all)")
		}
		targets = []*domain.CodexAccount{a}
	}
	if len(targets) == 0 {
		fmt.Println("No Codex accounts yet. Run 'codex-add' (or 'codex-import').")
		return
	}

	fmt.Println("Sending one minimal request per account, through its own proxy. This spends a little quota.")
	failed := 0
	for i, a := range targets {
		if i > 0 {
			time.Sleep(5 * time.Second) // never a burst, even by hand
		}
		res, err := svc.WarmUp(ctx, a.ID, codex.WarmOptions{
			RefreshOptions: codex.RefreshOptions{AllowDirect: *allowDirect, Force: *force},
			Model:          *model,
		})
		if err != nil {
			failed++
			fmt.Printf("  %s: failed: %v\n", a.Email, err)
			continue
		}
		fmt.Printf("  %s: warmed (model %s)\n", a.Email, res.Model)
	}
	if failed > 0 {
		os.Exit(1)
	}
}

// runCodexWarmupSchedule shows or sets an account's scheduled warm-up. The schedule runs while `serve`
// is running; it is off until turned on here (or in the dashboard).
func runCodexWarmupSchedule(args []string) {
	fs := flag.NewFlagSet("codex-warmup-schedule", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "Path to SQLite database file")
	times := fs.String("times", "", `Local times of day to warm the account, e.g. "07:00,12:30" (turns the schedule on)`)
	off := fs.Bool("off", false, "Turn the schedule off (the times are kept)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		codexFatal("Usage: antigravity-account-switcher codex-warmup-schedule [flags] <account_id|email>")
	}
	if *off && strings.TrimSpace(*times) != "" {
		codexFatal("Error: pass either --times or --off, not both.")
	}

	db, svc := openCodex(*dbPath)
	defer db.Close()
	ctx := context.Background()
	acc, err := svc.Resolve(ctx, fs.Arg(0))
	if err != nil {
		codexFatal("Error: %v", err)
	}

	switch {
	case strings.TrimSpace(*times) != "":
		parsed, perr := codex.ParseTimes(*times)
		if perr != nil {
			codexFatal("Error: %v", perr)
		}
		w, err := svc.SetWarmupSchedule(ctx, acc.ID, parsed, true)
		if err != nil {
			codexFatal("Error: %v", err)
		}
		fmt.Printf("Warm-up for %s is ON at %s (local time).\n", acc.Email, strings.Join(w.Times, ", "))
		fmt.Println("It runs while 'serve' is running; slots the switcher misses by more than 10 minutes are skipped, not fired late.")
		return
	case *off:
		cur, gerr := svc.Warmups.GetWarmup(ctx, acc.ID)
		if gerr != nil {
			fmt.Printf("Warm-up for %s was not set up.\n", acc.Email)
			return
		}
		if _, err := svc.SetWarmupSchedule(ctx, acc.ID, cur.Times, false); err != nil {
			codexFatal("Error: %v", err)
		}
		fmt.Printf("Warm-up for %s is OFF.\n", acc.Email)
		return
	}

	w, err := svc.Warmups.GetWarmup(ctx, acc.ID)
	if err != nil {
		fmt.Printf("%s: no warm-up schedule. Set one with --times \"07:00,12:30\".\n", acc.Email)
		return
	}
	state := "OFF"
	if w.Enabled {
		state = "ON"
	}
	fmt.Printf("%s: warm-up %s, times: %s\n", acc.Email, state, orDash(strings.Join(w.Times, ", ")))
	if !w.LastRunAt.IsZero() {
		line := fmt.Sprintf("  last run: %s - %s", w.LastRunAt.Local().Format("2006-01-02 15:04"), w.LastStatus)
		if w.LastModel != "" {
			line += " (model " + w.LastModel + ")"
		}
		if w.LastDetail != "" {
			line += ": " + w.LastDetail
		}
		fmt.Println(line)
	}
}
