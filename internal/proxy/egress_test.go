package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/domain"
	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/egress"
)

// unresolvableTarget can only be reached through a proxy that handles it: .invalid never resolves,
// so reaching it proves the switcher neither dialed nor resolved it from the local machine.
const unresolvableTarget = "api.example.invalid:443"

func createEgressAccount(t *testing.T, repo domain.AccountRepository, proxyURL string, active bool) {
	t.Helper()
	now := time.Now().UTC()
	acc := &domain.Account{
		ID:          "acc-egress",
		Email:       "user1@gmail.com",
		AccessToken: "token-1",
		ProxyURL:    proxyURL,
		IsActive:    active,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	// Create bypasses egress validation, which also lets tests seed legacy invalid values.
	if err := repo.Create(context.Background(), acc); err != nil {
		t.Fatalf("create account: %v", err)
	}
}

// fakeConnectProxy is an HTTP CONNECT proxy that records each requested target and then echoes.
func fakeConnectProxy(t *testing.T, targets chan<- string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				targets <- req.Host
				_, _ = c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
				_, _ = io.Copy(c, br)
			}()
		}
	}()
	return ln.Addr().String()
}

// rawConnect sends CONNECT target through the switcher and returns the parsed response and a
// reader positioned at the tunnel data.
func rawConnect(t *testing.T, switcherAddr, target string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	conn, err := net.Dial("tcp", switcherAddr)
	if err != nil {
		t.Fatalf("dial switcher: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	return conn, br, resp
}

type dialFunc = func(ctx context.Context, proxy *url.URL, addr string) (net.Conn, error)

// newSwitcher serves a ProxyHandler over HTTP. A non-nil dial replaces the tunnel dialer; it is set
// before the server starts so the handler goroutines observe it without a data race.
func newSwitcher(t *testing.T, repo domain.AccountRepository, dial dialFunc) (*ProxyHandler, string) {
	t.Helper()
	h, err := NewProxyHandler(repo)
	if err != nil {
		t.Fatalf("NewProxyHandler: %v", err)
	}
	if dial != nil {
		h.dial = dial
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return h, srv.Listener.Addr().String()
}

// recordingDial records the proxy each tunnel was routed through, then fails the dial.
func recordingDial() (dialFunc, <-chan *url.URL) {
	got := make(chan *url.URL, 1)
	return func(_ context.Context, p *url.URL, _ string) (net.Conn, error) {
		got <- p
		return nil, errors.New("recorded")
	}, got
}

// The leak this guards against: CONNECT tunnels (Antigravity's non-Cloud-Code HTTPS, voice
// included) used to be dialed directly, exposing the real IP next to the account's proxied traffic.
func TestConnect_TunnelsThroughActiveAccountProxy(t *testing.T) {
	targets := make(chan string, 1)
	proxyAddr := fakeConnectProxy(t, targets)

	_, repo, _, _ := setupTestDB(t)
	createEgressAccount(t, repo, "http://alice:s3cret@"+proxyAddr, true)
	_, switcherAddr := newSwitcher(t, repo, nil)

	conn, br, resp := rawConnect(t, switcherAddr, unresolvableTarget)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from switcher, got %s", resp.Status)
	}
	if got := <-targets; got != unresolvableTarget {
		t.Errorf("account proxy received CONNECT for %q, want %q", got, unresolvableTarget)
	}

	payload := []byte("voice-chunk")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("read through tunnel: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("tunnel echoed %q, want %q", got, payload)
	}
}

func TestConnect_InvalidAccountProxyFailsClosed(t *testing.T) {
	_, repo, _, _ := setupTestDB(t)
	createEgressAccount(t, repo, "1.2.3.4:8080:alice:s3cretPW", true) // legacy, never validated
	var dialed int32
	_, switcherAddr := newSwitcher(t, repo, func(context.Context, *url.URL, string) (net.Conn, error) {
		atomic.AddInt32(&dialed, 1)
		return nil, errors.New("must not dial")
	})

	_, _, resp := rawConnect(t, switcherAddr, unresolvableTarget)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %s", resp.Status)
	}
	if !strings.Contains(string(body), "egress blocked") {
		t.Errorf("expected an egress-blocked explanation, got %q", body)
	}
	if strings.Contains(string(body), "s3cretPW") {
		t.Errorf("response leaks the proxy password: %q", body)
	}
	if n := atomic.LoadInt32(&dialed); n != 0 {
		t.Errorf("dialed %d time(s); an invalid proxy must never fall back to a direct connection", n)
	}
}

func TestConnect_LoopbackTargetStaysLocal(t *testing.T) {
	_, repo, _, _ := setupTestDB(t)
	createEgressAccount(t, repo, "http://proxy.example.com:3128", true)
	dial, got := recordingDial()
	_, switcherAddr := newSwitcher(t, repo, dial)

	rawConnect(t, switcherAddr, "127.0.0.1:9")
	if p := <-got; p != nil {
		t.Errorf("loopback target was routed through %v; it must stay local", p)
	}
}

func TestConnect_AccountWithoutProxyGoesDirect(t *testing.T) {
	_, repo, _, _ := setupTestDB(t)
	createEgressAccount(t, repo, "", true)
	dial, got := recordingDial()
	_, switcherAddr := newSwitcher(t, repo, dial)

	rawConnect(t, switcherAddr, unresolvableTarget)
	if p := <-got; p != nil {
		t.Errorf("account has no proxy (its Cloud Code traffic is direct), yet CONNECT used %v", p)
	}
}

// With no active account yet, tunnels follow the account the next Cloud Code request would pick,
// rather than leaving directly in the gap before activation.
func TestConnect_NoActiveAccountFollowsNextAvailable(t *testing.T) {
	_, repo, _, _ := setupTestDB(t)
	createEgressAccount(t, repo, "http://proxy.example.com:3128", false)
	dial, got := recordingDial()
	_, switcherAddr := newSwitcher(t, repo, dial)

	rawConnect(t, switcherAddr, unresolvableTarget)
	if p := <-got; p == nil || p.Host != "proxy.example.com:3128" {
		t.Errorf("expected the next available account's proxy, got %v", p)
	}
}

// Plain-HTTP forward requests to other hosts carry no account but must still use its route.
func TestForwardPassThrough_UsesActiveAccountProxy(t *testing.T) {
	seen := make(chan string, 1)
	fakeProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.String() // absolute-form: the proxy was asked to fetch it
		_, _ = io.WriteString(w, "ok")
	}))
	defer fakeProxy.Close()

	_, repo, _, _ := setupTestDB(t)
	createEgressAccount(t, repo, fakeProxy.URL, true)
	_, switcherAddr := newSwitcher(t, repo, nil)

	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: switcherAddr})}}
	resp, err := client.Get("http://api.example.invalid/ping")
	if err != nil {
		t.Fatalf("forward request: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case got := <-seen:
		if got != "http://api.example.invalid/ping" {
			t.Errorf("account proxy fetched %q, want http://api.example.invalid/ping", got)
		}
	default:
		t.Fatal("forward request did not go through the active account's proxy")
	}
}

func TestGetClientForAccount_InvalidProxyFailsClosed(t *testing.T) {
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer upstream.Close()

	_, repo, _, _ := setupTestDB(t)
	h, err := NewProxyHandler(repo)
	if err != nil {
		t.Fatalf("NewProxyHandler: %v", err)
	}

	acc := &domain.Account{Email: "user1@gmail.com", ProxyURL: "1.2.3.4:8080"}
	resp, err := h.GetClientForAccount(acc).Get(upstream.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected the request to be refused")
	}
	if !errors.Is(err, egress.ErrInvalidProxy) {
		t.Errorf("expected ErrInvalidProxy, got %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("request reached upstream %d time(s) directly; must fail closed", n)
	}
}
