package codex

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
)

// Scheduler fires the warm-ups the user scheduled. It does nothing for an account until that
// account's schedule is enabled, so a switcher with no schedules makes no request at all.
//
// Design limits, on purpose: each slot runs at most once (no retry after a failure), accounts run one
// at a time with a pause between them, a slot that was missed by more than Grace (the switcher was
// not running) is recorded as missed rather than fired late, and the start of each slot is offset by
// a stable per-account jitter so requests do not all land on the exact minute.
type Scheduler struct {
	Svc *Service
	// Now returns the current local time (default time.Now).
	Now func() time.Time
	// Tick is how often schedules are evaluated (default 30s).
	Tick time.Duration
	// Grace is how late a slot may still fire (default 10m).
	Grace time.Duration
	// MaxJitter bounds the per-account offset of a slot (default 90s; 0 disables it).
	MaxJitter time.Duration
	// Gap is the pause between two accounts due together (default 5s).
	Gap time.Duration
	// RunTimeout bounds one warm-up (default 90s).
	RunTimeout time.Duration
	// OnEvent receives a short, credential-free line per attempt (kind: "ok", "failed", "skipped", "missed").
	OnEvent func(kind, accountID, message string)
}

// RunRecord is one attempt made by RunOnce.
type RunRecord struct {
	AccountID string
	Email     string
	Slot      string
	Status    string
	Detail    string
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func orDur(v, def time.Duration) time.Duration {
	if v == 0 {
		return def
	}
	return v
}

// Run evaluates the schedules every Tick until ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(orDur(s.Tick, 30*time.Second))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.RunOnce(ctx)
		}
	}
}

// jitterFor derives a stable offset in [0, max) from the account and slot, so a slot always fires at
// the same offset (and tests are deterministic).
func jitterFor(accountID, slot string, max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	h := sha256.Sum256([]byte(accountID + "|" + slot))
	return time.Duration(binary.BigEndian.Uint64(h[:8]) % uint64(max))
}

// RunOnce evaluates every enabled schedule once and runs what is due. It returns what it did.
func (s *Scheduler) RunOnce(ctx context.Context) []RunRecord {
	if s.Svc == nil || s.Svc.Warmups == nil {
		return nil
	}
	schedules, err := s.Svc.Warmups.ListWarmups(ctx)
	if err != nil || len(schedules) == 0 {
		return nil
	}
	now := s.now()
	grace := orDur(s.Grace, 10*time.Minute)
	maxJitter := s.MaxJitter
	if maxJitter == 0 {
		maxJitter = 90 * time.Second
	}

	ids := make([]string, 0, len(schedules))
	for id, w := range schedules {
		if w.Enabled {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	var records []RunRecord
	first := true
	for _, id := range ids {
		w := schedules[id]
		slot := latestSlot(now, w.Times)
		if slot == "" || slot <= w.LastFiredSlot {
			continue // nothing new has come due
		}
		slotAt, err := time.ParseInLocation("2006-01-02 15:04", slot, now.Location())
		if err != nil {
			continue
		}
		fireAt := slotAt.Add(jitterFor(id, slot, maxJitter))
		if now.Before(fireAt) {
			continue // the slot has begun but this account's offset has not elapsed yet
		}
		acc, err := s.Svc.Repo.GetByID(ctx, id)
		if err != nil {
			continue
		}

		// Mark the slot handled BEFORE running: a crash or a hang must never make it repeat.
		w.LastFiredSlot = slot
		if now.Sub(slotAt) > grace {
			w.LastRunAt, w.LastStatus, w.LastDetail = now.UTC(), "missed", "the switcher was not running at "+slot[11:]
			_ = s.Svc.Warmups.SaveWarmup(ctx, w)
			records = append(records, s.record(acc, slot, "missed", w.LastDetail))
			continue
		}
		if err := s.Svc.Warmups.SaveWarmup(ctx, w); err != nil {
			continue
		}

		if !first {
			select {
			case <-ctx.Done():
				return records
			case <-time.After(orDur(s.Gap, 5*time.Second)):
			}
		}
		first = false

		runCtx, cancel := context.WithTimeout(ctx, orDur(s.RunTimeout, 90*time.Second))
		_, werr := s.Svc.WarmUp(runCtx, id, WarmOptions{})
		cancel()
		status, detail := "ok", ""
		if werr != nil {
			status, detail = warmStatusFor(werr), werr.Error()
		}
		records = append(records, s.record(acc, slot, status, detail))
	}
	return records
}

func (s *Scheduler) record(acc *domain.CodexAccount, slot, status, detail string) RunRecord {
	if s.OnEvent != nil {
		msg := fmt.Sprintf("Codex warm-up %s for %s (slot %s)", status, acc.Email, slot[11:])
		if detail != "" {
			msg += ": " + detail
		}
		s.OnEvent(status, acc.ID, msg)
	}
	return RunRecord{AccountID: acc.ID, Email: acc.Email, Slot: slot, Status: status, Detail: detail}
}
