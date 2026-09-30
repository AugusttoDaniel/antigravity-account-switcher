package main

import (
	"testing"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
)

func TestFormatUsageWindow(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		w    *domain.CodexUsageWindow
		want string
	}{
		"missing":      {nil, "-"},
		"no reset":     {&domain.CodexUsageWindow{UsedPercent: 5}, "5% used"},
		"due":          {&domain.CodexUsageWindow{UsedPercent: 100, ResetAt: now.Add(-time.Minute)}, "100% used (reset due)"},
		"minutes":      {&domain.CodexUsageWindow{UsedPercent: 42, ResetAt: now.Add(25 * time.Minute)}, "42% used, resets in 25m"},
		"hours":        {&domain.CodexUsageWindow{UsedPercent: 42, ResetAt: now.Add(3*time.Hour + 12*time.Minute)}, "42% used, resets in 3h12m"},
		"days":         {&domain.CodexUsageWindow{UsedPercent: 7, ResetAt: now.Add(74 * time.Hour)}, "7% used, resets in 3d2h"},
		"just under 2": {&domain.CodexUsageWindow{UsedPercent: 7, ResetAt: now.Add(47 * time.Hour)}, "7% used, resets in 47h00m"},
	}
	for name, c := range cases {
		if got := formatUsageWindow(c.w, now); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}
