package codex

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/egress"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/omniroute"
)

// ExportOutcome is what happened to one account in ExportToOmniRoute.
type ExportOutcome string

const (
	// ExportExported: the tokens were imported into OmniRoute and the account is marked handed off.
	ExportExported ExportOutcome = "exported"
	// ExportExists: OmniRoute already has this account (through its own login); our tokens were not
	// sent and the account is not marked.
	ExportExists ExportOutcome = "exists"
	// ExportSkipped: not exported on purpose (already handed off, or it needs a new sign-in).
	ExportSkipped ExportOutcome = "skipped"
	// ExportFailed: OmniRoute refused the import or could not be reached; nothing was marked.
	ExportFailed ExportOutcome = "failed"
)

// ExportOptions tunes ExportToOmniRoute.
type ExportOptions struct {
	// Overwrite replaces a connection OmniRoute already has for the same account with our tokens.
	// It never applies to an account already handed off: what we hold is stale by then, and sending
	// it would break the live session.
	Overwrite bool
	// DryRun classifies the accounts without importing, marking or binding anything.
	DryRun bool
	// AssignProxies binds each account's proxy to its OmniRoute connection (also for accounts
	// OmniRoute already had or we already handed off). It only changes a connection whose proxy
	// differs and never removes one.
	AssignProxies bool
}

// ExportResult is the outcome for one account.
type ExportResult struct {
	Account *domain.CodexAccount
	Outcome ExportOutcome
	Detail  string
	// Proxy describes the proxy binding ("" when none was attempted).
	Proxy string
}

// ExportToOmniRoute hands Codex accounts to OmniRoute (POST /api/providers/codex-auth/import-bulk).
//
// A Codex refresh token is single-use, so the tokens can live in only one place. Every account
// OmniRoute accepts is marked handed off, after which this switcher refuses to renew, read or
// switch to it (see ErrHandedOff): OmniRoute is now the one renewing them. The access token is not
// refreshed first, on purpose: that would rotate the refresh token for nothing.
//
// Nothing is imported unless OmniRoute's own Codex connections can be listed first, so an account it
// already holds is never duplicated.
func (s *Service) ExportToOmniRoute(ctx context.Context, c *omniroute.Client, accs []*domain.CodexAccount, opts ExportOptions) ([]ExportResult, error) {
	existing, err := c.ListConnections(ctx, "codex")
	if err != nil {
		return nil, fmt.Errorf("could not check which accounts OmniRoute already has, nothing imported: %w", err)
	}
	byEmail := map[string][]omniroute.Connection{}
	for _, conn := range existing {
		k := strings.ToLower(strings.TrimSpace(conn.Email))
		byEmail[k] = append(byEmail[k], conn)
	}

	results := make([]ExportResult, len(accs))
	connIDs := make([]string, len(accs))
	type candidate struct {
		idx   int
		entry omniroute.CodexEntry
	}
	var cands []candidate
	nameCount := map[string]int{}

	for i, acc := range accs {
		results[i] = ExportResult{Account: acc}
		key := strings.ToLower(strings.TrimSpace(acc.Email))
		conns := byEmail[key]

		switch {
		case !acc.OmniRouteExportedAt.IsZero():
			results[i].Outcome = ExportSkipped
			results[i].Detail = "already handed to OmniRoute on " + acc.OmniRouteExportedAt.Local().Format("2006-01-02 15:04") + "; OmniRoute renews it"
			connIDs[i] = uniqueConnID(conns)
			continue
		case acc.Status == domain.AccountStatusError:
			results[i].Outcome = ExportSkipped
			results[i].Detail = "its session was rejected: sign in again with codex-add first"
			continue
		case acc.Status == domain.AccountStatusDisabled:
			results[i].Outcome = ExportSkipped
			results[i].Detail = "disabled"
			continue
		case len(conns) > 0 && !opts.Overwrite:
			results[i].Outcome = ExportExists
			results[i].Detail = "OmniRoute already has it (its own login); our tokens were not sent"
			connIDs[i] = uniqueConnID(conns)
			continue
		}

		// Send the newest tokens: the Codex CLI may have rotated them in auth.json.
		if acc.IsActive {
			if fresh, err := s.CaptureRotation(ctx); err == nil && fresh != nil && fresh.ID == acc.ID {
				acc = fresh
				results[i].Account = fresh
			}
		}
		name := acc.Email
		nameCount[key]++
		if nameCount[key] > 1 || hasSameEmailLater(accs, i) {
			name = fmt.Sprintf("%s (%s)", acc.Email, shortID(acc.ChatGPTAccountID, acc.ID))
		}
		cands = append(cands, candidate{idx: i, entry: omniroute.CodexEntry{
			JSON: omniroute.BuildCodexAuthJSON(acc.IDToken, acc.AccessToken, acc.RefreshToken, acc.ChatGPTAccountID),
			Name: name, Email: acc.Email,
		}})
	}

	if opts.DryRun {
		for _, cd := range cands {
			results[cd.idx].Outcome = ExportExported
			results[cd.idx].Detail = "would be handed over (dry run)"
		}
		return results, nil
	}

	for start := 0; start < len(cands); start += omniroute.MaxBulkEntries {
		end := start + omniroute.MaxBulkEntries
		if end > len(cands) {
			end = len(cands)
		}
		chunk := cands[start:end]
		entries := make([]omniroute.CodexEntry, len(chunk))
		for j, cd := range chunk {
			entries[j] = cd.entry
		}
		res, err := c.ImportBulkCodex(ctx, entries, opts.Overwrite)
		if err != nil {
			for _, cd := range chunk {
				results[cd.idx].Outcome = ExportFailed
				results[cd.idx].Detail = err.Error()
			}
			continue
		}
		failedAt := map[int]string{}
		for _, e := range res.Errors {
			failedAt[e.Index] = e.Message
		}
		createdByName := map[string]omniroute.Connection{}
		for _, cr := range res.Created {
			createdByName[cr.Name] = cr
		}
		for j, cd := range chunk {
			r := &results[cd.idx]
			if msg, bad := failedAt[j]; bad {
				if strings.Contains(strings.ToLower(msg), "already exists") {
					r.Outcome, r.Detail = ExportExists, "OmniRoute already has it; our tokens were not sent"
					connIDs[cd.idx] = uniqueConnID(byEmail[strings.ToLower(strings.TrimSpace(r.Account.Email))])
				} else {
					r.Outcome, r.Detail = ExportFailed, msg
				}
				continue
			}
			conn, ok := createdByName[cd.entry.Name]
			if !ok {
				r.Outcome, r.Detail = ExportFailed, "OmniRoute did not report the imported connection"
				continue
			}
			r.Outcome = ExportExported
			connIDs[cd.idx] = conn.ID
			if err := s.Repo.SetOmniRouteExported(ctx, r.Account.ID, time.Now().UTC()); err != nil {
				// The tokens are in OmniRoute but we could not record it: say so loudly.
				r.Detail = "exported, but the hand-off could not be recorded (" + err.Error() + "): do NOT use this account here"
			}
		}
	}

	if opts.AssignProxies {
		for i := range results {
			if connIDs[i] == "" || results[i].Outcome == ExportFailed || strings.TrimSpace(results[i].Account.ProxyURL) == "" {
				continue
			}
			results[i].Proxy = bindProxy(ctx, c, connIDs[i], results[i].Account.ProxyURL)
		}
	}
	return results, nil
}

func uniqueConnID(conns []omniroute.Connection) string {
	if len(conns) == 1 {
		return conns[0].ID
	}
	return "" // none, or ambiguous: never guess which connection is ours
}

func hasSameEmailLater(accs []*domain.CodexAccount, i int) bool {
	for j := i + 1; j < len(accs); j++ {
		if strings.EqualFold(accs[j].Email, accs[i].Email) {
			return true
		}
	}
	return false
}

func shortID(primary, fallback string) string {
	s := primary
	if s == "" {
		s = fallback
	}
	if len(s) > 8 {
		s = s[:8]
	}
	return s
}

// bindProxy binds the account's proxy to its connection and describes the outcome. It never
// includes credentials.
func bindProxy(ctx context.Context, c *omniroute.Client, connectionID, proxyURL string) string {
	u, err := egress.ParseProxyURL(proxyURL)
	if err != nil {
		return "not bound: the stored proxy is unusable"
	}
	res, err := c.BindConnectionProxy(ctx, connectionID, u)
	switch {
	case err != nil:
		var msg = err.Error()
		if masked, ok := egress.MaskProxyURL(proxyURL); ok {
			msg = strings.ReplaceAll(msg, proxyURL, masked)
		}
		return "not bound: " + msg
	case res.Changed:
		return "bound (was " + res.Was + ")"
	default:
		return "already in effect"
	}
}
