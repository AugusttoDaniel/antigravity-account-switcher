package omniroute_test

import (
	"context"
	"testing"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/omniroute"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/omniroute/omnitest"
)

func TestOAuth_StartAndExchangeCreateTheConnectionUnderTheProvider(t *testing.T) {
	fake := omnitest.New(t)
	fake.OAuthEmail = "new@example.com"
	c := omniroute.NewClient(omniroute.WithBaseURL(fake.URL))
	ctx := context.Background()

	start, err := c.StartOAuth(ctx, "antigravity")
	if err != nil || start.AuthURL == "" || start.State == "" || start.CodeVerifier == "" {
		t.Fatalf("start = %+v, %v", start, err)
	}
	conn, err := c.ExchangeOAuth(ctx, "antigravity", omniroute.OAuthExchange{
		Code: "the-code", RedirectURI: start.RedirectURI, CodeVerifier: start.CodeVerifier, State: start.State,
	})
	if err != nil || conn.ID == "" || conn.Email != "new@example.com" {
		t.Fatalf("exchange = %+v, %v", conn, err)
	}
	if got := fake.OAuthExchanges(); len(got) != 1 || got[0] != "antigravity" {
		t.Fatalf("exchanges = %v", got)
	}
	list, err := c.ListConnections(ctx, "antigravity")
	if err != nil || len(list) != 1 || list[0].Email != "new@example.com" {
		t.Fatalf("connections under antigravity = %+v, %v", list, err)
	}
	if agy, _ := c.ListConnections(ctx, "agy"); len(agy) != 0 {
		t.Fatalf("nothing may be created under agy: %+v", agy)
	}
}

func TestOAuth_ExchangeRefusedWithoutTheVerifier(t *testing.T) {
	fake := omnitest.New(t)
	c := omniroute.NewClient(omniroute.WithBaseURL(fake.URL))
	if _, err := c.ExchangeOAuth(context.Background(), "antigravity", omniroute.OAuthExchange{Code: "x", RedirectURI: "http://localhost:8080/callback"}); err == nil {
		t.Fatal("an exchange without the PKCE verifier must be refused")
	}
	if len(fake.OAuthExchanges()) != 0 {
		t.Fatal("a refused exchange created a connection")
	}
}

func TestCallbackCode(t *testing.T) {
	good := "http://localhost:8080/callback?code=4%2F0abc&state=S1&scope=x"
	if code, err := omniroute.CallbackCode(" "+good+" ", "S1"); err != nil || code != "4/0abc" {
		t.Fatalf("code = %q, %v", code, err)
	}
	for name, raw := range map[string]string{
		"wrong state":    "http://localhost:8080/callback?code=c&state=OTHER",
		"google refusal": "http://localhost:8080/callback?error=access_denied&state=S1",
		"no code":        "http://localhost:8080/callback?state=S1",
		"not an address": "hello",
		"empty":          "",
	} {
		if code, err := omniroute.CallbackCode(raw, "S1"); err == nil || code != "" {
			t.Errorf("%s: accepted (%q)", name, code)
		}
	}
}
