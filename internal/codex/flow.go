package codex

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// DefaultLoginTimeout bounds a sign-in a human completes by hand (2-step verification included).
const DefaultLoginTimeout = 15 * time.Minute

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
}

type loginResult struct {
	tokens *TokenResponse
	err    error
}

// Login runs the PKCE authorization-code flow through a loopback listener and returns the tokens.
// The exchange goes through opts.Client.HTTP, so a proxy-bound client keeps it off the real IP.
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

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.Port)))
	if err != nil {
		return nil, fmt.Errorf("the OAuth callback port %d is busy (is a Codex login already running?): %w", opts.Port, err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	redirect := RedirectURI(port)

	loginCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	resultCh := make(chan loginResult, 1)
	var once sync.Once
	finish := func(r loginResult) { once.Do(func() { resultCh <- r }) }

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
		tr, err := opts.Client.Exchange(loginCtx, code, pkce.Verifier, redirect)
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
	defer func() {
		sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer scancel()
		_ = srv.Shutdown(sctx)
	}()

	authURL := opts.Client.AuthorizeURL(redirect, state, pkce)
	if opts.URLLogger != nil {
		opts.URLLogger(authURL)
	}
	if opts.Opener != nil {
		if err := opts.Opener(authURL); err != nil && opts.URLLogger == nil {
			return nil, fmt.Errorf("could not open the sign-in page: %w", err)
		}
	}

	select {
	case r := <-resultCh:
		return r.tokens, r.err
	case <-loginCtx.Done():
		return nil, errors.New("the sign-in timed out or was cancelled")
	}
}

func writePage(w http.ResponseWriter, status int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>%s</title><body style=\"font-family:sans-serif;margin:3rem\"><h2>%s</h2><p>%s</p>",
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(body))
}
