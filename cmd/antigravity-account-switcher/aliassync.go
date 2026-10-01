package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/adspower"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/aliassync"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/config"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/egress"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/omniroute"
)

const aliasModeLocalURL = "http://127.0.0.1:50400"

// runSyncProxiesToAliasMode creates one AliasMode profile per proxy of the OmniRoute registry, each
// already bound to its proxy. OmniRoute never lists credentials, so they come from the connection a
// proxy is assigned to or, for the rest, from the local proxy pool matched by host and port.
func runSyncProxiesToAliasMode(args []string) {
	fs := flag.NewFlagSet("sync-proxies-to-aliasmode", flag.ExitOnError)
	apiURL := fs.String("url", omniroute.DefaultBaseURL, "OmniRoute base URL")
	token := fs.String("token", os.Getenv("OMNIROUTE_TOKEN"), "OmniRoute management Bearer token (or env OMNIROUTE_TOKEN)")
	insecure := fs.Bool("insecure", false, "Skip TLS verification (OmniRoute's self-signed localhost cert)")
	profileURL := fs.String("api-url", "", "Profile API URL (default: adspower_api_url, then AliasMode on 127.0.0.1:50400, then ADS Power)")
	profileKey := fs.String("api-key", "", "Profile API key (default: adspower_api_key)")
	engine := fs.String("engine", "", "Browser engine for the profiles (default: adspower_engine, else cloak)")
	dryRun := fs.Bool("dry-run", false, "Show what would be created without creating anything")
	_ = fs.Parse(args)

	if strings.TrimSpace(*token) == "" {
		fatalf("Error: no OmniRoute token: pass --token or set OMNIROUTE_TOKEN (a management token with the manage scope).")
	}
	cfg, _ := config.Load()
	if cfg == nil {
		cfg = config.DefaultConfig()
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	ads, base, err := connectProfileAPI(ctx, firstNonEmptyStr(*profileURL, cfg.AdsPowerAPIURL), firstNonEmptyStr(*profileKey, cfg.AdsPowerAPIKey))
	if err != nil {
		fatalf("Error: %v", err)
	}
	eng := firstNonEmptyStr(*engine, cfg.AdsPowerEngine, "cloak")

	omni := omniroute.NewClient(
		omniroute.WithBaseURL(*apiURL),
		omniroute.WithToken(*token),
		omniroute.WithInsecureTLS(*insecure),
	)
	cands, missing, err := aliassync.Collect(ctx, omni, cfg.Proxies)
	if err != nil {
		fatalf("Error: %v", err)
	}
	fmt.Printf("OmniRoute registry: %d proxies with credentials found, %d without.\n", len(cands), len(missing))
	fmt.Printf("Profile API: %s (engine %s)%s\n\n", base, eng, map[bool]string{true: "  [dry run]", false: ""}[*dryRun])

	results, err := aliassync.Apply(ctx, ads, cands, eng, *dryRun)
	if err != nil {
		fatalf("Error: %v", err)
	}
	counts := map[aliassync.Outcome]int{}
	for _, r := range results {
		counts[r.Outcome]++
		line := fmt.Sprintf("  %s (%s): %s", r.Name, r.HostPort, r.Outcome)
		switch r.Outcome {
		case aliassync.Created:
			line += fmt.Sprintf(" profile %q (id %s) [credentials from %s]", r.ProfileName, r.ProfileID, r.Source)
		case aliassync.WouldCreate:
			line += fmt.Sprintf(" profile %q [credentials from %s]", r.ProfileName, r.Source)
		case aliassync.Exists:
			line += fmt.Sprintf(" as profile %q (id %s)", r.ProfileName, r.ProfileID)
		case aliassync.Failed:
			line += ": " + r.Err.Error()
		}
		fmt.Println(line)
	}
	for _, m := range missing {
		fmt.Printf("  %s (%s): no credentials - %s\n", m.Name, m.HostPort, m.Reason)
	}

	fmt.Printf("\nProfiles: %d created, %d already there, %d failed", counts[aliassync.Created]+counts[aliassync.WouldCreate], counts[aliassync.Exists], counts[aliassync.Failed])
	fmt.Printf(", %d skipped for lack of credentials.\n", len(missing))
	if len(missing) > 0 {
		fmt.Println("OmniRoute never lists a proxy's credentials. For the ones above, paste your provider's proxy table into")
		fmt.Println("the dashboard's Proxy Pool (or add them to the config's \"proxies\"), then run this again: they are matched by host:port.")
	}
	if counts[aliassync.Failed] > 0 {
		os.Exit(1)
	}
}

// connectProfileAPI finds a reachable profile API. The proxy credentials are sent to it, so it must be
// on this machine.
func connectProfileAPI(ctx context.Context, configured, key string) (*adspower.Client, string, error) {
	candidates := []string{aliasModeLocalURL, adspower.DefaultBaseURL}
	if strings.TrimSpace(configured) != "" {
		candidates = []string{strings.TrimSpace(configured)}
	}
	var tried []string
	for _, base := range candidates {
		if err := requireLocalProfileAPI(base); err != nil {
			return nil, "", err
		}
		ads := adspower.NewClient(adspower.WithBaseURL(base), adspower.WithAPIKey(key))
		if err := ads.Ping(ctx); err == nil {
			return ads, base, nil
		}
		tried = append(tried, base)
	}
	return nil, "", fmt.Errorf("no profile API answered (tried %s): start AliasMode with its Local API enabled", strings.Join(tried, ", "))
}

func requireLocalProfileAPI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%q is not a valid profile API URL", raw)
	}
	host := u.Hostname()
	if egress.IsLoopbackHost(host) || strings.EqualFold(host, "local.adspower.net") {
		return nil
	}
	return fmt.Errorf("the profile API must run on this machine (got host %q): the proxies' credentials are sent to it", host)
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
