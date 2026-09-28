package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/omniroute"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/omniroute/omnitest"
)

func TestSkipExport(t *testing.T) {
	existing := existingOmniRouteAccounts{
		agy:         map[string][]omniroute.Connection{"in-agy@gmail.com": {{ID: "c1"}}},
		antigravity: map[string][]omniroute.Connection{"in-antigravity@gmail.com": {{ID: "c2"}}},
	}
	cases := []struct {
		email                     string
		overwrite, allowDuplicate bool
		skip                      bool
		reasonHas                 string
	}{
		{"new@gmail.com", false, false, false, ""},
		{"In-Agy@Gmail.com", false, false, true, "--overwrite"}, // case-insensitive
		{"in-agy@gmail.com", true, false, false, ""},            // --overwrite refreshes it there
		{"in-antigravity@gmail.com", false, false, true, "--allow-duplicate"},
		{"in-antigravity@gmail.com", true, false, true, "--allow-duplicate"}, // --overwrite does not help: another provider
		{"in-antigravity@gmail.com", false, true, false, ""},
	}
	for _, c := range cases {
		skip, reason := skipExport(c.email, existing, c.overwrite, c.allowDuplicate)
		if skip != c.skip || !strings.Contains(reason, c.reasonHas) {
			t.Errorf("skipExport(%q, overwrite=%v, allowDuplicate=%v) = (%v, %q), want skip=%v with %q",
				c.email, c.overwrite, c.allowDuplicate, skip, reason, c.skip, c.reasonHas)
		}
	}
}

func TestIsAlreadyExistsError(t *testing.T) {
	if !isAlreadyExistsError("An Antigravity CLI connection for this account already exists. Pass overwriteExisting: true to replace it.") {
		t.Error("OmniRoute's duplicate refusal was not recognised")
	}
	if isAlreadyExistsError("Failed to validate the agy token") {
		t.Error("an unrelated error was classified as already-exists")
	}
}

func TestFetchExistingOmniRouteAccounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/providers" {
			http.NotFound(w, r)
			return
		}
		var conns []map[string]any
		switch r.URL.Query().Get("provider") {
		case "agy":
			conns = []map[string]any{{"id": "c1", "email": "Daniel@Gmail.com"}}
		case "antigravity":
			conns = []map[string]any{{"id": "c2", "name": "jef@gmail.com"}} // email missing, name holds it
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"connections": conns, "total": len(conns)})
	}))
	defer srv.Close()

	got, err := fetchExistingOmniRouteAccounts(context.Background(), omniroute.NewClient(omniroute.WithBaseURL(srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.agy["daniel@gmail.com"]) != 1 || len(got.antigravity["jef@gmail.com"]) != 1 || len(got.agy["jef@gmail.com"]) != 0 {
		t.Errorf("existing accounts = %+v", got)
	}
}

// Without the list there is no safe way to import, so a listing failure must surface as an error.
func TestFetchExistingOmniRouteAccounts_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"AUTH_001","message":"Invalid management token"}}`))
	}))
	defer srv.Close()

	if _, err := fetchExistingOmniRouteAccounts(context.Background(), omniroute.NewClient(omniroute.WithBaseURL(srv.URL))); err == nil {
		t.Fatal("expected an error when OmniRoute refuses the listing")
	}
}

func TestConnectionsFor_PrefersAgy(t *testing.T) {
	e := existingOmniRouteAccounts{
		agy:         map[string][]omniroute.Connection{"a@gmail.com": {{ID: "agy-1"}}},
		antigravity: map[string][]omniroute.Connection{"a@gmail.com": {{ID: "ag-1"}}, "b@gmail.com": {{ID: "ag-2"}}},
	}
	if cs := e.connectionsFor(" A@Gmail.com "); len(cs) != 1 || cs[0].ID != "agy-1" {
		t.Errorf("connectionsFor(a) = %+v, want the agy connection", cs)
	}
	if cs := e.connectionsFor("b@gmail.com"); len(cs) != 1 || cs[0].ID != "ag-2" {
		t.Errorf("connectionsFor(b) = %+v, want the antigravity one", cs)
	}
	if cs := e.connectionsFor("c@gmail.com"); len(cs) != 0 {
		t.Errorf("connectionsFor(c) = %+v, want none", cs)
	}
}

func TestBindProxies(t *testing.T) {
	fake := omnitest.New(t)
	fake.AddConnection("agy", "conn-new", "new@gmail.com")
	fake.AddConnection("agy", "conn-same", "same@gmail.com")
	fake.AddConnection("agy", "conn-old", "old@gmail.com")
	same := fake.AddProxy("ws-1.1.1.1", "1.1.1.1", 8080, "u1", "pw1")
	fake.Bind("conn-same", same)
	fake.Bind("conn-old", fake.AddProxy("ws-9.9.9.9", "9.9.9.9", 3128, "u9", "pw9"))

	client := omniroute.NewClient(omniroute.WithBaseURL(fake.URL), omniroute.WithToken("manage-key"))
	var out, errOut bytes.Buffer
	bound, unchanged, skipped := bindProxies(context.Background(), client, []bindTarget{
		{email: "new@gmail.com", connectionID: "conn-new", proxyURL: "http://u2:pw2@2.2.2.2:8080"},
		{email: "same@gmail.com", connectionID: "conn-same", proxyURL: "http://u1:pw1@1.1.1.1:8080"},
		{email: "old@gmail.com", connectionID: "conn-old", proxyURL: "http://u3:pw3@3.3.3.3:8080"},
		{email: "bad@gmail.com", connectionID: "conn-bad", proxyURL: "1.2.3.4:8080:u:s3cretPW"}, // unusable local value
	}, &out, &errOut)

	if bound != 2 || unchanged != 1 || skipped != 1 {
		t.Errorf("bound=%d unchanged=%d skipped=%d, want 2 1 1\nout: %s\nerr: %s", bound, unchanged, skipped, &out, &errOut)
	}
	if b, _ := fake.BoundProxy("conn-new"); b.Host != "2.2.2.2" {
		t.Errorf("new connection bound to %+v", b)
	}
	if b, _ := fake.BoundProxy("conn-old"); b.Host != "3.3.3.3" {
		t.Errorf("old connection not rebound: %+v", b)
	}
	if !strings.Contains(out.String(), "9.9.9.9:3128") {
		t.Errorf("output should say what the connection used before: %s", &out)
	}
	for _, s := range []string{out.String(), errOut.String()} {
		if strings.Contains(s, "pw1") || strings.Contains(s, "pw2") || strings.Contains(s, "s3cretPW") || strings.Contains(s, "pw9") {
			t.Errorf("output leaks a password: %s", s)
		}
	}
	// The unusable value must be rejected before reaching the network.
	for _, r := range fake.Requests() {
		if strings.Contains(r, "conn-bad") {
			t.Errorf("an invalid proxy reached OmniRoute: %s", r)
		}
	}
}
