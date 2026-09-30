package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
	"github.com/google/uuid"
)

const codexColumns = `id, email, chatgpt_account_id, plan_type, id_token, access_token, refresh_token,
	last_refresh, proxy_url, adspower_profile_id, is_active, status, created_at, updated_at, omniroute_exported_at`

// CodexAccountRepository implements domain.CodexAccountRepository backed by SQLite.
type CodexAccountRepository struct {
	db *DB
}

var _ domain.CodexAccountRepository = (*CodexAccountRepository)(nil)

// NewCodexAccountRepository creates a SQLite-backed Codex account repository.
func NewCodexAccountRepository(db *DB) *CodexAccountRepository {
	return &CodexAccountRepository{db: db}
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "1970-01-01T00:00:00Z"
	}
	return t.UTC().Format(time.RFC3339)
}

// Upsert inserts the account or, for the same (email, ChatGPT account id), refreshes it.
func (r *CodexAccountRepository) Upsert(ctx context.Context, acc *domain.CodexAccount) (*domain.CodexAccount, error) {
	if strings.TrimSpace(acc.Email) == "" || acc.RefreshToken == "" {
		return nil, errors.New("a codex account needs an email and a refresh token")
	}
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var id string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM codex_accounts WHERE lower(email) = lower(?) AND chatgpt_account_id = ?`,
		acc.Email, acc.ChatGPTAccountID).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		id = acc.ID
		if id == "" {
			id = uuid.NewString()
		}
		status := acc.Status
		if status == "" {
			status = domain.AccountStatusActive
		}
		created := acc.CreatedAt
		if created.IsZero() {
			created = time.Now().UTC()
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO codex_accounts (`+codexColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, acc.Email, acc.ChatGPTAccountID, acc.PlanType, acc.IDToken, acc.AccessToken, acc.RefreshToken,
			fmtTime(acc.LastRefresh), acc.ProxyURL, acc.AdsPowerProfileID, 0, string(status), fmtTime(created), now, exportedStr(acc.OmniRouteExportedAt))
		if err != nil {
			return nil, fmt.Errorf("failed to create codex account: %w", err)
		}
	case err != nil:
		return nil, fmt.Errorf("failed to look up codex account: %w", err)
	default:
		// Same identity: refresh credentials; keep proxy/profile unless a new one is given.
		_, err = tx.ExecContext(ctx, `UPDATE codex_accounts SET
				plan_type = ?, id_token = ?, access_token = ?, refresh_token = ?, last_refresh = ?,
				proxy_url = CASE WHEN ? != '' THEN ? ELSE proxy_url END,
				adspower_profile_id = CASE WHEN ? != '' THEN ? ELSE adspower_profile_id END,
				status = 'active', updated_at = ?,
				-- a fresh sign-in is a new token family that OmniRoute does not hold
				omniroute_exported_at = ''
			WHERE id = ?`,
			acc.PlanType, acc.IDToken, acc.AccessToken, acc.RefreshToken, fmtTime(acc.LastRefresh),
			acc.ProxyURL, acc.ProxyURL, acc.AdsPowerProfileID, acc.AdsPowerProfileID, now, id)
		if err != nil {
			return nil, fmt.Errorf("failed to update codex account: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit: %w", err)
	}
	return r.GetByID(ctx, id)
}

// GetByID retrieves a Codex account by id.
func (r *CodexAccountRepository) GetByID(ctx context.Context, id string) (*domain.CodexAccount, error) {
	return scanCodex(r.db.QueryRowContext(ctx, `SELECT `+codexColumns+` FROM codex_accounts WHERE id = ?`, id))
}

// GetActive retrieves the active Codex account.
func (r *CodexAccountRepository) GetActive(ctx context.Context) (*domain.CodexAccount, error) {
	return scanCodex(r.db.QueryRowContext(ctx, `SELECT `+codexColumns+` FROM codex_accounts WHERE is_active = 1 LIMIT 1`))
}

// List returns every Codex account, oldest first.
func (r *CodexAccountRepository) List(ctx context.Context) ([]*domain.CodexAccount, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+codexColumns+` FROM codex_accounts ORDER BY created_at ASC, email ASC`)
	if err != nil {
		return nil, fmt.Errorf("failed to list codex accounts: %w", err)
	}
	defer rows.Close()
	var out []*domain.CodexAccount
	for rows.Next() {
		a, err := scanCodex(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetActive makes one account the active one.
func (r *CodexAccountRepository) SetActive(ctx context.Context, id string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `UPDATE codex_accounts SET is_active = 0, updated_at = ? WHERE is_active = 1`, now); err != nil {
		return fmt.Errorf("failed to deactivate the current codex account: %w", err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE codex_accounts SET is_active = 1, updated_at = ? WHERE id = ?`, now, id)
	if err != nil {
		return fmt.Errorf("failed to activate codex account: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.ErrCodexAccountNotFound
	}
	return tx.Commit()
}

func (r *CodexAccountRepository) update(ctx context.Context, id, set string, args ...any) error {
	args = append(args, time.Now().UTC().Format(time.RFC3339), id)
	res, err := r.db.ExecContext(ctx, `UPDATE codex_accounts SET `+set+`, updated_at = ? WHERE id = ?`, args...)
	if err != nil {
		return fmt.Errorf("failed to update codex account: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.ErrCodexAccountNotFound
	}
	return nil
}

// UpdateTokens stores rotated credentials.
func (r *CodexAccountRepository) UpdateTokens(ctx context.Context, id, idToken, accessToken, refreshToken string, lastRefresh time.Time) error {
	if refreshToken == "" {
		return errors.New("refusing to store an empty refresh token")
	}
	return r.update(ctx, id, `id_token = ?, access_token = ?, refresh_token = ?, last_refresh = ?`,
		idToken, accessToken, refreshToken, fmtTime(lastRefresh))
}

// UpdateStatus sets the operational status.
func (r *CodexAccountRepository) UpdateStatus(ctx context.Context, id string, status domain.AccountStatus) error {
	return r.update(ctx, id, `status = ?`, string(status))
}

// UpdateProxyURL sets the account's egress proxy.
func (r *CodexAccountRepository) UpdateProxyURL(ctx context.Context, id, proxyURL string) error {
	return r.update(ctx, id, `proxy_url = ?`, proxyURL)
}

// UpdateAdsPowerProfileID records the browser profile the account was onboarded through.
func (r *CodexAccountRepository) UpdateAdsPowerProfileID(ctx context.Context, id, profileID string) error {
	return r.update(ctx, id, `adspower_profile_id = ?`, profileID)
}

// Delete removes the account.
func (r *CodexAccountRepository) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM codex_accounts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("failed to delete codex account: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.ErrCodexAccountNotFound
	}
	return nil
}

func scanCodex(s rowScanner) (*domain.CodexAccount, error) {
	var a domain.CodexAccount
	var status, lastRefresh, created, updated, exported string
	var active int
	err := s.Scan(&a.ID, &a.Email, &a.ChatGPTAccountID, &a.PlanType, &a.IDToken, &a.AccessToken, &a.RefreshToken,
		&lastRefresh, &a.ProxyURL, &a.AdsPowerProfileID, &active, &status, &created, &updated, &exported)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrCodexAccountNotFound
		}
		return nil, fmt.Errorf("failed to scan codex account: %w", err)
	}
	a.IsActive = active == 1
	a.Status = domain.AccountStatus(status)
	a.LastRefresh, _ = parseDBTime(lastRefresh)
	a.CreatedAt, _ = parseDBTime(created)
	a.UpdatedAt, _ = parseDBTime(updated)
	if exported != "" {
		a.OmniRouteExportedAt, _ = parseDBTime(exported)
	}
	return &a, nil
}

func exportedStr(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// SetOmniRouteExported records the hand-off of the account's tokens to OmniRoute; a zero time clears it.
func (r *CodexAccountRepository) SetOmniRouteExported(ctx context.Context, id string, at time.Time) error {
	return r.update(ctx, id, `omniroute_exported_at = ?`, exportedStr(at))
}
