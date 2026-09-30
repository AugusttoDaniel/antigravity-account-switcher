package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
)

var _ domain.CodexUsageRepository = (*CodexAccountRepository)(nil)

// SaveUsage replaces the account's usage snapshot. Deleting the account drops it (cascade).
func (r *CodexAccountRepository) SaveUsage(ctx context.Context, u *domain.CodexUsage) error {
	if u == nil || u.AccountID == "" {
		return errors.New("a usage snapshot needs an account id")
	}
	if u.FetchedAt.IsZero() {
		u.FetchedAt = time.Now().UTC()
	}
	payload, err := json.Marshal(u)
	if err != nil {
		return fmt.Errorf("failed to encode usage: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO codex_usage (account_id, payload, fetched_at) VALUES (?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET payload = excluded.payload, fetched_at = excluded.fetched_at`,
		u.AccountID, string(payload), fmtTime(u.FetchedAt))
	if err != nil {
		return fmt.Errorf("failed to save usage: %w", err)
	}
	return nil
}

// GetUsage returns the stored snapshot, or ErrCodexAccountNotFound when there is none.
func (r *CodexAccountRepository) GetUsage(ctx context.Context, accountID string) (*domain.CodexUsage, error) {
	var payload string
	err := r.db.QueryRowContext(ctx, `SELECT payload FROM codex_usage WHERE account_id = ?`, accountID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrCodexAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read usage: %w", err)
	}
	return decodeUsage(payload)
}

// ListUsage returns every snapshot keyed by account id.
func (r *CodexAccountRepository) ListUsage(ctx context.Context) (map[string]*domain.CodexUsage, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT account_id, payload FROM codex_usage`)
	if err != nil {
		return nil, fmt.Errorf("failed to list usage: %w", err)
	}
	defer rows.Close()
	out := map[string]*domain.CodexUsage{}
	for rows.Next() {
		var id, payload string
		if err := rows.Scan(&id, &payload); err != nil {
			return nil, fmt.Errorf("failed to scan usage: %w", err)
		}
		u, err := decodeUsage(payload)
		if err != nil {
			continue // a corrupt row must not hide the others
		}
		out[id] = u
	}
	return out, rows.Err()
}

func decodeUsage(payload string) (*domain.CodexUsage, error) {
	var u domain.CodexUsage
	if err := json.Unmarshal([]byte(payload), &u); err != nil {
		return nil, fmt.Errorf("stored usage is unreadable: %w", err)
	}
	return &u, nil
}
