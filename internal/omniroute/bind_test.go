package omniroute

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/omniroute/omnitest"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func newBindClient(fake *omnitest.Server) *Client {
	return NewClient(WithBaseURL(fake.URL), WithToken("manage-key"))
}

func TestBindConnectionProxy_RegistersAndBinds(t *testing.T) {
	fake := omnitest.New(t)
	fake.AddConnection("agy", "conn-1", "a@gmail.com")

	res, err := newBindClient(fake).BindConnectionProxy(context.Background(), "conn-1", mustURL(t, "http://alice:s3cret@1.2.3.4:8080"))
	if err != nil {
		t.Fatalf("BindConnectionProxy: %v", err)
	}
	if !res.Changed || res.Was != "direct" || res.ProxyID == "" {
		t.Errorf("result = %+v", res)
	}
	bound, ok := fake.BoundProxy("conn-1")
	if !ok || bound.Host != "1.2.3.4" || bound.Port != 8080 || bound.Username != "alice" || bound.Password != "s3cret" {
		t.Errorf("bound proxy = %+v (%v)", bound, ok)
	}
	if n := len(fake.Proxies()); n != 1 {
		t.Errorf("registry has %d entries, want 1", n)
	}
}

// Binding what is already bound must not write anything (no rename, no duplicate, no re-assign).
func TestBindConnectionProxy_IsIdempotent(t *testing.T) {
	fake := omnitest.New(t)
	fake.AddConnection("agy", "conn-1", "a@gmail.com")
	c := newBindClient(fake)
	proxy := mustURL(t, "http://alice:s3cret@1.2.3.4:8080")

	if _, err := c.BindConnectionProxy(context.Background(), "conn-1", proxy); err != nil {
		t.Fatal(err)
	}
	writes := fake.Writes()

	res, err := c.BindConnectionProxy(context.Background(), "conn-1", proxy)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Errorf("second bind reported a change: %+v", res)
	}
	if fake.Writes() != writes {
		t.Errorf("second bind wrote to OmniRoute (%d -> %d writes)", writes, fake.Writes())
	}
	if !strings.Contains(res.Was, "1.2.3.4:8080") {
		t.Errorf("Was = %q", res.Was)
	}
}

func TestBindConnectionProxy_ReplacesADifferentProxy(t *testing.T) {
	fake := omnitest.New(t)
	fake.AddConnection("agy", "conn-1", "a@gmail.com")
	old := fake.AddProxy("old", "9.9.9.9", 3128, "bob", "pw")
	fake.Bind("conn-1", old)

	res, err := newBindClient(fake).BindConnectionProxy(context.Background(), "conn-1", mustURL(t, "http://alice:s3cret@1.2.3.4:8080"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || !strings.Contains(res.Was, "9.9.9.9:3128") {
		t.Errorf("result = %+v; want the previous proxy reported", res)
	}
	if bound, _ := fake.BoundProxy("conn-1"); bound.Host != "1.2.3.4" {
		t.Errorf("connection still bound to %+v", bound)
	}
}

// A proxy OmniRoute already holds is reused, not duplicated, and keeps the name it was given.
func TestBindConnectionProxy_ReusesEntryAndKeepsItsName(t *testing.T) {
	fake := omnitest.New(t)
	fake.AddConnection("agy", "conn-1", "a@gmail.com")
	fake.AddProxy("my custom name", "1.2.3.4", 8080, "alice", "old-password")

	if _, err := newBindClient(fake).BindConnectionProxy(context.Background(), "conn-1", mustURL(t, "http://alice:s3cret@1.2.3.4:8080")); err != nil {
		t.Fatal(err)
	}
	entries := fake.Proxies()
	if len(entries) != 1 {
		t.Fatalf("registry has %d entries, want the existing one reused", len(entries))
	}
	if entries[0].Name != "my custom name" {
		t.Errorf("entry was renamed to %q", entries[0].Name)
	}
	if entries[0].Password != "s3cret" {
		t.Errorf("password not refreshed from the local proxy: %q", entries[0].Password)
	}
}

// Two credentials on one gateway endpoint: the exact host+port+username entry must be the one bound.
func TestBindConnectionProxy_PicksTheRightCredentialOnASharedEndpoint(t *testing.T) {
	fake := omnitest.New(t)
	fake.AddConnection("agy", "conn-1", "a@gmail.com")
	fake.AddProxy("session-1", "gw.example.com", 80, "sess1", "pw1")
	second := fake.AddProxy("session-2", "gw.example.com", 80, "sess2", "pw2")

	if _, err := newBindClient(fake).BindConnectionProxy(context.Background(), "conn-1", mustURL(t, "http://sess2:pw2@gw.example.com:80")); err != nil {
		t.Fatal(err)
	}
	bound, _ := fake.BoundProxy("conn-1")
	if bound.ID != second || bound.Username != "sess2" {
		t.Errorf("bound %+v, want the sess2 entry %s", bound, second)
	}
	if n := len(fake.Proxies()); n != 2 {
		t.Errorf("registry has %d entries, want the 2 that existed", n)
	}
}

// An accepted assignment is not proof it is in effect: the resolution is checked afterwards.
func TestBindConnectionProxy_FailsWhenOmniRouteDoesNotApplyIt(t *testing.T) {
	fake := omnitest.New(t)
	fake.AddConnection("agy", "conn-1", "a@gmail.com")
	fake.IgnoreAssignments = true

	_, err := newBindClient(fake).BindConnectionProxy(context.Background(), "conn-1", mustURL(t, "http://alice:s3cret@1.2.3.4:8080"))
	if err == nil {
		t.Fatal("expected an error when the connection still resolves to direct")
	}
	if !strings.Contains(err.Error(), "direct") || !strings.Contains(err.Error(), "enabled") {
		t.Errorf("error should say what OmniRoute resolves and hint at the setting: %v", err)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error leaks the password: %v", err)
	}
}

func TestBindConnectionProxy_DefaultPortAndSocks5h(t *testing.T) {
	fake := omnitest.New(t)
	fake.AddConnection("agy", "conn-1", "a@gmail.com")
	fake.AddConnection("agy", "conn-2", "b@gmail.com")
	c := newBindClient(fake)

	if _, err := c.BindConnectionProxy(context.Background(), "conn-1", mustURL(t, "http://alice:pw@proxy.example.com")); err != nil {
		t.Fatalf("no explicit port: %v", err)
	}
	if b, _ := fake.BoundProxy("conn-1"); b.Port != 80 {
		t.Errorf("default http port = %d, want 80", b.Port)
	}
	if _, err := c.BindConnectionProxy(context.Background(), "conn-2", mustURL(t, "socks5h://alice:pw@10.0.0.1:1080")); err != nil {
		t.Fatalf("socks5h: %v", err)
	}
	if b, _ := fake.BoundProxy("conn-2"); b.Type != "socks5" {
		t.Errorf("socks5h registered as type %q, want socks5", b.Type)
	}
}

func TestAssignRegistryProxy_RequiresIDs(t *testing.T) {
	fake := omnitest.New(t)
	c := newBindClient(fake)
	if err := c.AssignRegistryProxy(context.Background(), "", "px-1"); err == nil {
		t.Error("expected an error for an empty connection id")
	}
	if err := c.AssignRegistryProxy(context.Background(), "conn-1", " "); err == nil {
		t.Error("expected an error for an empty proxy id")
	}
	if len(fake.Requests()) != 0 {
		t.Errorf("invalid input reached the network: %v", fake.Requests())
	}
}
