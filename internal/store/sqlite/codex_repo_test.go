package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/store/sqlite"
)

func setupCodexRepo(t *testing.T) (*sqlite.CodexAccountRepository, *sqlite.AccountRepository) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "codex.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return sqlite.NewCodexAccountRepository(db), sqlite.NewAccountRepository(db)
}

func codexAcc(email, chatgptID string) *domain.CodexAccount {
	return &domain.CodexAccount{
		Email: email, ChatGPTAccountID: chatgptID, PlanType: "plus",
		IDToken: "id", AccessToken: "at", RefreshToken: "rt",
		LastRefresh: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
	}
}

func TestCodexRepo_UpsertCreatesAndRefreshesSameIdentity(t *testing.T) {
	repo, _ := setupCodexRepo(t)
	ctx := context.Background()

	first := codexAcc("Dev@Example.com", "acc-1")
	first.ProxyURL = "http://u:p@h:1"
	first.AdsPowerProfileID = "prof-1"
	a, err := repo.Upsert(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.Status != domain.AccountStatusActive || a.IsActive || a.ProxyURL != "http://u:p@h:1" {
		t.Fatalf("created = %+v", a)
	}
	if err := repo.UpdateStatus(ctx, a.ID, domain.AccountStatusError); err != nil {
		t.Fatal(err)
	}

	// Same email (different case) and ChatGPT account: refresh in place, keep proxy/profile,
	// reactivate.
	again := codexAcc("dev@example.com", "acc-1")
	again.AccessToken, again.RefreshToken, again.PlanType = "at2", "rt2", "pro"
	b, err := repo.Upsert(ctx, again)
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != a.ID || b.AccessToken != "at2" || b.RefreshToken != "rt2" || b.PlanType != "pro" {
		t.Fatalf("refreshed = %+v", b)
	}
	if b.ProxyURL != "http://u:p@h:1" || b.AdsPowerProfileID != "prof-1" {
		t.Fatalf("proxy/profile were dropped: %+v", b)
	}
	if b.Status != domain.AccountStatusActive {
		t.Fatalf("status = %s, want active after a fresh login", b.Status)
	}
	if list, _ := repo.List(ctx); len(list) != 1 {
		t.Fatalf("%d accounts, want 1", len(list))
	}

	// A new non-empty proxy replaces the old one.
	again.ProxyURL = "http://u:p@h:2"
	c, _ := repo.Upsert(ctx, again)
	if c.ProxyURL != "http://u:p@h:2" {
		t.Fatalf("proxy = %q", c.ProxyURL)
	}
}

func TestCodexRepo_SameEmailDifferentWorkspaceAreDistinct(t *testing.T) {
	repo, _ := setupCodexRepo(t)
	ctx := context.Background()
	a, _ := repo.Upsert(ctx, codexAcc("dev@example.com", "personal"))
	b, err := repo.Upsert(ctx, codexAcc("dev@example.com", "team-ws"))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("a personal and a workspace login collapsed into one account")
	}
}

func TestCodexRepo_UpsertValidates(t *testing.T) {
	repo, _ := setupCodexRepo(t)
	bad := codexAcc("", "x")
	if _, err := repo.Upsert(context.Background(), bad); err == nil {
		t.Fatal("expected an error for an empty email")
	}
	noRefresh := codexAcc("a@b.com", "x")
	noRefresh.RefreshToken = ""
	if _, err := repo.Upsert(context.Background(), noRefresh); err == nil {
		t.Fatal("expected an error for an empty refresh token")
	}
}

func TestCodexRepo_SingleActive(t *testing.T) {
	repo, _ := setupCodexRepo(t)
	ctx := context.Background()
	a, _ := repo.Upsert(ctx, codexAcc("a@example.com", "1"))
	b, _ := repo.Upsert(ctx, codexAcc("b@example.com", "2"))

	if _, err := repo.GetActive(ctx); !errors.Is(err, domain.ErrCodexAccountNotFound) {
		t.Fatalf("GetActive with none = %v", err)
	}
	if err := repo.SetActive(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetActive(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	act, err := repo.GetActive(ctx)
	if err != nil || act.ID != b.ID {
		t.Fatalf("active = %+v, %v", act, err)
	}
	ga, _ := repo.GetByID(ctx, a.ID)
	if ga.IsActive {
		t.Fatal("the previous account stayed active")
	}
	if err := repo.SetActive(ctx, "missing"); !errors.Is(err, domain.ErrCodexAccountNotFound) {
		t.Fatalf("SetActive(missing) = %v", err)
	}
	// A failed activation must not leave the pool without an active account.
	if act, err := repo.GetActive(ctx); err != nil || act.ID != b.ID {
		t.Fatalf("after a failed SetActive: %+v, %v", act, err)
	}
}

func TestCodexRepo_UpdatesAndDelete(t *testing.T) {
	repo, _ := setupCodexRepo(t)
	ctx := context.Background()
	a, _ := repo.Upsert(ctx, codexAcc("a@example.com", "1"))

	when := time.Date(2026, 10, 1, 2, 3, 4, 0, time.UTC)
	if err := repo.UpdateTokens(ctx, a.ID, "id2", "at2", "rt2", when); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateTokens(ctx, a.ID, "x", "y", "", when); err == nil {
		t.Fatal("an empty refresh token must be refused")
	}
	if err := repo.UpdateProxyURL(ctx, a.ID, "http://h:9"); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateAdsPowerProfileID(ctx, a.ID, "p9"); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.GetByID(ctx, a.ID)
	if got.IDToken != "id2" || got.AccessToken != "at2" || got.RefreshToken != "rt2" || !got.LastRefresh.Equal(when) ||
		got.ProxyURL != "http://h:9" || got.AdsPowerProfileID != "p9" {
		t.Fatalf("after updates = %+v", got)
	}

	if err := repo.UpdateProxyURL(ctx, "missing", "x"); !errors.Is(err, domain.ErrCodexAccountNotFound) {
		t.Fatalf("update of a missing account = %v", err)
	}
	if err := repo.Delete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, a.ID); !errors.Is(err, domain.ErrCodexAccountNotFound) {
		t.Fatalf("second delete = %v", err)
	}
}

// Codex logins must never show up where Antigravity accounts are routed or exported.
func TestCodexRepo_IsolatedFromAntigravityAccounts(t *testing.T) {
	repo, accRepo := setupCodexRepo(t)
	ctx := context.Background()
	if _, err := repo.Upsert(ctx, codexAcc("codex@example.com", "1")); err != nil {
		t.Fatal(err)
	}
	accs, err := accRepo.List(ctx)
	if err != nil || len(accs) != 0 {
		t.Fatalf("Antigravity list = %d accounts, %v", len(accs), err)
	}
}

func TestCodexRepo_UsageSnapshots(t *testing.T) {
	repo, _ := setupCodexRepo(t)
	ctx := context.Background()
	a, _ := repo.Upsert(ctx, codexAcc("a@example.com", "1"))

	if _, err := repo.GetUsage(ctx, a.ID); !errors.Is(err, domain.ErrCodexAccountNotFound) {
		t.Fatalf("GetUsage with none = %v", err)
	}
	reset := time.Date(2026, 10, 1, 5, 0, 0, 0, time.UTC)
	u := &domain.CodexUsage{
		AccountID: a.ID, PlanType: "plus", Allowed: true,
		Primary:       &domain.CodexUsageWindow{UsedPercent: 42, WindowSeconds: 18000, ResetAt: reset},
		Secondary:     &domain.CodexUsageWindow{UsedPercent: 7, WindowSeconds: 604800, ResetAt: reset.Add(72 * time.Hour)},
		CreditBalance: "12.5", HasCredits: true,
		FetchedAt: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
	}
	if err := repo.SaveUsage(ctx, u); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetUsage(ctx, a.ID)
	if err != nil || got.Primary.UsedPercent != 42 || got.Secondary.UsedPercent != 7 || !got.Primary.ResetAt.Equal(reset) || got.CreditBalance != "12.5" {
		t.Fatalf("usage = %+v, %v", got, err)
	}

	// A second save replaces the first.
	u.Primary.UsedPercent = 80
	u.LimitReached = true
	if err := repo.SaveUsage(ctx, u); err != nil {
		t.Fatal(err)
	}
	all, _ := repo.ListUsage(ctx)
	if len(all) != 1 || all[a.ID].Primary.UsedPercent != 80 || !all[a.ID].LimitReached {
		t.Fatalf("list = %+v", all)
	}

	if err := repo.SaveUsage(ctx, &domain.CodexUsage{}); err == nil {
		t.Fatal("a snapshot with no account id must be refused")
	}

	// Removing the account drops its snapshot.
	if err := repo.Delete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if all, _ := repo.ListUsage(ctx); len(all) != 0 {
		t.Fatalf("usage survived the account: %+v", all)
	}
}

func TestCodexRepo_OmniRouteHandOffMarker(t *testing.T) {
	repo, _ := setupCodexRepo(t)
	ctx := context.Background()
	a, _ := repo.Upsert(ctx, codexAcc("a@example.com", "1"))
	if !a.OmniRouteExportedAt.IsZero() {
		t.Fatalf("a new account is marked as exported: %v", a.OmniRouteExportedAt)
	}

	at := time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)
	if err := repo.SetOmniRouteExported(ctx, a.ID, at); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.GetByID(ctx, a.ID)
	if !got.OmniRouteExportedAt.Equal(at) {
		t.Fatalf("marker = %v, want %v", got.OmniRouteExportedAt, at)
	}

	// Refreshing tokens in place (what a rotation does) must not drop the marker.
	if err := repo.UpdateTokens(ctx, a.ID, "i2", "a2", "r2", at); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.GetByID(ctx, a.ID); got.OmniRouteExportedAt.IsZero() {
		t.Fatal("a token update cleared the hand-off marker")
	}

	// A fresh sign-in is a new token family that OmniRoute does not hold: the marker goes away.
	again := codexAcc("a@example.com", "1")
	again.RefreshToken = "brand-new-family"
	fresh, err := repo.Upsert(ctx, again)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.OmniRouteExportedAt.IsZero() || fresh.ID != a.ID {
		t.Fatalf("after a new sign-in: %+v", fresh)
	}

	// Explicit clear, and a missing account.
	_ = repo.SetOmniRouteExported(ctx, a.ID, at)
	if err := repo.SetOmniRouteExported(ctx, a.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.GetByID(ctx, a.ID); !got.OmniRouteExportedAt.IsZero() {
		t.Fatal("a zero time did not clear the marker")
	}
	if err := repo.SetOmniRouteExported(ctx, "missing", at); !errors.Is(err, domain.ErrCodexAccountNotFound) {
		t.Fatalf("missing account = %v", err)
	}
}

func TestCodexRepo_WarmupSchedules(t *testing.T) {
	repo, _ := setupCodexRepo(t)
	ctx := context.Background()
	a, _ := repo.Upsert(ctx, codexAcc("a@example.com", "1"))

	if _, err := repo.GetWarmup(ctx, a.ID); !errors.Is(err, domain.ErrCodexAccountNotFound) {
		t.Fatalf("GetWarmup with none = %v", err)
	}
	ran := time.Date(2026, 10, 1, 7, 0, 5, 0, time.UTC)
	w := &domain.CodexWarmup{AccountID: a.ID, Enabled: true, Times: []string{"07:00", "12:30"},
		LastFiredSlot: "2026-10-01 07:00", LastRunAt: ran, LastStatus: "ok", LastModel: "m1"}
	if err := repo.SaveWarmup(ctx, w); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetWarmup(ctx, a.ID)
	if err != nil || !got.Enabled || len(got.Times) != 2 || got.Times[1] != "12:30" || !got.LastRunAt.Equal(ran) || got.LastFiredSlot != "2026-10-01 07:00" {
		t.Fatalf("warm-up = %+v, %v", got, err)
	}

	w.Enabled = false
	w.LastStatus = "failed"
	if err := repo.SaveWarmup(ctx, w); err != nil {
		t.Fatal(err)
	}
	all, _ := repo.ListWarmups(ctx)
	if len(all) != 1 || all[a.ID].Enabled || all[a.ID].LastStatus != "failed" {
		t.Fatalf("list = %+v", all)
	}
	if err := repo.SaveWarmup(ctx, &domain.CodexWarmup{}); err == nil {
		t.Fatal("a schedule with no account id must be refused")
	}
	if err := repo.Delete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if all, _ := repo.ListWarmups(ctx); len(all) != 0 {
		t.Fatalf("the schedule survived the account: %+v", all)
	}
}
