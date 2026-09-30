package codex

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/omniroute"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/omniroute/omnitest"
)

func exportEnv(t *testing.T) (*svcEnv, *omnitest.Server, *omniroute.Client) {
	t.Helper()
	e := newSvcEnv(t)
	fake := omnitest.New(t)
	return e, fake, omniroute.NewClient(omniroute.WithBaseURL(fake.URL))
}

func (e *svcEnv) all(t *testing.T) []*domain.CodexAccount {
	t.Helper()
	l, err := e.repo.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestExportHandsTheAccountOverAndMarksIt(t *testing.T) {
	e, fake, c := exportEnv(t)
	a := e.add(t, "a@example.com", "acct-a", "http://u:p@127.0.0.1:7001")

	res, err := e.svc.ExportToOmniRoute(context.Background(), c, e.all(t), ExportOptions{AssignProxies: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Outcome != ExportExported {
		t.Fatalf("results = %+v", res)
	}
	if !strings.HasPrefix(res[0].Proxy, "bound") {
		t.Fatalf("proxy = %q, want it bound", res[0].Proxy)
	}

	imps := fake.CodexImports()
	if len(imps) != 1 || imps[0].Email != "a@example.com" || imps[0].RefreshToken != "rt-a@example.com" ||
		imps[0].AccessToken != "at-a@example.com" || imps[0].AccountID != "acct-a" || imps[0].IDToken == "" || imps[0].Overwrite {
		t.Fatalf("import = %+v", imps)
	}
	got, _ := e.repo.GetByID(context.Background(), a.ID)
	if got.OmniRouteExportedAt.IsZero() {
		t.Fatal("the account was not marked as handed off")
	}
	if p, ok := fake.BoundProxy("codex-conn-1"); !ok || p.Host != "127.0.0.1" || p.Port != 7001 {
		t.Fatalf("bound proxy = %+v, %v", p, ok)
	}
}

func TestExportDoesNotRefreshTokensFirst(t *testing.T) {
	e, _, c := exportEnv(t)
	var refreshes int32
	e.issuer(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&refreshes, 1)
		_, _ = w.Write([]byte(`{"access_token":"x"}`))
	})
	e.add(t, "a@example.com", "acct-a", "http://u:p@127.0.0.1:7001")
	if _, err := e.svc.ExportToOmniRoute(context.Background(), c, e.all(t), ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&refreshes) != 0 {
		t.Fatal("exporting rotated the refresh token")
	}
}

func TestExportIsIdempotentAndNeverResendsStaleTokens(t *testing.T) {
	e, fake, c := exportEnv(t)
	e.add(t, "a@example.com", "acct-a", "http://u:p@127.0.0.1:7001")
	ctx := context.Background()
	if _, err := e.svc.ExportToOmniRoute(ctx, c, e.all(t), ExportOptions{AssignProxies: true}); err != nil {
		t.Fatal(err)
	}

	// Second run, even with Overwrite: what we hold is stale now, so nothing may be sent.
	res, err := e.svc.ExportToOmniRoute(ctx, c, e.all(t), ExportOptions{Overwrite: true, AssignProxies: true})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Outcome != ExportSkipped || !strings.Contains(res[0].Detail, "already handed") {
		t.Fatalf("second run = %+v", res[0])
	}
	if n := len(fake.CodexImports()); n != 1 {
		t.Fatalf("%d imports, want still 1", n)
	}
	if res[0].Proxy != "already in effect" {
		t.Fatalf("proxy = %q (binding a handed-off account must still work)", res[0].Proxy)
	}
}

func TestExportLeavesAnAccountOmniRouteAlreadyHas(t *testing.T) {
	e, fake, c := exportEnv(t)
	a := e.add(t, "a@example.com", "acct-a", "http://u:p@127.0.0.1:7001")
	fake.AddConnection("codex", "conn-own", "A@Example.com") // from OmniRoute's own login

	res, err := e.svc.ExportToOmniRoute(context.Background(), c, e.all(t), ExportOptions{AssignProxies: true})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Outcome != ExportExists {
		t.Fatalf("outcome = %s (%s)", res[0].Outcome, res[0].Detail)
	}
	if len(fake.CodexImports()) != 0 {
		t.Fatal("tokens were sent for an account OmniRoute already has")
	}
	got, _ := e.repo.GetByID(context.Background(), a.ID)
	if !got.OmniRouteExportedAt.IsZero() {
		t.Fatal("an account we did not send was marked as handed off")
	}
	if p, ok := fake.BoundProxy("conn-own"); !ok || p.Port != 7001 {
		t.Fatalf("the existing connection did not get the proxy: %+v, %v", p, ok)
	}
	// We still hold the only copy of OUR tokens, so they remain usable here.
	if _, err := e.svc.Switch(context.Background(), a.ID); err != nil {
		t.Fatalf("switch = %v", err)
	}
}

func TestExportOverwriteReplacesAnUnsentAccount(t *testing.T) {
	e, fake, c := exportEnv(t)
	a := e.add(t, "a@example.com", "acct-a", "")
	fake.AddConnection("codex", "conn-own", "a@example.com")

	res, err := e.svc.ExportToOmniRoute(context.Background(), c, e.all(t), ExportOptions{Overwrite: true})
	if err != nil || res[0].Outcome != ExportExported {
		t.Fatalf("results = %+v, %v", res, err)
	}
	imps := fake.CodexImports()
	if len(imps) != 1 || !imps[0].Overwrite {
		t.Fatalf("imports = %+v", imps)
	}
	if got, _ := e.repo.GetByID(context.Background(), a.ID); got.OmniRouteExportedAt.IsZero() {
		t.Fatal("not marked after an overwrite")
	}
}

func TestExportSkipsBrokenAccountsAndNamesDuplicatesApart(t *testing.T) {
	e, fake, c := exportEnv(t)
	bad := e.add(t, "bad@example.com", "acct-bad", "")
	_ = e.repo.UpdateStatus(context.Background(), bad.ID, domain.AccountStatusError)
	e.add(t, "team@example.com", "ws-one-1234", "")
	e.add(t, "team@example.com", "ws-two-5678", "")

	res, err := e.svc.ExportToOmniRoute(context.Background(), c, e.all(t), ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]ExportOutcome{}
	for _, r := range res {
		by[r.Account.ChatGPTAccountID] = r.Outcome
	}
	if by["acct-bad"] != ExportSkipped || by["ws-one-1234"] != ExportExported || by["ws-two-5678"] != ExportExported {
		t.Fatalf("outcomes = %v", by)
	}
	names := map[string]bool{}
	for _, i := range fake.CodexImports() {
		names[i.Name] = true
	}
	if len(names) != 2 {
		t.Fatalf("names = %v, want two distinct ones for the shared email", names)
	}
}

func TestExportDoesNothingWhenOmniRouteCannotBeListed(t *testing.T) {
	e := newSvcEnv(t)
	e.add(t, "a@example.com", "acct-a", "")
	dead := omniroute.NewClient(omniroute.WithBaseURL("http://127.0.0.1:1"))
	if _, err := e.svc.ExportToOmniRoute(context.Background(), dead, e.all(t), ExportOptions{}); err == nil || !strings.Contains(err.Error(), "nothing imported") {
		t.Fatalf("err = %v", err)
	}
	if got := e.all(t)[0]; !got.OmniRouteExportedAt.IsZero() {
		t.Fatal("an account was marked although nothing was sent")
	}
}

func TestExportedAccountsRefuseToBeUsedHere(t *testing.T) {
	e, _, c := exportEnv(t)
	e.issuer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/wham/usage" {
			_, _ = w.Write([]byte(usageJSON))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"at-new","refresh_token":"rt-new"}`))
	})
	a := e.add(t, "a@example.com", "acct-a", "http://u:p@127.0.0.1:7001")
	if _, err := e.svc.ExportToOmniRoute(context.Background(), c, e.all(t), ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := e.svc.Refresh(ctx, a.ID, RefreshOptions{}); !errors.Is(err, ErrHandedOff) {
		t.Fatalf("Refresh = %v, want ErrHandedOff", err)
	}
	if _, err := e.svc.Usage(ctx, a.ID, RefreshOptions{}); !errors.Is(err, ErrHandedOff) {
		t.Fatalf("Usage = %v, want ErrHandedOff", err)
	}
	if _, err := e.svc.Switch(ctx, a.ID); !errors.Is(err, ErrHandedOff) {
		t.Fatalf("Switch = %v, want ErrHandedOff", err)
	}
	if _, err := e.svc.SwitchForce(ctx, a.ID, false); !errors.Is(err, ErrHandedOff) {
		t.Fatalf("SwitchForce(false) = %v", err)
	}
	// Nothing was written for a refused switch.
	if f, _ := ReadAuthFile(AuthPath(e.home)); f != nil {
		t.Fatal("a refused switch wrote auth.json")
	}

	// An explicit force is the user's call.
	if _, err := e.svc.Refresh(ctx, a.ID, RefreshOptions{Force: true}); err != nil {
		t.Fatalf("forced Refresh = %v", err)
	}
	if _, err := e.svc.SwitchForce(ctx, a.ID, true); err != nil {
		t.Fatalf("forced Switch = %v", err)
	}
}

func TestANewSignInTakesTheAccountBack(t *testing.T) {
	e, _, c := exportEnv(t)
	a := e.add(t, "a@example.com", "acct-a", "http://u:p@127.0.0.1:7001")
	if _, err := e.svc.ExportToOmniRoute(context.Background(), c, e.all(t), ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	// A fresh login is a new token family that OmniRoute never saw.
	if _, err := e.svc.AddFromTokens(context.Background(), &TokenResponse{IDToken: idTok(t, "a@example.com", "acct-a"), AccessToken: "at2", RefreshToken: "rt2"}, "", ""); err != nil {
		t.Fatal(err)
	}
	got, _ := e.repo.GetByID(context.Background(), a.ID)
	if !got.OmniRouteExportedAt.IsZero() {
		t.Fatal("a fresh sign-in kept the hand-off marker")
	}
	if _, err := e.svc.Switch(context.Background(), a.ID); err != nil {
		t.Fatalf("switch after a new sign-in = %v", err)
	}
}
