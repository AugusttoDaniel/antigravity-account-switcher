package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
)

func TestParseTimes(t *testing.T) {
	got, err := ParseTimes(" 12:30, 7:00;07:00  18:05 ")
	if err != nil || strings.Join(got, ",") != "07:00,12:30,18:05" {
		t.Fatalf("got %v, %v", got, err)
	}
	for name, in := range map[string]string{
		"empty":    "  ",
		"not time": "noon",
		"hour 24":  "24:00",
		"minute":   "07:75",
		"too many": "01:00,02:00,03:00,04:00,05:00,06:00,07:00",
	} {
		if _, err := ParseTimes(in); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestPickModel(t *testing.T) {
	models := []ModelInfo{
		{Slug: "hidden-first", Visibility: "hide", Priority: 0},
		{Slug: "second", Visibility: "list", Priority: 5},
		{Slug: "first", Visibility: "list", Priority: 2},
		{Slug: "", Visibility: "list", Priority: 1},
	}
	if got, err := PickModel(models); err != nil || got != "first" {
		t.Fatalf("picked %q, %v", got, err)
	}
	if got, _ := PickModel([]ModelInfo{{Slug: "no-visibility-field", Priority: 3}}); got != "no-visibility-field" {
		t.Fatalf("a model with no visibility must still be usable, got %q", got)
	}
	if _, err := PickModel([]ModelInfo{{Slug: "x", Visibility: "hide"}}); !errors.Is(err, ErrNoModel) {
		t.Fatalf("err = %v", err)
	}
}

const modelsJSON = `{"models":[{"slug":"model-b","visibility":"list","priority":7},{"slug":"model-a","visibility":"list","priority":1},{"slug":"hidden","visibility":"hide","priority":0}]}`

const sseOK = "event: response.created\ndata: {\"type\":\"response.created\"}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n"

// warmBackend serves the models and responses endpoints and records what the warm-up sent.
type warmBackend struct {
	mu          sync.Mutex
	body        map[string]any
	auth, acct  string
	accept      string
	modelsAuth  string
	responses   int32
	respond     func(w http.ResponseWriter, r *http.Request)
	tokenCalls  int32
	versionSeen string
}

func (b *warmBackend) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/codex/models":
			b.mu.Lock()
			b.modelsAuth, b.versionSeen = r.Header.Get("Authorization"), r.URL.Query().Get("client_version")
			b.mu.Unlock()
			_, _ = w.Write([]byte(modelsJSON))
		case "/codex/responses":
			atomic.AddInt32(&b.responses, 1)
			raw, _ := io.ReadAll(r.Body)
			b.mu.Lock()
			b.body = map[string]any{}
			_ = json.Unmarshal(raw, &b.body)
			b.auth, b.acct, b.accept = r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-Id"), r.Header.Get("Accept")
			b.mu.Unlock()
			if b.respond != nil {
				b.respond(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(sseOK))
		case "/oauth/token":
			atomic.AddInt32(&b.tokenCalls, 1)
			_, _ = w.Write([]byte(`{"access_token":"at-fresh","refresh_token":"rt-rotated"}`))
		default:
			http.NotFound(w, r)
		}
	}
}

func TestWarmUpSendsAMinimalRequestToTheDiscoveredModel(t *testing.T) {
	e := newSvcEnv(t)
	b := &warmBackend{}
	e.issuer(b.handler())
	a := e.add(t, "a@example.com", "acct-a", "http://u:p@127.0.0.1:7001")

	res, err := e.svc.WarmUp(context.Background(), a.ID, WarmOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != "model-a" {
		t.Fatalf("model = %q, want the visible one with the lowest priority", res.Model)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.versionSeen != ClientVersion || b.modelsAuth != "Bearer at-a@example.com" {
		t.Fatalf("models request: version=%q auth=%q", b.versionSeen, b.modelsAuth)
	}
	if b.auth != "Bearer at-a@example.com" || b.acct != "acct-a" || b.accept != "text/event-stream" {
		t.Fatalf("headers: auth=%q acct=%q accept=%q", b.auth, b.acct, b.accept)
	}
	if b.body["model"] != "model-a" || b.body["stream"] != true || b.body["store"] != false {
		t.Fatalf("body = %v", b.body)
	}
	if ins, _ := b.body["instructions"].(string); ins == "" {
		t.Fatal("the backend needs non-empty instructions")
	}
	if _, has := b.body["tools"]; has {
		t.Fatal("a warm-up must not offer tools")
	}
	if r, _ := b.body["reasoning"].(map[string]any); r["effort"] != "low" {
		t.Fatalf("reasoning = %v, want low effort", b.body["reasoning"])
	}
	input, _ := b.body["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input = %v", b.body["input"])
	}

	rec, _ := e.repo.GetWarmup(context.Background(), a.ID)
	if rec == nil || rec.LastStatus != "ok" || rec.LastModel != "model-a" || rec.LastRunAt.IsZero() {
		t.Fatalf("record = %+v", rec)
	}
}

func TestWarmUpHonorsAnExplicitModelAndSkipsDiscovery(t *testing.T) {
	e := newSvcEnv(t)
	b := &warmBackend{}
	e.issuer(b.handler())
	a := e.add(t, "a@example.com", "acct-a", "http://u:p@127.0.0.1:7001")
	res, err := e.svc.WarmUp(context.Background(), a.ID, WarmOptions{Model: "my-model"})
	if err != nil || res.Model != "my-model" {
		t.Fatalf("res = %+v, %v", res, err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.versionSeen != "" {
		t.Fatal("the model list was requested although a model was given")
	}
	if b.body["model"] != "my-model" {
		t.Fatalf("body = %v", b.body)
	}
}

func TestWarmUpStreamOutcomes(t *testing.T) {
	cases := map[string]struct {
		status  int
		body    string
		wantErr string
	}{
		"failed event":   {200, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"rate_limit_exceeded\"}}}\n\n", "rate_limit_exceeded"},
		"error event":    {200, "data: {\"type\":\"error\",\"code\":\"server_error\"}\n\n", "server_error"},
		"free text code": {200, "data: {\"type\":\"error\",\"code\":\"secret TOKEN-xyz leaked\"}\n\n", "reported a failure"},
		"no completion":  {200, "data: {\"type\":\"response.created\"}\n\n", "without completing"},
		"rate limited":   {429, "SECRET-BODY", "rate limited"},
		"server error":   {500, "SECRET-BODY", "HTTP 500"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newSvcEnv(t)
			b := &warmBackend{respond: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}}
			e.issuer(b.handler())
			a := e.add(t, "a@example.com", "acct-a", "http://u:p@127.0.0.1:7001")
			_, err := e.svc.WarmUp(context.Background(), a.ID, WarmOptions{})
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want %q", err, c.wantErr)
			}
			for _, leak := range []string{"SECRET-BODY", "TOKEN-xyz", "at-a@example.com"} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("the error leaks %q: %v", leak, err)
				}
			}
			rec, _ := e.repo.GetWarmup(context.Background(), a.ID)
			if rec == nil || rec.LastStatus != "failed" || strings.Contains(rec.LastDetail, "SECRET-BODY") {
				t.Fatalf("record = %+v", rec)
			}
		})
	}
}

func TestWarmUpRenewsAnExpiredTokenOnceAndRetries(t *testing.T) {
	e := newSvcEnv(t)
	b := &warmBackend{}
	b.respond = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(sseOK))
	}
	e.issuer(b.handler())
	a := e.add(t, "a@example.com", "acct-a", "http://u:p@127.0.0.1:7001")
	if _, err := e.svc.WarmUp(context.Background(), a.ID, WarmOptions{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&b.tokenCalls) != 1 || atomic.LoadInt32(&b.responses) != 2 {
		t.Fatalf("tokenCalls=%d responses=%d, want 1 and 2", b.tokenCalls, b.responses)
	}
	got, _ := e.repo.GetByID(context.Background(), a.ID)
	if got.AccessToken != "at-fresh" || got.RefreshToken != "rt-rotated" {
		t.Fatalf("renewed tokens were not stored: %+v", got)
	}
}

func TestWarmUpRefusesWhatShouldNotReachOpenAI(t *testing.T) {
	e := newSvcEnv(t)
	b := &warmBackend{}
	e.issuer(b.handler())
	ctx := context.Background()

	noProxy := e.add(t, "noproxy@example.com", "acct-np", "")
	if _, err := e.svc.WarmUp(ctx, noProxy.ID, WarmOptions{}); !errors.Is(err, ErrProxyRequired) {
		t.Fatalf("no proxy: %v", err)
	}
	broken := e.add(t, "broken@example.com", "acct-br", "http://u:p@127.0.0.1:7001")
	_ = e.repo.UpdateStatus(ctx, broken.ID, domain.AccountStatusError)
	if _, err := e.svc.WarmUp(ctx, broken.ID, WarmOptions{}); !errors.Is(err, ErrNeedsSignIn) {
		t.Fatalf("errored account: %v", err)
	}
	handed := e.add(t, "handed@example.com", "acct-ho", "http://u:p@127.0.0.1:7001")
	_ = e.repo.SetOmniRouteExported(ctx, handed.ID, time.Now())
	if _, err := e.svc.WarmUp(ctx, handed.ID, WarmOptions{}); !errors.Is(err, ErrHandedOff) {
		t.Fatalf("handed-off account: %v", err)
	}
	if atomic.LoadInt32(&b.responses) != 0 {
		t.Fatal("a refused account still reached the backend")
	}
	// Refusals are recorded as skipped, not as failed attempts.
	for _, a := range []*domain.CodexAccount{noProxy, broken, handed} {
		if rec, _ := e.repo.GetWarmup(ctx, a.ID); rec == nil || rec.LastStatus != "skipped" {
			t.Fatalf("%s record = %+v", a.Email, rec)
		}
	}
}

func TestSetWarmupScheduleDoesNotFireSlotsAlreadyPast(t *testing.T) {
	e := newSvcEnv(t)
	a := e.add(t, "a@example.com", "acct-a", "http://u:p@127.0.0.1:7001")
	e.svc.Now = func() time.Time { return time.Date(2026, 10, 1, 15, 0, 0, 0, time.Local) }

	w, err := e.svc.SetWarmupSchedule(context.Background(), a.ID, []string{"07:00", "12:00", "18:00"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !w.Enabled || w.LastFiredSlot != "2026-10-01 12:00" {
		t.Fatalf("schedule = %+v, want the 12:00 slot already marked handled", w)
	}
	if _, err := e.svc.SetWarmupSchedule(context.Background(), a.ID, nil, true); err == nil {
		t.Fatal("enabling with no times must be refused")
	}
	off, err := e.svc.SetWarmupSchedule(context.Background(), a.ID, w.Times, false)
	if err != nil || off.Enabled {
		t.Fatalf("off = %+v, %v", off, err)
	}
	if _, err := e.svc.SetWarmupSchedule(context.Background(), "missing", []string{"07:00"}, true); !errors.Is(err, domain.ErrCodexAccountNotFound) {
		t.Fatalf("missing account: %v", err)
	}
}

// ---- scheduler ----

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func schedEnv(t *testing.T, start time.Time) (*svcEnv, *Scheduler, *clock, *warmBackend, *[]string) {
	t.Helper()
	e := newSvcEnv(t)
	b := &warmBackend{}
	e.issuer(b.handler())
	clk := &clock{t: start}
	e.svc.Now = clk.now
	var events []string
	var mu sync.Mutex
	s := &Scheduler{Svc: e.svc, Now: clk.now, MaxJitter: time.Minute, Gap: time.Millisecond, Grace: 10 * time.Minute,
		OnEvent: func(kind, id, msg string) { mu.Lock(); events = append(events, kind+": "+msg); mu.Unlock() }}
	return e, s, clk, b, &events
}

func at(h, m, sec int) time.Time { return time.Date(2026, 10, 1, h, m, sec, 0, time.Local) }

func TestSchedulerFiresASlotOnceAfterItsJitter(t *testing.T) {
	e, s, clk, b, events := schedEnv(t, at(6, 59, 0))
	a := e.add(t, "a@example.com", "acct-a", "http://u:p@127.0.0.1:7001")
	if _, err := e.svc.SetWarmupSchedule(context.Background(), a.ID, []string{"07:00"}, true); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if recs := s.RunOnce(ctx); len(recs) != 0 {
		t.Fatalf("fired before the slot: %+v", recs)
	}
	clk.t = at(7, 0, 0) // the slot has begun, but its per-account offset may not have elapsed
	jit := jitterFor(a.ID, "2026-10-01 07:00", time.Minute)
	if jit > 0 {
		if recs := s.RunOnce(ctx); len(recs) != 0 {
			t.Fatalf("fired before its jitter (%v) elapsed: %+v", jit, recs)
		}
	}
	clk.t = at(7, 0, 0).Add(jit)
	recs := s.RunOnce(ctx)
	if len(recs) != 1 || recs[0].Status != "ok" || recs[0].Slot != "2026-10-01 07:00" {
		t.Fatalf("records = %+v", recs)
	}
	if atomic.LoadInt32(&b.responses) != 1 {
		t.Fatalf("%d requests, want 1", b.responses)
	}

	// The same slot never runs twice, however often the scheduler looks.
	clk.t = at(7, 5, 0)
	for i := 0; i < 3; i++ {
		if recs := s.RunOnce(ctx); len(recs) != 0 {
			t.Fatalf("the slot fired again: %+v", recs)
		}
	}
	if atomic.LoadInt32(&b.responses) != 1 {
		t.Fatalf("%d requests after repeats, want still 1", b.responses)
	}
	if len(*events) != 1 || !strings.HasPrefix((*events)[0], "ok:") {
		t.Fatalf("events = %v", *events)
	}

	// Next day, the slot comes due again.
	clk.t = at(7, 30, 0).AddDate(0, 0, 1)
	if recs := s.RunOnce(ctx); len(recs) != 1 {
		t.Fatalf("the next day did not fire: %+v", recs)
	}
}

func TestSchedulerRecordsASlotMissedBeyondGraceInsteadOfFiringLate(t *testing.T) {
	e, s, clk, b, _ := schedEnv(t, at(6, 0, 0))
	a := e.add(t, "a@example.com", "acct-a", "http://u:p@127.0.0.1:7001")
	if _, err := e.svc.SetWarmupSchedule(context.Background(), a.ID, []string{"07:00"}, true); err != nil {
		t.Fatal(err)
	}
	clk.t = at(9, 0, 0) // the switcher was not running at 07:00
	recs := s.RunOnce(context.Background())
	if len(recs) != 1 || recs[0].Status != "missed" {
		t.Fatalf("records = %+v", recs)
	}
	if atomic.LoadInt32(&b.responses) != 0 {
		t.Fatal("a missed slot sent a request")
	}
	if recs := s.RunOnce(context.Background()); len(recs) != 0 {
		t.Fatalf("the missed slot was reported again: %+v", recs)
	}
}

func TestSchedulerIgnoresDisabledSchedulesAndNeverRetriesAFailure(t *testing.T) {
	e, s, clk, b, _ := schedEnv(t, at(6, 0, 0))
	b.respond = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }
	on := e.add(t, "on@example.com", "acct-on", "http://u:p@127.0.0.1:7001")
	off := e.add(t, "off@example.com", "acct-off", "http://u:p@127.0.0.1:7002")
	ctx := context.Background()
	if _, err := e.svc.SetWarmupSchedule(ctx, on.ID, []string{"07:00"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetWarmupSchedule(ctx, off.ID, []string{"07:00"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetWarmupSchedule(ctx, off.ID, []string{"07:00"}, false); err != nil {
		t.Fatal(err)
	}

	clk.t = at(7, 2, 0)
	recs := s.RunOnce(ctx)
	if len(recs) != 1 || recs[0].AccountID != on.ID || recs[0].Status != "failed" {
		t.Fatalf("records = %+v", recs)
	}
	clk.t = at(7, 8, 0)
	if recs := s.RunOnce(ctx); len(recs) != 0 {
		t.Fatalf("a failed slot was retried: %+v", recs)
	}
	if n := atomic.LoadInt32(&b.responses); n != 1 {
		t.Fatalf("%d requests, want exactly 1 (no retry, disabled account untouched)", n)
	}
}

func TestSchedulerSkipsWhatShouldNotBeWarmed(t *testing.T) {
	e, s, clk, b, _ := schedEnv(t, at(6, 0, 0))
	ctx := context.Background()
	noProxy := e.add(t, "noproxy@example.com", "acct-np", "")
	handed := e.add(t, "handed@example.com", "acct-ho", "http://u:p@127.0.0.1:7001")
	_ = e.repo.SetOmniRouteExported(ctx, handed.ID, time.Now())
	for _, a := range []*domain.CodexAccount{noProxy, handed} {
		if _, err := e.svc.SetWarmupSchedule(ctx, a.ID, []string{"07:00"}, true); err != nil {
			t.Fatal(err)
		}
	}
	clk.t = at(7, 3, 0)
	recs := s.RunOnce(ctx)
	if len(recs) != 2 {
		t.Fatalf("records = %+v", recs)
	}
	for _, r := range recs {
		if r.Status != "skipped" {
			t.Errorf("%s = %s, want skipped", r.Email, r.Status)
		}
	}
	if atomic.LoadInt32(&b.responses) != 0 {
		t.Fatal("a skipped account reached the backend")
	}
}

func TestSchedulerDoesNothingWithoutSchedules(t *testing.T) {
	e, s, clk, b, _ := schedEnv(t, at(7, 0, 0))
	e.add(t, "a@example.com", "acct-a", "http://u:p@127.0.0.1:7001")
	clk.t = at(12, 0, 0)
	if recs := s.RunOnce(context.Background()); len(recs) != 0 {
		t.Fatalf("records = %+v", recs)
	}
	if atomic.LoadInt32(&b.responses) != 0 {
		t.Fatal("a request was made with no schedule enabled")
	}
	if (&Scheduler{}).RunOnce(context.Background()) != nil {
		t.Fatal("a scheduler with no service must do nothing")
	}
}

func TestJitterIsStableAndBounded(t *testing.T) {
	a := jitterFor("acc", "2026-10-01 07:00", 90*time.Second)
	if a != jitterFor("acc", "2026-10-01 07:00", 90*time.Second) {
		t.Fatal("jitter is not stable")
	}
	for i := 0; i < 50; i++ {
		if j := jitterFor("acc"+string(rune('a'+i%26)), "slot"+string(rune('a'+i%7)), 90*time.Second); j < 0 || j >= 90*time.Second {
			t.Fatalf("jitter %v out of range", j)
		}
	}
	if jitterFor("x", "y", 0) != 0 {
		t.Fatal("zero max must give no jitter")
	}
}
