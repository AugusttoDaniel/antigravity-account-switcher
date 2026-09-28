package adspower

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPickPageTargetPrefersBlank(t *testing.T) {
	pages := []devtoolsTarget{
		{ID: "a", Type: "page", URL: "https://example.com/app"},
		{ID: "b", Type: "page", URL: "about:blank"},
	}
	got, ok := pickPageTarget(pages)
	if !ok || got.ID != "b" {
		t.Errorf("expected blank tab b, got %+v ok=%v", got, ok)
	}
}

func TestPickPageTargetFallsBackToFirst(t *testing.T) {
	pages := []devtoolsTarget{
		{ID: "a", Type: "page", URL: "https://example.com/app"},
	}
	got, ok := pickPageTarget(pages)
	if !ok || got.ID != "a" {
		t.Errorf("expected first tab a, got %+v ok=%v", got, ok)
	}
}

func TestPickPageTargetEmpty(t *testing.T) {
	if _, ok := pickPageTarget(nil); ok {
		t.Error("expected no target for empty list")
	}
}

func TestListPageTargetsFiltersPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":"p1","type":"page","url":"about:blank","webSocketDebuggerUrl":"ws://x/p1"},
			{"id":"bg","type":"service_worker","url":"chrome-extension://z","webSocketDebuggerUrl":"ws://x/bg"},
			{"id":"p2","type":"page","url":"https://ex.com","webSocketDebuggerUrl":"ws://x/p2"}
		]`))
	}))
	defer srv.Close()

	// httptest listens on 127.0.0.1; ListPageTargets rebuilds the URL from the port, so pass it.
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	pages, err := ListPageTargets(context.Background(), srv.Client(), port)
	if err != nil {
		t.Fatalf("ListPageTargets: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("expected 2 page targets, got %d", len(pages))
	}
	for _, p := range pages {
		if p.Type != "page" {
			t.Errorf("non-page target leaked: %+v", p)
		}
	}
}
