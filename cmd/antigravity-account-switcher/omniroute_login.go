package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/adspower"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/aliassync"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/config"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/omniroute"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/onboard"
)

// omniRouteOAuthProvider is the provider OmniRoute files a Google account under when it signs it in
// itself; export-omniroute imports under "agy" instead.
const omniRouteOAuthProvider = "antigravity"

// runOmniRouteLogin signs a Google account in through OmniRoute's OWN OAuth flow, from an isolated
// browser profile, so the connection is created under the antigravity provider and carries tokens
// issued to OmniRoute's own client (the one it renews with). The switcher only drives the browser and
// relays the code; the tokens are stored by OmniRoute and never pass through here.
func runOmniRouteLogin(args []string) {
	fs := flag.NewFlagSet("omniroute-login", flag.ExitOnError)
	apiURL := fs.String("url", omniroute.DefaultBaseURL, "OmniRoute base URL")
	token := fs.String("token", os.Getenv("OMNIROUTE_TOKEN"), "OmniRoute management Bearer token (or env OMNIROUTE_TOKEN)")
	insecure := fs.Bool("insecure", false, "Skip TLS verification (OmniRoute's self-signed localhost cert)")
	profileID := fs.String("profile", "", "Browser profile id to sign in from (required)")
	proxyOverride := fs.String("proxy", "", "Proxy of that profile; default: looked up in the Proxy Pool from the profile name (proxy-<host>-<port>)")
	adsURL := fs.String("api-url", adspower.DefaultBaseURL, "Browser-profile Local API URL")
	adsKey := fs.String("api-key", "", "Browser-profile Local API key")
	port := fs.Int("callback-port", 8080, "Local port tried for OmniRoute's redirect (localhost:8080); if busy, paste the address instead")
	timeout := fs.Duration("timeout", 15*time.Minute, "How long to wait for the sign-in")
	_ = fs.Parse(args)

	if strings.TrimSpace(*profileID) == "" {
		fatalf("Error: --profile is required (the browser profile the sign-in happens in).")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg, _ := config.Load()
	if cfg == nil {
		cfg = config.DefaultConfig()
	}
	apiSet := false
	fs.Visit(func(f *flag.Flag) { apiSet = apiSet || f.Name == "api-url" })
	if !apiSet && strings.TrimSpace(cfg.AdsPowerAPIURL) != "" {
		*adsURL = cfg.AdsPowerAPIURL // the dashboard's setting (AliasMode listens on 50400, not ADS Power's 50325)
	}
	ads := adspower.NewClient(adspower.WithBaseURL(*adsURL), adspower.WithAPIKey(*adsKey))

	proxy := strings.TrimSpace(*proxyOverride)
	if proxy == "" {
		p, err := ads.GetProfile(ctx, *profileID)
		if err != nil {
			fatalf("Error: could not read profile %s: %v", *profileID, err)
		}
		ep, ok := aliassync.EndpointFromProfileName(p.Name)
		if !ok {
			fatalf("Error: profile %q is not named proxy-<host>-<port>, so its proxy is unknown. Pass --proxy.", p.Name)
		}
		if proxy, ok = aliassync.PoolProxyByEndpoint(cfg.Proxies, ep); !ok {
			fatalf("Error: %s is not in the Proxy Pool, so OmniRoute could not be told to use it. Add it to the pool or pass --proxy.", ep)
		}
	}

	client := omniroute.NewClient(omniroute.WithBaseURL(*apiURL), omniroute.WithToken(*token), omniroute.WithInsecureTLS(*insecure))
	// Who OmniRoute already has, taken before anything is created: a second connection for the same
	// Google account would make it route through that account twice.
	existing, err := fetchExistingOmniRouteAccounts(ctx, client)
	if err != nil {
		fatalf("Error: could not check which accounts OmniRoute already has, nothing started: %v", err)
	}

	start, err := client.StartOAuth(ctx, omniRouteOAuthProvider)
	if err != nil {
		fatalf("Error: %v", err)
	}

	sess, err := onboard.Start(ctx, ads, *profileID, proxy)
	if err != nil {
		fatalf("Error: %v", err)
	}
	defer sess.Close()
	fmt.Printf("Opening the profile (proxy %s). Sign in to Google inside its window.\n", maskProxy(proxy))
	if err := sess.Opener(ctx, nil)(start.AuthURL); err != nil {
		fmt.Fprintf(os.Stderr, "Could not drive the browser (%v); open this URL in the profile yourself:\n%s\n", err, start.AuthURL)
	}

	code, err := awaitOmniRouteCode(ctx, start, *port, *timeout)
	if err != nil {
		fatalf("\nSign-in failed: %v", err)
	}

	conn, err := client.ExchangeOAuth(ctx, omniRouteOAuthProvider, omniroute.OAuthExchange{
		Code: code, RedirectURI: start.RedirectURI, CodeVerifier: start.CodeVerifier, State: start.State,
	})
	if err != nil {
		fatalf("Error: OmniRoute could not finish the sign-in: %v", err)
	}
	sess.Close()

	if conn.ID == "" {
		// The exchange did not say which connection it made: it is the one that was not there before.
		known := map[string]bool{}
		for _, cs := range existing.antigravity {
			for _, c := range cs {
				known[c.ID] = true
			}
		}
		if after, lErr := client.ListConnections(ctx, omniRouteOAuthProvider); lErr == nil {
			var fresh []omniroute.Connection
			for _, c := range after {
				if !known[c.ID] {
					fresh = append(fresh, c)
				}
			}
			if len(fresh) == 1 {
				conn = &omniroute.OAuthConnection{ID: fresh[0].ID, Email: fresh[0].Email, Name: fresh[0].Name}
			}
		}
	}

	email := strings.TrimSpace(conn.Email)
	if email == "" {
		email = strings.TrimSpace(conn.Name)
	}
	fmt.Printf("\nOmniRoute stored the %s connection for %s.\n", omniRouteOAuthProvider, orDash(email))
	if key := strings.ToLower(email); key != "" && (len(existing.agy[key]) > 0 || len(existing.antigravity[key]) > 0) {
		fmt.Fprintf(os.Stderr, "Warning: OmniRoute already had a connection for %s before this sign-in; check its dashboard for a duplicate.\n", email)
	}

	if conn.ID == "" {
		fmt.Fprintln(os.Stderr, "Note: OmniRoute did not report the connection id, so its proxy was not bound; run export-omniroute --api to bind it.")
		return
	}
	bound, unchanged, skipped := bindProxies(ctx, client, []bindTarget{{email: email, connectionID: conn.ID, proxyURL: proxy}}, os.Stdout, os.Stderr)
	fmt.Printf("Proxy binding: %d bound, %d already in effect, %d skipped.\n", bound, unchanged, skipped)
}

// awaitOmniRouteCode waits for the consent redirect. OmniRoute's redirect is fixed at
// localhost:<port>/callback; the loopback listener catches it when that port is free, and the user
// can always paste the address the browser landed on.
func awaitOmniRouteCode(ctx context.Context, start *omniroute.OAuthStart, port int, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	results := make(chan string, 2)

	if ru, err := url.Parse(start.RedirectURI); err == nil && ru.Path != "" {
		if ln, lerr := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); lerr == nil {
			mux := http.NewServeMux()
			mux.HandleFunc(ru.Path, func(w http.ResponseWriter, r *http.Request) {
				code, cerr := omniroute.CallbackCode(r.URL.String(), start.State)
				if cerr != nil {
					http.Error(w, cerr.Error(), http.StatusBadRequest)
					return
				}
				fmt.Fprintln(w, "Signed in. You can close this window.")
				select {
				case results <- code:
				default:
				}
			})
			srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
			go func() { _ = srv.Serve(ln) }()
			defer srv.Close()
		} else {
			fmt.Fprintf(os.Stderr, "Note: port %d cannot be opened here (%v).\n", port, lerr)
		}
	}

	fmt.Println("After signing in, the browser lands on a page that may fail to load. If this does not finish by")
	fmt.Println("itself, copy that page's FULL address from the address bar, paste it here and press Enter.")
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			code, err := omniroute.CallbackCode(line, start.State)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  %v\nPaste it again: ", err)
				continue
			}
			select {
			case results <- code:
			default:
			}
			return
		}
	}()

	select {
	case code := <-results:
		return code, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", errors.New("timed out waiting for the sign-in")
		}
		return "", ctx.Err()
	}
}
