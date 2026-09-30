package codex

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultLoginTimeout bounds a sign-in a human completes by hand (2-step verification included).
const DefaultLoginTimeout = 15 * time.Minute

// Paste carries the redirect URL the browser ended up at, for machines where the loopback port
// cannot be bound (Windows reserves 1374-1473 for Hyper-V/WSL/Docker, which contains 1455: the
// page fails to load, but its address bar holds the code). Result, when set, receives the outcome:
// a problem with the pasted URL (the login keeps waiting), or nil/the exchange error once the
// login is over. It should be buffered.
type Paste struct {
	URL    string
	Result chan<- error
}

// LoginOptions configures one browser sign-in.
type LoginOptions struct {
	Client *Client
	// Port of the loopback callback; 0 picks a free one (tests). Production uses CallbackPort,
	// the only redirect the public client has registered.
	Port    int
	Timeout time.Duration
	// Opener launches the consent URL (a browser profile, or the default browser).
	Opener func(authURL string) error
	// URLLogger receives the consent URL, in case the opener cannot open it.
	URLLogger func(authURL string)
	// Pasted enables the manual fallback: a callback port that cannot be bound is no longer fatal,
	// and the sign-in completes from a redirect URL sent here.
	Pasted <-chan Paste
	// OnManual is called, before the consent URL is issued, when the port could not be bound and
	// the sign-in will complete from Pasted.
	OnManual func(reason error)
}

type loginResult struct {
	tokens *TokenResponse
	err    error
}

// Login runs the PKCE authorization-code flow and returns the tokens. The code comes back through
// a loopback listener, or, when it cannot be bound and opts.Pasted is set, from a pasted redirect
// URL. The exchange goes through opts.Client.HTTP, so a proxy-bound client keeps it off the real IP.
func Login(ctx context.Context, opts LoginOptions) (*TokenResponse, error) {
	if opts.Client == nil {
		return nil, errors.New("no OAuth client configured")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultLoginTimeout
	}
	pkce, err := NewPKCE()
	if err != nil {
		return nil, err
	}
	state, err := NewState()
	if err != nil {
		return nil, err
	}

	loginCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	resultCh := make(chan loginResult, 1)
	var once sync.Once
	finish := func(r loginResult) { once.Do(func() { resultCh <- r }) }

	port := opts.Port
	ln, lerr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.Port)))
	switch {
	case lerr == nil:
		port = ln.Addr().(*net.TCPAddr).Port
		redirect := RedirectURI(port)
		srv := serveCallback(loginCtx, ln, opts.Client, state, pkce.Verifier, redirect, finish)
		defer func() {
			sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer scancel()
			_ = srv.Shutdown(sctx)
		}()
	case opts.Pasted != nil:
		// The registered redirect is still the loopback one: the browser just cannot reach it.
		if opts.OnManual != nil {
			opts.OnManual(lerr)
		}
	default:
		return nil, fmt.Errorf("the OAuth callback port %d cannot be opened (a Codex login may be running, or Windows reserves it: see netsh int ipv4 show excludedportrange protocol=tcp): %w", opts.Port, lerr)
	}
	redirect := RedirectURI(port)

	authURL := opts.Client.AuthorizeURL(redirect, state, pkce)
	if opts.URLLogger != nil {
		opts.URLLogger(authURL)
	}
	if opts.Opener != nil {
		if err := opts.Opener(authURL); err != nil && opts.URLLogger == nil {
			return nil, fmt.Errorf("could not open the sign-in page: %w", err)
		}
	}

	for {
		select {
		case r := <-resultCh:
			return r.tokens, r.err
		case p := <-opts.Pasted:
			code, perr := codeFromRedirect(p.URL, state)
			if perr != nil {
				reply(p, perr)
				continue // a typo must not cost the whole sign-in
			}
			tr, xerr := opts.Client.Exchange(loginCtx, code, pkce.Verifier, redirect)
			reply(p, xerr)
			return tr, xerr
		case <-loginCtx.Done():
			return nil, errors.New("the sign-in timed out or was cancelled")
		}
	}
}

func reply(p Paste, err error) {
	if p.Result != nil {
		select {
		case p.Result <- err:
		default:
		}
	}
}

// codeFromRedirect extracts the authorization code from a pasted redirect URL (or just its query
// string), checking the state. Errors never echo the pasted text: it carries the code.
func codeFromRedirect(raw, wantState string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("paste the address of the page that failed to load after you signed in")
	}
	var q url.Values
	if i := strings.Index(raw, "?"); i >= 0 {
		var err error
		if q, err = url.ParseQuery(strings.SplitN(raw[i+1:], "#", 2)[0]); err != nil {
			return "", errors.New("that is not a valid redirect URL")
		}
	} else {
		return "", errors.New("that address has no code: paste the full address of the page that failed to load")
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(wantState)) != 1 {
		return "", errors.New("that address belongs to a different sign-in (state mismatch): start again and paste the newest one")
	}
	if e := q.Get("error"); e != "" {
		return "", fmt.Errorf("the sign-in was refused: %s", sanitizeCode(e))
	}
	code := q.Get("code")
	if code == "" {
		return "", errors.New("that address carries no authorization code")
	}
	return code, nil
}

func serveCallback(ctx context.Context, ln net.Listener, client *Client, state, verifier, redirect string, finish func(loginResult)) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			http.Error(w, "invalid state", http.StatusBadRequest)
			finish(loginResult{err: errors.New("the callback state did not match: sign-in aborted")})
			return
		}
		if e := q.Get("error"); e != "" {
			writePage(w, http.StatusOK, "Sign-in was not completed", "You can close this window.")
			finish(loginResult{err: fmt.Errorf("the sign-in was refused: %s", sanitizeCode(e))})
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			finish(loginResult{err: errors.New("the callback carried no authorization code")})
			return
		}
		tr, err := client.Exchange(ctx, code, verifier, redirect)
		if err != nil {
			writePage(w, http.StatusBadGateway, "Sign-in failed", "Go back to the switcher for details.")
			finish(loginResult{err: err})
			return
		}
		writePage(w, http.StatusOK, "Signed in", "You can close this window and return to the switcher.")
		finish(loginResult{tokens: tr})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 60 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return srv
}

func writePage(w http.ResponseWriter, status int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>%s</title><body style=\"font-family:sans-serif;margin:3rem\"><h2>%s</h2><p>%s</p>",
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(body))
}
