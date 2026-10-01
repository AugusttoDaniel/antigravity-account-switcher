package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/omniroute"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// OmniRoute's redirect lands on the loopback listener, which hands the code over; a redirect with
// the wrong state is refused and does not end the wait.
func TestAwaitOmniRouteCode_TheRedirectDeliversTheCode(t *testing.T) {
	port := freePort(t)
	start := &omniroute.OAuthStart{State: "S1", RedirectURI: fmt.Sprintf("http://localhost:%d/callback", port)}

	type res struct {
		code string
		err  error
	}
	done := make(chan res, 1)
	go func() {
		c, err := awaitOmniRouteCode(context.Background(), start, port, 10*time.Second)
		done <- res{c, err}
	}()

	base := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(base + "?code=bad&state=OTHER")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("wrong state answered %d, want 400", resp.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the listener never came up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case r := <-done:
		t.Fatalf("a redirect with the wrong state ended the wait: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}

	resp, err := http.Get(base + "?code=the-code&state=S1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	select {
	case r := <-done:
		if r.err != nil || r.code != "the-code" {
			t.Fatalf("got %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the code was never delivered")
	}
}

func TestAwaitOmniRouteCode_TimesOut(t *testing.T) {
	start := &omniroute.OAuthStart{State: "S1", RedirectURI: "http://localhost:1/callback"}
	if _, err := awaitOmniRouteCode(context.Background(), start, freePort(t), 150*time.Millisecond); err == nil {
		t.Fatal("expected a timeout")
	}
}
