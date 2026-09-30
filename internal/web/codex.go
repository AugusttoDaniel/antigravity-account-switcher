package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/codex"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/egress"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/onboard"
)

// WithCodexService enables the Codex (ChatGPT) account endpoints.
func WithCodexService(svc *codex.Service) Option {
	return func(c *ServerConfig) { c.CodexService = svc }
}

// CodexAccountView is a Codex account as the dashboard sees it: no tokens, and the proxy without
// its credentials (editing replaces it wholesale, so the secret is never needed back).
type CodexAccountView struct {
	*domain.CodexAccount
	ProxyURL     string `json:"proxy_url,omitempty"`
	ProxyInvalid bool   `json:"proxy_invalid,omitempty"`
}

func newCodexAccountView(a *domain.CodexAccount) *CodexAccountView {
	v := &CodexAccountView{CodexAccount: a}
	if a.ProxyURL != "" {
		if masked, ok := egress.MaskProxyURL(a.ProxyURL); ok {
			v.ProxyURL = masked
		} else {
			v.ProxyInvalid = true
		}
		if egress.ValidateProxyURL(a.ProxyURL) != nil {
			v.ProxyInvalid = true
		}
	}
	return v
}

type codexIDRequest struct {
	ID string `json:"id"`
}

type codexProxyRequest struct {
	ID       string `json:"id"`
	PoolID   string `json:"pool_id"`
	ProxyURL string `json:"proxy_url"`
}

type codexLoginRequest struct {
	PoolID    string `json:"pool_id"`
	ProxyURL  string `json:"proxy_url"`
	Mode      string `json:"mode"` // "link" (default) or "profile"
	ProfileID string `json:"profile_id"`
}

// HandleCodex serves /api/codex/*. Every state change is POST-only.
func (a *APIHandler) HandleCodex(w http.ResponseWriter, r *http.Request) {
	svc := a.codexSvc
	if svc == nil {
		writeErrorJSON(w, http.StatusNotImplemented, "Codex accounts are not enabled", nil)
		return
	}
	route := strings.TrimPrefix(r.URL.Path, "/api/codex/")

	if route == "accounts" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.codexList(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch route {
	case "accounts/switch":
		a.codexSwitch(w, r)
	case "accounts/refresh":
		a.codexRefresh(w, r)
	case "accounts/proxy":
		a.codexSetProxy(w, r)
	case "accounts/remove":
		a.codexRemove(w, r)
	case "login/start":
		a.codexLoginStart(w, r)
	case "login/complete":
		a.codexLoginComplete(w, r)
	default:
		http.NotFound(w, r)
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeErrorJSON(w, http.StatusBadRequest, "invalid request payload", err)
		return false
	}
	return true
}

func (a *APIHandler) codexList(w http.ResponseWriter, r *http.Request) {
	accs, err := a.codexSvc.Repo.List(r.Context())
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to list Codex accounts", err)
		return
	}
	views := make([]*CodexAccountView, 0, len(accs))
	for _, acc := range accs {
		views = append(views, newCodexAccountView(acc))
	}
	writeJSON(w, http.StatusOK, views)
}

func (a *APIHandler) codexAccountFromRequest(w http.ResponseWriter, r *http.Request) (*domain.CodexAccount, bool) {
	var req codexIDRequest
	if !decodeBody(w, r, &req) {
		return nil, false
	}
	acc, err := a.codexSvc.Resolve(r.Context(), req.ID)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, domain.ErrCodexAccountNotFound) {
			status = http.StatusNotFound
		}
		writeErrorJSON(w, status, "account not found", err)
		return nil, false
	}
	return acc, true
}

func (a *APIHandler) codexSwitch(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.codexAccountFromRequest(w, r)
	if !ok {
		return
	}
	switched, err := a.codexSvc.Switch(r.Context(), acc.ID)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "could not switch the Codex account", err)
		return
	}
	a.broadcastOAuth("account_switched", switched.ID, fmt.Sprintf("Codex CLI now uses %s", switched.Email))
	writeJSON(w, http.StatusOK, map[string]any{"status": "switched", "account": newCodexAccountView(switched)})
}

func (a *APIHandler) codexRefresh(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.codexAccountFromRequest(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	refreshed, err := a.codexSvc.Refresh(ctx, acc.ID, codex.RefreshOptions{})
	switch {
	case errors.Is(err, codex.ErrProxyRequired):
		writeErrorJSON(w, http.StatusConflict, "this account has no proxy", err)
	case errors.Is(err, codex.ErrInvalidGrant):
		writeErrorJSON(w, http.StatusUnauthorized, "the account must sign in again", err)
	case err != nil:
		writeErrorJSON(w, http.StatusBadGateway, "could not refresh the tokens", err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"status": "refreshed", "account": newCodexAccountView(refreshed)})
	}
}

func (a *APIHandler) codexSetProxy(w http.ResponseWriter, r *http.Request) {
	var req codexProxyRequest
	if !decodeBody(w, r, &req) {
		return
	}
	proxyURL, ok := a.proxyFromRequest(w, req.PoolID, req.ProxyURL)
	if !ok {
		return
	}
	acc, err := a.codexSvc.Resolve(r.Context(), req.ID)
	if err != nil {
		writeErrorJSON(w, http.StatusNotFound, "account not found", err)
		return
	}
	if err := a.codexSvc.Repo.UpdateProxyURL(r.Context(), acc.ID, proxyURL); err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "could not save the proxy", err)
		return
	}
	updated, _ := a.codexSvc.Repo.GetByID(r.Context(), acc.ID)
	writeJSON(w, http.StatusOK, map[string]any{"status": "updated", "account": newCodexAccountView(updated)})
}

func (a *APIHandler) codexRemove(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.codexAccountFromRequest(w, r)
	if !ok {
		return
	}
	if err := a.codexSvc.Repo.Delete(r.Context(), acc.ID); err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "could not remove the account", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// proxyFromRequest resolves a pool id or a literal proxy URL and validates it. A proxy is always
// required: anything else would make OpenAI see the machine's real IP.
func (a *APIHandler) proxyFromRequest(w http.ResponseWriter, poolID, proxyURL string) (string, bool) {
	proxyURL = strings.TrimSpace(proxyURL)
	if id := strings.TrimSpace(poolID); id != "" {
		resolved, err := a.resolvePoolProxy(id)
		if err != nil {
			writeErrorJSON(w, http.StatusBadRequest, "invalid pool_id", err)
			return "", false
		}
		proxyURL = resolved
	}
	if proxyURL == "" {
		writeErrorJSON(w, http.StatusBadRequest, "a proxy is required",
			errors.New("pick a proxy from the pool: the sign-in and every token refresh would otherwise leave from your real IP"))
		return "", false
	}
	if err := egress.ValidateProxyURL(proxyURL); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, "invalid proxy_url", err)
		return "", false
	}
	return proxyURL, true
}

// codexLoginStart starts a sign-in in the background. Only one can run at a time: the OAuth
// callback port is fixed.
func (a *APIHandler) codexLoginStart(w http.ResponseWriter, r *http.Request) {
	var req codexLoginRequest
	if !decodeBody(w, r, &req) {
		return
	}
	proxyURL, ok := a.proxyFromRequest(w, req.PoolID, req.ProxyURL)
	if !ok {
		return
	}
	masked, _ := egress.MaskProxyURL(proxyURL)
	if req.Mode != "" && req.Mode != "link" && req.Mode != "profile" {
		writeErrorJSON(w, http.StatusBadRequest, "unknown mode", fmt.Errorf("mode %q: use \"link\" or \"profile\"", req.Mode))
		return
	}
	if !atomic.CompareAndSwapInt32(&a.codexLoginBusy, 0, 1) {
		writeErrorJSON(w, http.StatusConflict, "a Codex sign-in is already in progress",
			errors.New("finish or wait for the current one: the OAuth callback port is shared"))
		return
	}
	release := func() { atomic.StoreInt32(&a.codexLoginBusy, 0) }

	client, err := a.codexSvc.NewClient(proxyURL)
	if err != nil {
		release()
		writeErrorJSON(w, http.StatusBadRequest, "invalid proxy", err)
		return
	}
	port := codex.CallbackPort
	if a.codexRandomPort {
		port = 0
	}
	if a.codexPort > 0 {
		port = a.codexPort
	}
	opts := codex.LoginOptions{Client: client, Port: port, Timeout: codex.DefaultLoginTimeout}
	profileID := ""
	var sess *onboard.Session

	if req.Mode == "profile" {
		prepCtx, cancel := context.WithTimeout(r.Context(), profilePrepareBudget)
		defer cancel()
		conn, _, err := a.connectProfileAPI(prepCtx)
		if err != nil {
			release()
			writeErrorJSON(w, http.StatusBadGateway, "the browser-profile API is not available", scrubProxyCredentials(err, proxyURL))
			return
		}
		profileID = strings.TrimSpace(req.ProfileID)
		if profileID == "" {
			profileID, err = onboard.CreateProfile(prepCtx, conn.api, "codex-"+time.Now().UTC().Format("20060102-150405"), conn.engine, proxyURL)
			if err != nil {
				release()
				writeErrorJSON(w, http.StatusBadGateway, "could not create the browser profile", scrubProxyCredentials(err, proxyURL))
				return
			}
		}
		sess, err = onboard.Start(prepCtx, conn.api, profileID, proxyURL)
		if err != nil {
			release()
			writeErrorJSON(w, http.StatusBadGateway, "could not open the profile's browser", scrubProxyCredentials(err, proxyURL))
			return
		}
		opts.Opener = sess.Opener(context.Background(), a.profileNavigate)
	} else {
		opts.Opener = func(string) error { return nil } // the link goes back to the user instead
	}

	urlChan := make(chan string, 1)
	earlyErr := make(chan error, 1)
	var manual int32
	pasteCh := make(chan codex.Paste, 4)
	opts.Pasted = pasteCh
	opts.OnManual = func(reason error) {
		atomic.StoreInt32(&manual, 1)
		a.broadcastOAuth("oauth_started", "", "The OAuth callback port cannot be opened on this machine: sign in, then paste the address of the page that fails to load")
	}
	opts.URLLogger = func(u string) {
		select {
		case urlChan <- u:
		default:
		}
		a.broadcastOAuth("oauth_started", "", fmt.Sprintf("Codex sign-in started through %s", masked))
	}

	a.setCodexPaste(pasteCh)
	go func() {
		defer release()
		defer a.setCodexPaste(nil)
		if sess != nil {
			defer sess.Close()
		}
		fail := func(err error) {
			select {
			case earlyErr <- err:
			default:
			}
			a.broadcastOAuthError(fmt.Sprintf("Codex sign-in failed: %v", err))
		}
		tr, err := codex.Login(context.Background(), opts)
		if err != nil {
			fail(err)
			return
		}
		acc, err := a.codexSvc.AddFromTokens(context.Background(), tr, proxyURL, profileID)
		if err != nil {
			fail(err)
			return
		}
		a.broadcastOAuth("oauth_completed", acc.ID, fmt.Sprintf("Codex account %s added through %s", acc.Email, masked))
	}()

	var authURL string
	select {
	case authURL = <-urlChan:
	case err := <-earlyErr:
		// The sign-in died before it could issue a link: say so instead of "started".
		writeErrorJSON(w, http.StatusBadGateway, "the Codex sign-in could not start", scrubProxyCredentials(err, proxyURL))
		return
	case <-time.After(2 * time.Second):
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "started", "mode": modeOrLink(req.Mode), "profile_id": profileID, "proxy": masked,
		"auth_url": authURL, "manual": atomic.LoadInt32(&manual) == 1,
	})
}

func (a *APIHandler) setCodexPaste(ch chan codex.Paste) {
	a.codexPasteMu.Lock()
	a.codexPasteCh = ch
	a.codexPasteMu.Unlock()
}

type codexCompleteRequest struct {
	URL string `json:"url"`
}

// codexLoginComplete finishes a sign-in whose callback port could not be opened: the user pastes
// the address of the page that failed to load. A bad paste (typo, wrong sign-in) leaves the
// sign-in waiting so they can try again.
func (a *APIHandler) codexLoginComplete(w http.ResponseWriter, r *http.Request) {
	var req codexCompleteRequest
	if !decodeBody(w, r, &req) {
		return
	}
	a.codexPasteMu.Lock()
	ch := a.codexPasteCh
	a.codexPasteMu.Unlock()
	if ch == nil {
		writeErrorJSON(w, http.StatusConflict, "no Codex sign-in is waiting", errors.New("start a sign-in first (it may also have timed out)"))
		return
	}
	result := make(chan error, 1)
	select {
	case ch <- codex.Paste{URL: req.URL, Result: result}:
	default:
		writeErrorJSON(w, http.StatusConflict, "too many pending pastes", nil)
		return
	}
	select {
	case err := <-result:
		if err != nil {
			writeErrorJSON(w, http.StatusBadRequest, "that address could not complete the sign-in", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "completed"})
	case <-time.After(45 * time.Second):
		writeErrorJSON(w, http.StatusGatewayTimeout, "the sign-in did not answer in time", nil)
	case <-r.Context().Done():
	}
}

func modeOrLink(m string) string {
	if m == "" {
		return "link"
	}
	return m
}
