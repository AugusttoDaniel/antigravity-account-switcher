package main

import (
	"testing"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/adspower"
)

func TestFilterProfiles(t *testing.T) {
	profiles := []adspower.Profile{
		{UserID: "1", Name: "work-alice"},
		{UserID: "2", Name: "WORK-bob"},
		{UserID: "3", Name: "personal-carol"},
	}

	if got := filterProfiles(profiles, ""); len(got) != 3 {
		t.Errorf("empty filter should keep all, got %d", len(got))
	}

	got := filterProfiles(profiles, "work")
	if len(got) != 2 {
		t.Fatalf("expected 2 matches (case-insensitive), got %d", len(got))
	}
	for _, p := range got {
		if p.UserID == "3" {
			t.Errorf("personal-carol should not match 'work'")
		}
	}

	if got := filterProfiles(profiles, "nope"); len(got) != 0 {
		t.Errorf("no matches expected, got %d", len(got))
	}
}

func TestMaskProxy(t *testing.T) {
	cases := map[string]string{
		"":                                   "(direct)",
		"http://user:pass@host.example:8080": "http://***@host.example:8080",
		"socks5://1.2.3.4:1080":              "socks5://1.2.3.4:1080",
	}
	for in, want := range cases {
		if got := maskProxy(in); got != want {
			t.Errorf("maskProxy(%q) = %q, want %q", in, got, want)
		}
	}
}
