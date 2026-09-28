package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/omniroute"
)

func TestSkipExport(t *testing.T) {
	existing := existingOmniRouteAccounts{
		agy:         map[string]bool{"in-agy@gmail.com": true},
		antigravity: map[string]bool{"in-antigravity@gmail.com": true},
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
	if !got.agy["daniel@gmail.com"] || !got.antigravity["jef@gmail.com"] || got.agy["jef@gmail.com"] {
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
