package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
)

var _ domain.CodexWarmupRepository = (*CodexAccountRepository)(nil)

// SaveWarmup replaces the account's warm-up schedule and last-run record. Deleting the account drops
// it (cascade).
func (r *CodexAccountRepository) SaveWarmup(ctx context.Context, w *domain.CodexWarmup) error {
	if w == nil || w.AccountID == "" {
		return errors.New("a warm-up schedule needs an account id")
	}
	payload, err := json.Marshal(w)
	if err != nil {
		return fmt.Errorf("failed to encode the warm-up schedule: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO codex_warmup (account_id, payload) VALUES (?, ?)
		ON CONFLICT(account_id) DO UPDATE SET payload = excluded.payload`, w.AccountID, string(payload))
	if err != nil {
		return fmt.Errorf("failed to save the warm-up schedule: %w", err)
	}
	return nil
}

// GetWarmup returns the schedule, or ErrCodexAccountNotFound when none was saved.
func (r *CodexAccountRepository) GetWarmup(ctx context.Context, accountID string) (*domain.CodexWarmup, error) {
	var payload string
	err := r.db.QueryRowContext(ctx, `SELECT payload FROM codex_warmup WHERE account_id = ?`, accountID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrCodexAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read the warm-up schedule: %w", err)
	}
	return decodeWarmup(payload)
}

// ListWarmups returns every schedule keyed by account id.
func (r *CodexAccountRepository) ListWarmups(ctx context.Context) (map[string]*domain.CodexWarmup, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT account_id, payload FROM codex_warmup`)
	if err != nil {
		return nil, fmt.Errorf("failed to list warm-up schedules: %w", err)
	}
	defer rows.Close()
	out := map[string]*domain.CodexWarmup{}
	for rows.Next() {
		var id, payload string
		if err := rows.Scan(&id, &payload); err != nil {
			return nil, fmt.Errorf("failed to scan a warm-up schedule: %w", err)
		}
		w, err := decodeWarmup(payload)
		if err != nil {
			continue // a corrupt row must not hide the others
		}
		out[id] = w
	}
	return out, rows.Err()
}

func decodeWarmup(payload string) (*domain.CodexWarmup, error) {
	var w domain.CodexWarmup
	if err := json.Unmarshal([]byte(payload), &w); err != nil {
		return nil, fmt.Errorf("the stored warm-up schedule is unreadable: %w", err)
	}
	return &w, nil
}
