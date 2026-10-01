package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCodexWarmup_RunNowStartsTheWindowAndRecordsIt(t *testing.T) {
	e := newCodexEnv(t, "x@example.com")
	a := e.seed(t, "a@example.com", "acct-a", codexTestProxy)

	code, raw := e.call(t, http.MethodPost, "/api/codex/accounts/warmup", map[string]string{"id": a.ID})
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	var out struct {
		Status string `json:"status"`
		Model  string `json:"model"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out.Status != "warmed" || out.Model != "model-a" {
		t.Fatalf("response = %s (%v)", raw, err)
	}
	for _, secret := range []string{"at-acct-a", "rt-acct-a", "s3cretPW"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("the response leaks %q", secret)
		}
	}

	// The last result shows up in the list, with no credential.
	_, body := e.call(t, http.MethodGet, "/api/codex/accounts", nil)
	var views []struct {
		Warmup *struct {
			LastStatus string `json:"last_status"`
			LastModel  string `json:"last_model"`
		} `json:"warmup"`
	}
	if err := json.Unmarshal([]byte(body), &views); err != nil || len(views) != 1 || views[0].Warmup == nil ||
		views[0].Warmup.LastStatus != "ok" || views[0].Warmup.LastModel != "model-a" {
		t.Fatalf("list = %s (%v)", body, err)
	}
}

func TestCodexWarmup_RefusesWhatShouldNotReachOpenAI(t *testing.T) {
	e := newCodexEnv(t, "x@example.com")
	noProxy := e.seed(t, "np@example.com", "acct-np", "")
	handed := e.seed(t, "ho@example.com", "acct-ho", codexTestProxy)
	if err := e.repo.SetOmniRouteExported(context.Background(), handed.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]string{"no proxy": noProxy.ID, "handed to OmniRoute": handed.ID} {
		if code, raw := e.call(t, http.MethodPost, "/api/codex/accounts/warmup", map[string]string{"id": id}); code != http.StatusConflict {
			t.Errorf("%s: status %d, want 409 (%s)", name, code, raw)
		}
	}
	if code, _ := e.call(t, http.MethodGet, "/api/codex/accounts/warmup", nil); code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", code)
	}
}

func TestCodexWarmup_AFailureIsReportedAndRecorded(t *testing.T) {
	e := newCodexEnv(t, "x@example.com")
	bad := e.seed(t, "bad@example.com", "acct-bad", codexTestProxy) // the fake answers 500 for this one
	code, raw := e.call(t, http.MethodPost, "/api/codex/accounts/warmup", map[string]string{"id": bad.ID})
	if code != http.StatusBadGateway || strings.Contains(raw, "at-acct-bad") {
		t.Fatalf("status %d: %s", code, raw)
	}
	rec, _ := e.repo.GetWarmup(context.Background(), bad.ID)
	if rec == nil || rec.LastStatus != "failed" {
		t.Fatalf("record = %+v", rec)
	}
}

func TestCodexWarmup_ScheduleSetOffAndValidated(t *testing.T) {
	e := newCodexEnv(t, "x@example.com")
	a := e.seed(t, "a@example.com", "acct-a", codexTestProxy)

	code, raw := e.call(t, http.MethodPost, "/api/codex/accounts/warmup/schedule", map[string]any{"id": a.ID, "times": "12:30, 7:00", "enabled": true})
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	w, _ := e.repo.GetWarmup(context.Background(), a.ID)
	if w == nil || !w.Enabled || strings.Join(w.Times, ",") != "07:00,12:30" {
		t.Fatalf("schedule = %+v", w)
	}

	// Off keeps the times, so turning it back on needs no retyping.
	if code, raw := e.call(t, http.MethodPost, "/api/codex/accounts/warmup/schedule", map[string]any{"id": a.ID, "enabled": false}); code != http.StatusOK {
		t.Fatalf("off: %d %s", code, raw)
	}
	w, _ = e.repo.GetWarmup(context.Background(), a.ID)
	if w.Enabled || strings.Join(w.Times, ",") != "07:00,12:30" {
		t.Fatalf("after off = %+v", w)
	}
	// Enabling always names its times (the page sends them): none at all is refused.
	if code, _ := e.call(t, http.MethodPost, "/api/codex/accounts/warmup/schedule", map[string]any{"id": a.ID, "enabled": true}); code != http.StatusBadRequest {
		t.Fatalf("enabling with no times = %d, want 400", code)
	}

	for name, body := range map[string]map[string]any{
		"bad time":        {"id": a.ID, "times": "noon", "enabled": true},
		"too many":        {"id": a.ID, "times": "01:00,02:00,03:00,04:00,05:00,06:00,07:00", "enabled": true},
		"unknown account": {"id": "missing", "times": "07:00", "enabled": true},
	} {
		code, _ := e.call(t, http.MethodPost, "/api/codex/accounts/warmup/schedule", body)
		if code != http.StatusBadRequest && code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 400/404", name, code)
		}
	}

	fresh := e.seed(t, "b@example.com", "acct-b", codexTestProxy)
	if code, _ := e.call(t, http.MethodPost, "/api/codex/accounts/warmup/schedule", map[string]any{"id": fresh.ID, "enabled": true}); code != http.StatusBadRequest {
		t.Fatalf("enabling with no times at all = %d, want 400", code)
	}
}
