package proxypool

import (
	"testing"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
)

func TestEntriesMergeFindRemove(t *testing.T) {
	a := "http://alice:pw@proxy-a.example:8080"
	b := "http://bob:pw@proxy-b.example:8080"
	c := "socks5://proxy-c.example:1080"

	merged, added := Merge([]string{a, " ", b}, []string{b, "HTTP://BOB:PW@PROXY-B.EXAMPLE:8080", c, ""})
	if added != 1 || len(merged) != 3 || merged[2] != c {
		t.Fatalf("Merge = %v (added %d); want a, b, c with only c added", merged, added)
	}

	entries := Entries(merged, []*domain.Account{
		{Email: "user1@gmail.com", ProxyURL: b},
		{Email: "user2@gmail.com"},
		nil,
	})
	if len(entries) != 3 {
		t.Fatalf("Entries: %+v", entries)
	}
	if entries[1].UsedBy != "user1@gmail.com" || entries[0].UsedBy != "" || entries[2].UsedBy != "" {
		t.Errorf("bindings wrong: %+v", entries)
	}
	if entries[0].ID == entries[1].ID || len(entries[0].ID) != 12 {
		t.Errorf("IDs must be distinct 12-char hashes: %q %q", entries[0].ID, entries[1].ID)
	}
	if ID(" HTTP://ALICE:PW@proxy-a.example:8080 ") != entries[0].ID {
		t.Error("ID must be stable across trivial spelling differences")
	}

	if got, ok := Find(merged, entries[2].ID); !ok || got != c {
		t.Errorf("Find = %q, %v", got, ok)
	}
	if _, ok := Find(merged, "000000000000"); ok {
		t.Error("Find matched an unknown ID")
	}

	rest, removed := Remove(merged, entries[0].ID)
	if !removed || len(rest) != 2 || rest[0] != b {
		t.Errorf("Remove = %v, %v", rest, removed)
	}
	if _, removed := Remove(rest, entries[0].ID); removed {
		t.Error("removing an absent ID reported success")
	}
}
