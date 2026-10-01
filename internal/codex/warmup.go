package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
)

const (
	// ClientVersion is sent as client_version when listing models, as the Codex CLI does: the
	// backend hides models that need a newer client. It is the release this protocol was read from.
	ClientVersion = "0.159.3"

	// MaxWarmupSlotsPerDay caps a schedule: a warm-up spends a little quota, and more than a few a
	// day is not what it is for.
	MaxWarmupSlotsPerDay = 6

	warmupPrompt       = "ok"
	warmupInstructions = "Reply with the single word: ok"
	warmupReadLimit    = 512 << 10
)

var (
	// ErrNeedsSignIn marks an account whose session was rejected.
	ErrNeedsSignIn = errors.New("this account's session was rejected: sign in again with codex-add")
	// ErrNoModel means the backend listed no model to use.
	ErrNoModel = errors.New("the backend listed no usable model (pass one explicitly)")
)

// ParseTimes validates a list of local times of day ("7:00, 12:30", separated by commas, spaces or
// semicolons) and returns them normalized as "HH:MM", sorted and unique.
func ParseTimes(s string) ([]string, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' })
	seen := map[string]bool{}
	var out []string
	for _, f := range fields {
		t, err := time.Parse("15:04", f)
		if err != nil {
			return nil, fmt.Errorf("%q is not a time of day: use HH:MM (24-hour), e.g. 07:00", f)
		}
		hhmm := t.Format("15:04")
		if !seen[hhmm] {
			seen[hhmm] = true
			out = append(out, hhmm)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("give at least one time, e.g. 07:00")
	}
	if len(out) > MaxWarmupSlotsPerDay {
		return nil, fmt.Errorf("at most %d times per day", MaxWarmupSlotsPerDay)
	}
	sort.Strings(out)
	return out, nil
}

// ModelInfo is the part of a model entry the warm-up reads.
type ModelInfo struct {
	Slug       string `json:"slug"`
	Visibility string `json:"visibility"`
	Priority   int    `json:"priority"`
}

// PickModel chooses what the Codex CLI itself would default to: the visible model with the lowest
// priority number. Model names change often, so none is hard-coded.
func PickModel(models []ModelInfo) (string, error) {
	best := -1
	for i, m := range models {
		if m.Slug == "" || (m.Visibility != "" && m.Visibility != "list") {
			continue
		}
		if best < 0 || m.Priority < models[best].Priority {
			best = i
		}
	}
	if best < 0 {
		return "", ErrNoModel
	}
	return models[best].Slug, nil
}

func (c *Client) backend() string {
	if b := strings.TrimRight(c.BackendURL, "/"); b != "" {
		return b
	}
	return DefaultBackendURL
}

func (c *Client) authedRequest(ctx context.Context, method, path string, body io.Reader, accessToken, accountID string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.backend()+path, body)
	if err != nil {
		return nil, fmt.Errorf("build the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", usageUserAgent)
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
	return req, nil
}

func (c *Client) send(req *http.Request) (*http.Response, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("the backend is unreachable: %w", err)
	}
	return resp, nil
}

// ListModels reads the models the account can use (GET /codex/models), as the Codex CLI does.
func (c *Client) ListModels(ctx context.Context, accessToken, accountID string) ([]ModelInfo, error) {
	req, err := c.authedRequest(ctx, http.MethodGet, "/codex/models?client_version="+url.QueryEscape(ClientVersion), nil, accessToken, accountID)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.send(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, ErrUnauthorized
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("the models endpoint answered HTTP %d", resp.StatusCode)
	}
	var out struct {
		Models []ModelInfo `json:"models"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, errors.New("the models endpoint returned an unreadable response")
	}
	return out.Models, nil
}

// WarmUp sends one minimal request (POST /codex/responses, streamed) so the account's rate-limit
// window starts now. It reads the stream only until the response completes. Failures never include
// the response body or the token.
func (c *Client) WarmUp(ctx context.Context, accessToken, accountID, model string) error {
	payload := map[string]any{
		"model":               model,
		"stream":              true,
		"instructions":        warmupInstructions,
		"input":               []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": warmupPrompt}}}},
		"tool_choice":         "auto",
		"parallel_tool_calls": false,
		"reasoning":           map[string]any{"effort": "low"},
		"store":               false,
		"include":             []string{},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode the request: %w", err)
	}
	req, err := c.authedRequest(ctx, http.MethodPost, "/codex/responses", bytes.NewReader(b), accessToken, accountID)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.send(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case resp.StatusCode == http.StatusTooManyRequests:
		return errors.New("the account is rate limited right now (its window may already be open or exhausted)")
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("the backend answered HTTP %d", resp.StatusCode)
	}
	return readUntilCompleted(io.LimitReader(resp.Body, warmupReadLimit))
}

// readUntilCompleted scans server-sent events for the end of the response.
func readUntilCompleted(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev struct {
			Type     string `json:"type"`
			Response struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			} `json:"response"`
			Code string `json:"code"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "response.completed":
			return nil
		case "response.failed", "error":
			code := sanitizeCode(firstNonEmpty(ev.Response.Error.Code, ev.Code))
			if code == "" || code == "error" {
				return errors.New("the backend reported a failure")
			}
			return fmt.Errorf("the backend reported a failure (%s)", code)
		}
	}
	return errors.New("the response stream ended without completing")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// WarmOptions tunes Service.WarmUp.
type WarmOptions struct {
	RefreshOptions
	// Model forces a model; empty discovers one from the account's model list.
	Model string
}

// WarmResult describes a successful warm-up.
type WarmResult struct {
	Model string
	At    time.Time
}

// authed runs call with the account's access token, renewing it once when the backend rejects it
// (which may rotate the refresh token) and retrying. It returns the account the call ran for.
func (s *Service) authed(ctx context.Context, id string, opts RefreshOptions, call func(c *Client, accessToken, accountID string) error) (*domain.CodexAccount, error) {
	acc, err := s.Repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := checkHandedOff(acc, opts.Force); err != nil {
		return nil, err
	}
	if acc.Status == domain.AccountStatusError {
		return nil, ErrNeedsSignIn
	}
	if acc.ProxyURL == "" && !opts.AllowDirect {
		return nil, ErrProxyRequired
	}
	if acc.IsActive {
		// The CLI may have rotated the tokens since we stored them: start from its newest.
		if fresh, err := s.CaptureRotation(ctx); err == nil && fresh != nil && fresh.ID == acc.ID {
			acc = fresh
		}
	}
	client, err := s.NewClient(acc.ProxyURL)
	if err != nil {
		return nil, err
	}
	err = call(client, acc.AccessToken, acc.ChatGPTAccountID)
	if errors.Is(err, ErrUnauthorized) {
		renewed, rerr := s.Refresh(ctx, acc.ID, opts)
		if rerr != nil {
			return nil, fmt.Errorf("the access token expired and could not be renewed: %w", rerr)
		}
		acc = renewed
		err = call(client, renewed.AccessToken, renewed.ChatGPTAccountID)
	}
	if err != nil {
		return nil, err
	}
	return acc, nil
}

// WarmUp sends the account one minimal request through its own proxy to start its rate-limit window,
// and records the outcome. It refuses an account with no proxy, one handed to OmniRoute, or one that
// needs a new sign-in, exactly like every other call that reaches OpenAI.
func (s *Service) WarmUp(ctx context.Context, id string, opts WarmOptions) (*WarmResult, error) {
	model := strings.TrimSpace(opts.Model)
	_, err := s.authed(ctx, id, opts.RefreshOptions, func(c *Client, token, acct string) error {
		if model == "" {
			models, err := c.ListModels(ctx, token, acct)
			if err != nil {
				return err
			}
			if model, err = PickModel(models); err != nil {
				return err
			}
		}
		return c.WarmUp(ctx, token, acct, model)
	})
	if err != nil {
		s.recordWarmup(ctx, id, warmStatusFor(err), err.Error(), model)
		return nil, err
	}
	res := &WarmResult{Model: model, At: s.now().UTC()}
	s.recordWarmup(ctx, id, "ok", "", model)
	return res, nil
}

// warmStatusFor classifies a failure: refusals that are the account's state, not an attempt.
func warmStatusFor(err error) string {
	switch {
	case errors.Is(err, ErrHandedOff), errors.Is(err, ErrProxyRequired), errors.Is(err, ErrNeedsSignIn):
		return "skipped"
	}
	return "failed"
}

// recordWarmup stores the latest attempt on the account's warm-up record, keeping its schedule.
func (s *Service) recordWarmup(ctx context.Context, id, status, detail, model string) {
	if s.Warmups == nil {
		return
	}
	w, err := s.Warmups.GetWarmup(ctx, id)
	if err != nil {
		w = &domain.CodexWarmup{AccountID: id}
	}
	w.LastRunAt, w.LastStatus, w.LastDetail = s.now().UTC(), status, detail
	if model != "" {
		w.LastModel = model
	}
	_ = s.Warmups.SaveWarmup(ctx, w)
}

// SetWarmupSchedule turns an account's scheduled warm-up on or off and sets its times. Enabling
// marks the slots already past today as handled, so turning it on at 15:00 does not fire the 12:00
// slot.
func (s *Service) SetWarmupSchedule(ctx context.Context, id string, times []string, enabled bool) (*domain.CodexWarmup, error) {
	if s.Warmups == nil {
		return nil, errors.New("warm-up schedules are not stored")
	}
	if _, err := s.Repo.GetByID(ctx, id); err != nil {
		return nil, err
	}
	if enabled && len(times) == 0 {
		return nil, errors.New("give at least one time to enable the warm-up")
	}
	w, err := s.Warmups.GetWarmup(ctx, id)
	if err != nil {
		w = &domain.CodexWarmup{AccountID: id}
	}
	w.Enabled, w.Times = enabled, times
	if enabled {
		w.LastFiredSlot = latestSlot(s.now(), times)
	}
	if err := s.Warmups.SaveWarmup(ctx, w); err != nil {
		return nil, err
	}
	return w, nil
}

// slotString formats a slot as the scheduler tracks it.
func slotString(day time.Time, hhmm string) string {
	return day.Format("2006-01-02") + " " + hhmm
}

// latestSlot returns today's latest slot at or before now ("" when none has passed yet).
func latestSlot(now time.Time, times []string) string {
	nowStr := now.Format("15:04")
	last := ""
	for _, t := range times {
		if t <= nowStr {
			last = slotString(now, t)
		}
	}
	return last
}
