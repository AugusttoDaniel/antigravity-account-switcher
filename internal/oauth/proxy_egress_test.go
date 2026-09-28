package oauth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestRefreshTokenVia_EgressesThroughProxy proves that a non-empty proxyURL causes the token
// refresh to leave through the proxy rather than connecting to the token endpoint directly.
// This is the regression guard for the IP leak: background renewal of a proxied account must
// never touch Google from the operator's real connection.
func TestRefreshTokenVia_EgressesThroughProxy(t *testing.T) {
	var directHits, proxyHits int32

	// The token endpoint. If the refresh egresses directly this handler is hit.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&directHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"direct-access","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer origin.Close()

	// A forward proxy: for a plain-HTTP origin the transport sends the full absolute-URI request
	// here, so we can both assert the hit and answer as the origin would.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&proxyHits, 1)
		if r.Method != http.MethodPost {
			t.Errorf("proxy: expected POST, got %s", r.Method)
		}
		if got := r.URL.Path; got != "/token" {
			t.Errorf("proxy: expected path /token, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"proxied-access","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer proxy.Close()

	svc := NewOAuthService(nil,
		WithTokenURL(origin.URL+"/token"),
		WithClientID("client-id"),
		WithClientSecret("client-secret"),
	)

	resp, err := svc.RefreshTokenVia(context.Background(), "refresh-token", proxy.URL)
	if err != nil {
		t.Fatalf("RefreshTokenVia: %v", err)
	}
	if resp.AccessToken != "proxied-access" {
		t.Errorf("expected token from proxy path, got %q", resp.AccessToken)
	}
	if got := atomic.LoadInt32(&proxyHits); got != 1 {
		t.Errorf("expected proxy to be hit once, got %d", got)
	}
	if got := atomic.LoadInt32(&directHits); got != 0 {
		t.Errorf("expected token endpoint NOT to be hit directly, got %d hits (real IP leak)", got)
	}
}

// TestRefreshToken_DirectWhenNoProxy proves the default (empty proxy) path still connects
// straight to the token endpoint, so the proxy plumbing does not alter existing behaviour.
func TestRefreshToken_DirectWhenNoProxy(t *testing.T) {
	var directHits int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&directHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"direct-access","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer origin.Close()

	svc := NewOAuthService(nil,
		WithTokenURL(origin.URL+"/token"),
		WithClientID("client-id"),
		WithClientSecret("client-secret"),
	)

	resp, err := svc.RefreshToken(context.Background(), "refresh-token")
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if resp.AccessToken != "direct-access" {
		t.Errorf("expected direct token, got %q", resp.AccessToken)
	}
	if got := atomic.LoadInt32(&directHits); got != 1 {
		t.Errorf("expected token endpoint hit once directly, got %d", got)
	}
}

// TestClientForProxy_CachesPerURL verifies distinct proxy URLs get distinct cached clients and
// that the same URL returns the identical cached client, while an empty URL returns the direct one.
func TestClientForProxy_CachesPerURL(t *testing.T) {
	svc := NewOAuthService(nil,
		WithClientID("client-id"),
		WithClientSecret("client-secret"),
	)

	if c := svc.clientForProxy(""); c != svc.client {
		t.Error("empty proxy URL must return the direct client")
	}

	a1 := svc.clientForProxy("http://user:pass@proxy-a.example:8080")
	a2 := svc.clientForProxy("http://user:pass@proxy-a.example:8080")
	b := svc.clientForProxy("http://proxy-b.example:3128")

	if a1 == nil || a1 != a2 {
		t.Error("same proxy URL must return the same cached client")
	}
	if a1 == b {
		t.Error("different proxy URLs must return different clients")
	}
	if a1 == svc.client || b == svc.client {
		t.Error("a proxied client must not be the direct client")
	}
}
