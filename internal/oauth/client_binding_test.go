package oauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/store/sqlite"
)

// fakeGoogle answers like Google's token endpoint: a client authenticates with its own secret
// (invalid_client otherwise), and a refresh token is bound to the client that issued it
// (unauthorized_client for any other, even with the right secret).
type fakeGoogle struct {
	mu       sync.Mutex
	requests []string // "client_id|refresh_token"
}

var (
	googleSecrets = map[string]string{"client-884": "sec-884", "client-1071": "sec-1071"}
	// token -> issuing client ("" = revoked)
	googleIssuers = map[string]string{"rt-884": "client-884", "rt-1071": "client-1071", "rt-revoked": ""}
)

func (g *fakeGoogle) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		client, secret, rt := r.FormValue("client_id"), r.FormValue("client_secret"), r.FormValue("refresh_token")
		g.mu.Lock()
		g.requests = append(g.requests, client+"|"+rt)
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if googleSecrets[client] == "" || googleSecrets[client] != secret {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		issuer, known := googleIssuers[rt]
		if !known || issuer == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		if issuer != client {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized_client"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"at-new","expires_in":3600,"token_type":"Bearer"}`))
	}
}

func (g *fakeGoogle) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.requests)
}

func bindingEnv(t *testing.T, ids ...string) (*OAuthService, *fakeGoogle, *sqlite.AccountRepository) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	repo := sqlite.NewAccountRepository(db)
	g := &fakeGoogle{}
	srv := httptest.NewServer(g.handler())
	t.Cleanup(srv.Close)
	svc := NewOAuthService(repo,
		WithTokenURL(srv.URL),
		WithClientID(ids[0]),
		WithCredentialCandidates(ids, []string{"sec-884", "sec-1071"}),
	)
	return svc, g, repo
}

func addAccount(t *testing.T, repo *sqlite.AccountRepository, email, rt, client string) *domain.Account {
	t.Helper()
	now := time.Now().UTC()
	acc := &domain.Account{Email: email, RefreshToken: rt, OAuthClientID: client, Status: domain.AccountStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := repo.Create(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	return acc
}

// The bug this guards: tokens issued by one Antigravity client were renewed with another, which
// Google refuses (unauthorized_client) forever. An account of unknown origin must find its client.
func TestRefresh_LearnsWhichClientIssuedTheToken(t *testing.T) {
	svc, _, repo := bindingEnv(t, "client-1071", "client-884") // the default client is the WRONG one for rt-884
	acc := addAccount(t, repo, "a@example.com", "rt-884", "")

	resp, err := svc.RefreshTokenVia(context.Background(), "rt-884", "")
	if err != nil {
		t.Fatal(err)
	}
	if resp.ClientID != "client-884" || resp.AccessToken != "at-new" {
		t.Fatalf("resp = %+v", resp)
	}
	got, _ := repo.GetByID(context.Background(), acc.ID)
	if got.OAuthClientID != "client-884" {
		t.Fatalf("the learned client was not recorded: %q", got.OAuthClientID)
	}
}

func TestRefresh_ABoundAccountUsesOnlyItsOwnClient(t *testing.T) {
	svc, g, repo := bindingEnv(t, "client-1071", "client-884")
	addAccount(t, repo, "a@example.com", "rt-884", "client-884")

	if _, err := svc.RefreshTokenVia(context.Background(), "rt-884", ""); err != nil {
		t.Fatal(err)
	}
	for _, r := range g.requests {
		if r[:len("client-884")] != "client-884" {
			t.Fatalf("a bound account tried another client: %v", g.requests)
		}
	}
}

// A recorded client is authoritative: if Google refuses it, falling back to other clients would only
// hide a wrong record.
func TestRefresh_ABoundAccountDoesNotFallBackToOtherClients(t *testing.T) {
	svc, g, repo := bindingEnv(t, "client-1071", "client-884")
	acc := addAccount(t, repo, "a@example.com", "rt-884", "client-1071") // wrongly recorded

	if _, err := svc.RefreshTokenVia(context.Background(), "rt-884", ""); err == nil {
		t.Fatal("expected an error")
	}
	for _, r := range g.requests {
		if r[:len("client-1071")] != "client-1071" {
			t.Fatalf("it fell back to another client: %v", g.requests)
		}
	}
	if got, _ := repo.GetByID(context.Background(), acc.ID); got.OAuthClientID != "client-1071" {
		t.Fatalf("a failed refresh changed the recorded client to %q", got.OAuthClientID)
	}
}

func TestRefresh_ALearnedClientIsNotProbedAgain(t *testing.T) {
	svc, g, repo := bindingEnv(t, "client-1071", "client-884")
	addAccount(t, repo, "a@example.com", "rt-884", "")
	if _, err := svc.RefreshTokenVia(context.Background(), "rt-884", ""); err != nil {
		t.Fatal(err)
	}
	before := g.count()
	if _, err := svc.RefreshTokenVia(context.Background(), "rt-884", ""); err != nil {
		t.Fatal(err)
	}
	if n := g.count() - before; n != 1 {
		t.Fatalf("the second refresh made %d requests, want 1", n)
	}
}

func TestRefresh_ARevokedTokenStopsAtOnce(t *testing.T) {
	svc, g, repo := bindingEnv(t, "client-1071", "client-884")
	addAccount(t, repo, "a@example.com", "rt-revoked", "")
	_, err := svc.RefreshTokenVia(context.Background(), "rt-revoked", "")
	if !errors.Is(err, domain.ErrInvalidRefreshToken) {
		t.Fatalf("err = %v, want ErrInvalidRefreshToken", err)
	}
	if g.count() > 2 { // at most the secret probe on the first client
		t.Fatalf("a revoked token kept probing: %d requests", g.count())
	}
}

func TestProbeRefresh(t *testing.T) {
	svc, _, _ := bindingEnv(t, "client-1071", "client-884")
	ctx := context.Background()
	cases := []struct {
		name, rt, client string
		want             ProbeResult
	}{
		{"the issuing client", "rt-884", "client-884", ProbeOK},
		{"another real client", "rt-884", "client-1071", ProbeMismatch},
		{"a revoked token", "rt-revoked", "client-884", ProbeRevoked},
		{"a client no secret pairs with", "rt-884", "client-unknown", ProbeInconclusive},
		{"no client given", "rt-884", "", ProbeInconclusive},
		{"no token given", "", "client-884", ProbeInconclusive},
	}
	for _, c := range cases {
		if got := svc.ProbeRefresh(ctx, c.rt, c.client, ""); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestProbeRefresh_NeverStoresAnything(t *testing.T) {
	svc, _, repo := bindingEnv(t, "client-1071", "client-884")
	acc := addAccount(t, repo, "a@example.com", "rt-884", "")
	if svc.ProbeRefresh(context.Background(), "rt-884", "client-884", "") != ProbeOK {
		t.Fatal("probe failed")
	}
	got, _ := repo.GetByID(context.Background(), acc.ID)
	if got.OAuthClientID != "" || got.AccessToken != "" {
		t.Fatalf("a probe wrote to the account: %+v", got)
	}
}

func TestUpsertAccount_RecordsTheClientThatIssuedTheTokens(t *testing.T) {
	svc, _, repo := bindingEnv(t, "client-1071", "client-884")
	ctx := context.Background()

	created, err := svc.UpsertAccount(ctx, "new@example.com", "at", "rt-1071", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if created.OAuthClientID != "client-1071" {
		t.Fatalf("new account client = %q", created.OAuthClientID)
	}
	if got, _ := repo.GetByID(ctx, created.ID); got.OAuthClientID != "client-1071" {
		t.Fatalf("stored client = %q", got.OAuthClientID)
	}

	// Signing the same account in again through another client replaces the record: the new refresh
	// token was issued by that client.
	other, _, repo2 := bindingEnv(t, "client-884", "client-1071")
	addAccount(t, repo2, "old@example.com", "rt-884", "client-1071")
	again, err := other.UpsertAccount(ctx, "old@example.com", "at", "rt-884", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := repo2.GetByID(ctx, again.ID); got.OAuthClientID != "client-884" {
		t.Fatalf("re-login client = %q, want client-884", got.OAuthClientID)
	}
}

func TestPinningDoesNotHideOtherClientsFromRefresh(t *testing.T) {
	t.Setenv("ANTIGRAVITY_CLIENT_ID", "pinned-client")
	t.Setenv("ANTIGRAVITY_CLIENT_SECRET", "pinned-secret")
	ids, _ := ResolveCredentialCandidates()
	if len(ids) != 1 || ids[0] != "pinned-client" {
		t.Fatalf("a pinned id must reduce the sign-in candidates to itself, got %v", ids)
	}
	all := ResolveAllClientIDs()
	found := false
	for _, id := range all {
		if id == "pinned-client" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ResolveAllClientIDs lost the pinned id: %v", all)
	}
}
