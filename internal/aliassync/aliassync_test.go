package aliassync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/adspower"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/omniroute"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/omniroute/omnitest"
)

// fakeProfiles is an in-memory profile API.
type fakeProfiles struct {
	profiles []adspower.Profile
	created  []adspower.CreateProfileRequest
	failWith error
}

func (f *fakeProfiles) ListProfiles(_ context.Context, page, size int) ([]adspower.Profile, error) {
	start := (page - 1) * size
	if start >= len(f.profiles) {
		return nil, nil
	}
	end := start + size
	if end > len(f.profiles) {
		end = len(f.profiles)
	}
	return f.profiles[start:end], nil
}

func (f *fakeProfiles) CreateProfile(_ context.Context, req adspower.CreateProfileRequest) (string, error) {
	if f.failWith != nil {
		return "", f.failWith
	}
	f.created = append(f.created, req)
	id := fmt.Sprintf("prof-%d", len(f.created))
	f.profiles = append(f.profiles, adspower.Profile{UserID: id, Name: req.Name, ProxyConfig: req.ProxyConfig})
	return id, nil
}

func registry(t *testing.T) (*omnitest.Server, *omniroute.Client) {
	t.Helper()
	fake := omnitest.New(t)
	return fake, omniroute.NewClient(omniroute.WithBaseURL(fake.URL))
}

func TestCollectFindsCredentialsWhereTheyCanBeFound(t *testing.T) {
	fake, c := registry(t)
	assigned := fake.AddProxy("ws-1", "10.0.0.1", 6001, "userA", "passA")
	fake.AddProxy("ws-2", "10.0.0.2", 6002, "redacted-in-list", "redacted-in-list") // not assigned
	fake.AddProxy("ws-3", "10.0.0.3", 6003, "x", "y")                               // not assigned, not in the pool
	fake.AddConnection("agy", "conn-1", "a@example.com")
	fake.Bind("conn-1", assigned)

	pool := []string{"http://poolUser:poolPass@10.0.0.2:6002", "http://other:pw@99.9.9.9:1"}
	cands, missing, err := Collect(context.Background(), c, pool)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Candidate{}
	for _, cd := range cands {
		by[cd.HostPort] = cd
	}
	if got := by["10.0.0.1:6001"]; got.Source != "omniroute" || got.URL != "http://userA:passA@10.0.0.1:6001" {
		t.Fatalf("assigned proxy = %+v", got)
	}
	if got := by["10.0.0.2:6002"]; got.Source != "pool" || got.URL != "http://poolUser:poolPass@10.0.0.2:6002" {
		t.Fatalf("pooled proxy = %+v", got)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want 2 (a pool proxy that is not in OmniRoute must be ignored)", len(cands))
	}
	if len(missing) != 1 || missing[0].HostPort != "10.0.0.3:6003" || missing[0].Name != "ws-3" {
		t.Fatalf("missing = %+v", missing)
	}
	if strings.Contains(missing[0].Reason, "x") && strings.Contains(missing[0].Reason, "y@") {
		t.Fatal("the reason leaks credentials")
	}
}

func TestCollectPrefersOmniRouteCredentialsOverThePool(t *testing.T) {
	fake, c := registry(t)
	a := fake.AddProxy("ws-a", "10.0.0.1", 6001, "userA", "passA")
	fake.AddConnection("agy", "conn-a", "a@example.com")
	fake.Bind("conn-a", a)

	// The pool also has this endpoint, with a different login: OmniRoute's real credential wins.
	cands, _, err := Collect(context.Background(), c, []string{"http://poolUser:poolPass@10.0.0.1:6001"})
	if err != nil || len(cands) != 1 || cands[0].Source != "omniroute" || !strings.Contains(cands[0].URL, "userA") {
		t.Fatalf("cands = %+v, %v", cands, err)
	}
}

func TestCollectDeduplicatesAnEndpoint(t *testing.T) {
	fake, c := registry(t)
	fake.AddProxy("first", "10.0.0.1", 6001, "u", "p")
	fake.AddProxy("second", "10.0.0.1", 6001, "u2", "p2")
	cands, _, _ := Collect(context.Background(), c, []string{"http://u:p@10.0.0.1:6001"})
	if len(cands) != 1 {
		t.Fatalf("%d candidates for one endpoint", len(cands))
	}
}

func TestCollectFailsWhenOmniRouteCannotBeListed(t *testing.T) {
	dead := omniroute.NewClient(omniroute.WithBaseURL("http://127.0.0.1:1"))
	if _, _, err := Collect(context.Background(), dead, nil); err == nil {
		t.Fatal("expected an error")
	}
}

func cand(hp, user, pass string) Candidate {
	return Candidate{Name: "ws", HostPort: hp, URL: fmt.Sprintf("http://%s:%s@%s", user, pass, hp), Source: "pool"}
}

func TestApplyCreatesOneBoundProfilePerProxy(t *testing.T) {
	api := &fakeProfiles{}
	res, err := Apply(context.Background(), api, []Candidate{cand("10.0.0.1:6001", "uA", "pA"), cand("10.0.0.2:6002", "uB", "pB")}, "cloak", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(api.created) != 2 {
		t.Fatalf("created %d profiles", len(api.created))
	}
	first := api.created[0]
	if first.Name != "proxy-10.0.0.1-6001" || first.Browser != "cloak" ||
		first.ProxyConfig.ProxyHost != "10.0.0.1" || first.ProxyConfig.ProxyPort != "6001" ||
		first.ProxyConfig.ProxyUser != "uA" || first.ProxyConfig.ProxyPassword != "pA" {
		t.Fatalf("request = %+v", first)
	}
	if res[0].Outcome != Created || res[0].ProfileID != "prof-1" {
		t.Fatalf("result = %+v", res[0])
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	api := &fakeProfiles{}
	cands := []Candidate{cand("10.0.0.1:6001", "u", "p")}
	if _, err := Apply(context.Background(), api, cands, "cloak", false); err != nil {
		t.Fatal(err)
	}
	res, err := Apply(context.Background(), api, cands, "cloak", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(api.created) != 1 || res[0].Outcome != Exists || res[0].ProfileID != "prof-1" {
		t.Fatalf("created=%d result=%+v", len(api.created), res[0])
	}
}

func TestApplyRecognizesAProfileAlreadyUsingTheProxyUnderAnotherName(t *testing.T) {
	api := &fakeProfiles{profiles: []adspower.Profile{{
		UserID: "mine", Name: "my own profile",
		ProxyConfig: adspower.ProxyConfig{ProxyHost: "10.0.0.1", ProxyPort: "6001"},
	}}}
	res, err := Apply(context.Background(), api, []Candidate{cand("10.0.0.1:6001", "u", "p")}, "cloak", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(api.created) != 0 || res[0].Outcome != Exists || res[0].ProfileName != "my own profile" {
		t.Fatalf("created=%d result=%+v", len(api.created), res[0])
	}
}

func TestApplyDryRunCreatesNothing(t *testing.T) {
	api := &fakeProfiles{}
	res, err := Apply(context.Background(), api, []Candidate{cand("10.0.0.1:6001", "u", "p")}, "cloak", true)
	if err != nil || len(api.created) != 0 || res[0].Outcome != WouldCreate {
		t.Fatalf("created=%d result=%+v err=%v", len(api.created), res, err)
	}
}

func TestApplyNeverLeaksCredentialsInErrors(t *testing.T) {
	api := &fakeProfiles{failWith: errors.New("create failed for user alicebob with password sup3rSecret at host")}
	res, err := Apply(context.Background(), api, []Candidate{cand("10.0.0.1:6001", "alicebob", "sup3rSecret")}, "cloak", false)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Outcome != Failed || res[0].Err == nil {
		t.Fatalf("result = %+v", res[0])
	}
	if msg := res[0].Err.Error(); strings.Contains(msg, "sup3rSecret") || strings.Contains(msg, "alicebob") {
		t.Fatalf("the error leaks credentials: %s", msg)
	}
}

func TestApplyPagesThroughManyProfiles(t *testing.T) {
	api := &fakeProfiles{}
	for i := 0; i < 230; i++ {
		api.profiles = append(api.profiles, adspower.Profile{UserID: fmt.Sprint("p", i), Name: fmt.Sprint("other-", i)})
	}
	api.profiles = append(api.profiles, adspower.Profile{UserID: "late", Name: "proxy-10.0.0.1-6001"})
	res, _ := Apply(context.Background(), api, []Candidate{cand("10.0.0.1:6001", "u", "p")}, "cloak", false)
	if res[0].Outcome != Exists || res[0].ProfileID != "late" || len(api.created) != 0 {
		t.Fatalf("a profile past the first page was missed: %+v", res[0])
	}
}

func TestProfileName(t *testing.T) {
	if got := ProfileName("31.59.20.176:6754"); got != "proxy-31.59.20.176-6754" {
		t.Fatalf("name = %q", got)
	}
	if got := ProfileName("[::1]:8080"); !strings.HasPrefix(got, "proxy-") || strings.ContainsAny(got, "[]") {
		t.Fatalf("name = %q", got)
	}
}
