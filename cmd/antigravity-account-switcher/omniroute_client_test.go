package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/oauth"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/store/sqlite"
)

const (
	issuerClient = "884354919052-issuer.apps.googleusercontent.com"
	omniClient   = "1071006060591-omni.apps.googleusercontent.com"
)

// googleAsProxy answers the way Google's token endpoint does, and is used AS the account's proxy:
// the refresh request reaches it in absolute form whatever the token URL is, so no real network is
// involved. A token is bound to the client that issued it (unauthorized_client for any other).
func googleAsProxy(t *testing.T) string {
	t.Helper()
	secrets := map[string]string{issuerClient: "sec-issuer", omniClient: "sec-omni"}
	issuers := map[string]string{"rt-issuer": issuerClient, "rt-omni": omniClient, "rt-revoked": ""}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		client, secret, rt := r.FormValue("client_id"), r.FormValue("client_secret"), r.FormValue("refresh_token")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case secrets[client] == "" || secrets[client] != secret:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
		case issuers[rt] == "":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		case issuers[rt] != client:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized_client"}`))
		default:
			_, _ = w.Write([]byte(`{"access_token":"at","expires_in":3600}`))
		}
	}))
	t.Cleanup(srv.Close)
	return "http://u:p@" + strings.TrimPrefix(srv.URL, "http://")
}

func clientCheckEnv(t *testing.T) (*oauth.OAuthService, *sqlite.AccountRepository) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	repo := sqlite.NewAccountRepository(db)
	svc := oauth.NewOAuthService(repo,
		oauth.WithTokenURL("http://127.0.0.1:1/token"),
		oauth.WithClientID(issuerClient),
		oauth.WithCredentialCandidates([]string{issuerClient, omniClient}, []string{"sec-issuer", "sec-omni"}),
	)
	return svc, repo
}

func newAcc(t *testing.T, repo *sqlite.AccountRepository, email, rt, client, proxy string) *domain.Account {
	t.Helper()
	now := time.Now().UTC()
	a := &domain.Account{Email: email, RefreshToken: rt, OAuthClientID: client, ProxyURL: proxy, Status: domain.AccountStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := repo.Create(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestOmniRouteClientProblem_KnownClient(t *testing.T) {
	svc, repo := clientCheckEnv(t)
	ctx := context.Background()

	ok := newAcc(t, repo, "ok@example.com", "rt-omni", omniClient, "")
	if got := omniRouteClientProblem(ctx, svc, repo, ok, omniClient); got != "" {
		t.Fatalf("an account issued by OmniRoute's client was refused: %q", got)
	}

	bad := newAcc(t, repo, "bad@example.com", "rt-issuer", issuerClient, "")
	got := omniRouteClientProblem(ctx, svc, repo, bad, omniClient)
	for _, want := range []string{"884354919052-...", "1071006060591-...", "could never renew", "config set oauth_client_id " + omniClient} {
		if !strings.Contains(got, want) {
			t.Errorf("the explanation lacks %q: %s", want, got)
		}
	}
	if strings.Contains(got, "issuer.apps.googleusercontent.com") {
		t.Errorf("the message prints a full client id: %s", got)
	}
}

func TestOmniRouteClientProblem_UnknownOriginIsCheckedThroughItsProxy(t *testing.T) {
	svc, repo := clientCheckEnv(t)
	ctx := context.Background()
	proxy := googleAsProxy(t)

	// Issued by OmniRoute's own client: the check passes and the issuer is remembered.
	fine := newAcc(t, repo, "fine@example.com", "rt-omni", "", proxy)
	if got := omniRouteClientProblem(ctx, svc, repo, fine, omniClient); got != "" {
		t.Fatalf("refused an account OmniRoute can renew: %q", got)
	}
	if stored, _ := repo.GetByID(ctx, fine.ID); stored.OAuthClientID != omniClient {
		t.Fatalf("the proven client was not recorded: %q", stored.OAuthClientID)
	}

	// Issued by another client: Google refuses OmniRoute's, so the export is refused with the fix.
	other := newAcc(t, repo, "other@example.com", "rt-issuer", "", proxy)
	got := omniRouteClientProblem(ctx, svc, repo, other, omniClient)
	if !strings.Contains(got, "could never renew") || !strings.Contains(got, "config set oauth_client_id") {
		t.Fatalf("mismatch = %q", got)
	}
	if stored, _ := repo.GetByID(ctx, other.ID); stored.OAuthClientID != "" {
		t.Fatalf("a failed check recorded a client: %q", stored.OAuthClientID)
	}

	// A revoked token is not a client problem: it says so.
	dead := newAcc(t, repo, "dead@example.com", "rt-revoked", "", proxy)
	if got := omniRouteClientProblem(ctx, svc, repo, dead, omniClient); !strings.Contains(got, "revoked or expired") {
		t.Fatalf("revoked = %q", got)
	}
}

// The check calls Google with the account's token, so it must never do it from this machine's IP.
func TestOmniRouteClientProblem_NeverChecksWithoutAProxy(t *testing.T) {
	svc, repo := clientCheckEnv(t)
	acc := newAcc(t, repo, "noproxy@example.com", "rt-omni", "", "")
	got := omniRouteClientProblem(context.Background(), svc, repo, acc, omniClient)
	if !strings.Contains(got, "no proxy") || !strings.Contains(got, "real IP") {
		t.Fatalf("got %q", got)
	}
}

func TestOmniRouteClientProblem_AnUnsettledAnswerBlocksUnlessSkipped(t *testing.T) {
	svc, repo := clientCheckEnv(t)
	// No secret pairs with this OmniRoute client id, so nothing can be concluded.
	acc := newAcc(t, repo, "u@example.com", "rt-issuer", "", googleAsProxy(t))
	got := omniRouteClientProblem(context.Background(), svc, repo, acc, "unknown-client-id")
	if !strings.Contains(got, "could not confirm") || !strings.Contains(got, "--skip-client-check") {
		t.Fatalf("got %q", got)
	}
}
