package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestExchangeCode_ProbesSecretsOnInvalidClient guards the client-secret pairing probe: the
// Antigravity bundle yields several secrets with no reliable pairing, so the exchange must try each
// on an "invalid_client" response until one works, then cache it.
func TestExchangeCode_ProbesSecretsOnInvalidClient(t *testing.T) {
	const correct = "correct-secret"
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		attempts++
		if r.FormValue("client_secret") == correct {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"ok","refresh_token":"rt","expires_in":3600,"token_type":"Bearer"}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"The provided client secret is invalid."}`))
	}))
	defer srv.Close()

	svc := NewOAuthService(nil,
		WithTokenURL(srv.URL),
		WithClientID("client-id"),
		WithCredentialCandidates([]string{"client-id"}, []string{"wrong-1", "wrong-2", correct}),
	)

	resp, err := svc.ExchangeCode(context.Background(), "code", "verifier", "http://127.0.0.1:1/callback")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if resp.AccessToken != "ok" {
		t.Errorf("access token: %q", resp.AccessToken)
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts (2 wrong + 1 correct), got %d", attempts)
	}
	if svc.cfg.ClientSecret != correct {
		t.Errorf("working secret must be cached, got %q", svc.cfg.ClientSecret)
	}
}

// TestExchangeCode_AllSecretsInvalidClient: if every candidate fails auth, the exchange errors.
func TestExchangeCode_AllSecretsInvalidClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
	}))
	defer srv.Close()

	svc := NewOAuthService(nil,
		WithTokenURL(srv.URL),
		WithClientID("client-id"),
		WithCredentialCandidates([]string{"client-id"}, []string{"a", "b"}),
	)
	if _, err := svc.ExchangeCode(context.Background(), "code", "v", "http://127.0.0.1:1/callback"); err == nil {
		t.Error("expected an error when all secrets fail with invalid_client")
	}
}

// TestExchangeCode_NonInvalidClientStopsProbe: a non-invalid_client failure (e.g. invalid_grant)
// means the pairing is fine, so probing must stop after the first attempt rather than burning the
// single-use authorization code against every secret.
func TestExchangeCode_NonInvalidClientStopsProbe(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	svc := NewOAuthService(nil,
		WithTokenURL(srv.URL),
		WithClientID("client-id"),
		WithCredentialCandidates([]string{"client-id"}, []string{"a", "b", "c"}),
	)
	if _, err := svc.ExchangeCode(context.Background(), "code", "v", "http://127.0.0.1:1/callback"); err == nil {
		t.Error("expected an error")
	}
	if attempts != 1 {
		t.Errorf("a non-invalid_client error must stop probing after 1 attempt, got %d", attempts)
	}
}
