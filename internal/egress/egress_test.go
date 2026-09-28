package egress

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// unresolvableTarget can only be reached if the proxy (not this process) handles it: .invalid is
// reserved and never resolves, so a successful tunnel proves nothing was dialed or resolved locally.
const unresolvableTarget = "api.example.invalid:443"

func TestValidateProxyURL(t *testing.T) {
	valid := []string{
		"",
		"http://127.0.0.1:8080",
		"http://user:pass@proxy.example.com:3128",
		"https://proxy.example.com:443",
		"socks5://10.0.0.1:1080",
		"socks5h://host:1080",
		"http://proxy.example.com", // default port
	}
	for _, v := range valid {
		if err := ValidateProxyURL(v); err != nil {
			t.Errorf("expected %q to be valid, got %v", v, err)
		}
	}

	invalid := []string{
		"1.2.3.4:8080",               // no scheme: would have silently fallen back to direct
		"proxy.example.com",          // no scheme
		"1.2.3.4:8080:user:pass",     // provider-list format pasted verbatim
		"user:pass@proxy.example:80", // no scheme; "user" would be misread as the scheme
		"ftp://host:21",              // unsupported scheme
		"http://",                    // missing host
		"http://host:0",              // port out of range
		"http://host:99999",          // port out of range
	}
	for _, v := range invalid {
		err := ValidateProxyURL(v)
		if err == nil {
			t.Errorf("expected %q to be rejected, got nil error", v)
			continue
		}
		if !errors.Is(err, ErrInvalidProxy) {
			t.Errorf("%q: error %v does not wrap ErrInvalidProxy", v, err)
		}
	}
}

// Proxy errors reach logs and the dashboard, so they must never contain the credentials.
func TestParseProxyURL_ErrorsNeverEchoCredentials(t *testing.T) {
	inputs := []string{
		"1.2.3.4:8080:alice:s3cretPW",
		"alice:s3cretPW@proxy.example:80",
		"http://alice:s3cretPW%zz@proxy.example:80", // invalid escape inside the password
		"ftp://alice:s3cretPW@proxy.example:21",
	}
	for _, in := range inputs {
		_, err := ParseProxyURL(in)
		if err == nil {
			t.Fatalf("expected %q to be rejected", in)
		}
		msg := err.Error()
		if strings.Contains(msg, "s3cretPW") || strings.Contains(msg, "alice") {
			t.Errorf("error for %q leaks credentials: %q", in, msg)
		}
	}
}

func TestParseProxyURL_HintsProviderListFormat(t *testing.T) {
	_, err := ParseProxyURL("1.2.3.4:8080:alice:s3cretPW")
	if err == nil {
		t.Fatal("expected an error")
	}
	if want := "http://<user>:<pass>@1.2.3.4:8080"; !strings.Contains(err.Error(), want) {
		t.Errorf("expected hint %q in %q", want, err.Error())
	}
}

func TestIsLoopbackHost(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8080":        true,
		"127.0.0.1":             true,
		"localhost:443":         true,
		"LOCALHOST":             true,
		"[::1]:443":             true,
		"::1":                   true,
		"10.0.0.1:443":          false,
		"speech.googleapis.com": false,
		"example.com:443":       false,
		"localhost.evil.com:80": false,
	}
	for in, want := range cases {
		if got := IsLoopbackHost(in); got != want {
			t.Errorf("IsLoopbackHost(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestFailClosedClient_NeverReachesNetwork(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()

	cause := errors.New("bad proxy")
	resp, err := FailClosedClient(cause).Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected the fail-closed client to refuse the request")
	}
	if !errors.Is(err, cause) {
		t.Errorf("expected error to wrap the cause, got %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("request reached the network %d time(s); must not egress at all", n)
	}
}

// startListener runs handle for every accepted connection until the test ends.
func startListener(t *testing.T, handle func(net.Conn)) string {
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
				handle(c)
			}()
		}
	}()
	return ln.Addr().String()
}

func echo(c net.Conn) { _, _ = io.Copy(c, c) }

// assertEcho writes a payload through conn and expects it back.
func assertEcho(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	payload := []byte("tunnel-payload")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read through tunnel: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("tunnel payload mismatch: got %q", got)
	}
}

func TestDial_Direct(t *testing.T) {
	addr := startListener(t, echo)
	conn, err := Dial(context.Background(), nil, addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	assertEcho(t, conn)
}

func TestDial_HTTPConnect(t *testing.T) {
	type seen struct{ method, target, auth string }
	got := make(chan seen, 1)
	proxyAddr := startListener(t, func(c net.Conn) {
		br := bufio.NewReader(c)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		got <- seen{req.Method, req.Host, req.Header.Get("Proxy-Authorization")}
		_, _ = c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		_, _ = io.Copy(c, br) // echo, draining anything already buffered
	})

	proxy := &url.URL{Scheme: "http", Host: proxyAddr, User: url.UserPassword("alice", "s3cret")}
	conn, err := Dial(context.Background(), proxy, unresolvableTarget)
	if err != nil {
		t.Fatalf("Dial via HTTP proxy: %v", err)
	}
	defer conn.Close()
	assertEcho(t, conn)

	s := <-got
	if s.method != http.MethodConnect {
		t.Errorf("method = %q, want CONNECT", s.method)
	}
	if s.target != unresolvableTarget {
		t.Errorf("CONNECT target = %q, want %q", s.target, unresolvableTarget)
	}
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret")); s.auth != want {
		t.Errorf("Proxy-Authorization = %q, want %q", s.auth, want)
	}
}

func TestDial_HTTPConnectRefused(t *testing.T) {
	proxyAddr := startListener(t, func(c net.Conn) {
		if _, err := http.ReadRequest(bufio.NewReader(c)); err != nil {
			return
		}
		_, _ = c.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n"))
	})
	_, err := Dial(context.Background(), &url.URL{Scheme: "http", Host: proxyAddr}, unresolvableTarget)
	if err == nil || !strings.Contains(err.Error(), "407") {
		t.Fatalf("expected a 407 refusal error, got %v", err)
	}
}

// fakeSOCKS5 is a minimal RFC 1928/1929 server that requires alice:s3cret (or rejects all
// credentials when reject is set), records the requested target, then echoes.
func fakeSOCKS5(t *testing.T, reject bool, target chan<- string) string {
	return startListener(t, func(c net.Conn) {
		var greet [2]byte
		if _, err := io.ReadFull(c, greet[:]); err != nil {
			return
		}
		methods := make([]byte, greet[1])
		if _, err := io.ReadFull(c, methods); err != nil {
			return
		}
		if !bytes.Contains(methods, []byte{socksAuthPassword}) {
			_, _ = c.Write([]byte{socksVersion, socksNoAcceptable})
			return
		}
		_, _ = c.Write([]byte{socksVersion, socksAuthPassword})

		var hdr [2]byte
		if _, err := io.ReadFull(c, hdr[:]); err != nil {
			return
		}
		user := make([]byte, hdr[1])
		_, _ = io.ReadFull(c, user)
		var pl [1]byte
		_, _ = io.ReadFull(c, pl[:])
		pass := make([]byte, pl[0])
		_, _ = io.ReadFull(c, pass)
		if reject || string(user) != "alice" || string(pass) != "s3cret" {
			_, _ = c.Write([]byte{0x01, 0x01})
			return
		}
		_, _ = c.Write([]byte{0x01, 0x00})

		var req [4]byte
		if _, err := io.ReadFull(c, req[:]); err != nil {
			return
		}
		if req[3] != socksAtypDomain {
			target <- "non-domain address type (resolved locally)"
			return
		}
		var l [1]byte
		_, _ = io.ReadFull(c, l[:])
		host := make([]byte, l[0])
		_, _ = io.ReadFull(c, host)
		var port [2]byte
		_, _ = io.ReadFull(c, port[:])
		target <- net.JoinHostPort(string(host), strconv.Itoa(int(port[0])<<8|int(port[1])))

		_, _ = c.Write([]byte{socksVersion, 0x00, 0x00, socksAtypIPv4, 0, 0, 0, 0, 0, 0})
		echo(c)
	})
}

func TestDial_SOCKS5_RemoteResolutionWithAuth(t *testing.T) {
	for _, scheme := range []string{"socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			target := make(chan string, 1)
			addr := fakeSOCKS5(t, false, target)
			proxy := &url.URL{Scheme: scheme, Host: addr, User: url.UserPassword("alice", "s3cret")}
			conn, err := Dial(context.Background(), proxy, unresolvableTarget)
			if err != nil {
				t.Fatalf("Dial via SOCKS5: %v", err)
			}
			defer conn.Close()
			assertEcho(t, conn)
			if got := <-target; got != unresolvableTarget {
				t.Errorf("proxy received target %q, want %q (host name must be resolved by the proxy)", got, unresolvableTarget)
			}
		})
	}
}

func TestDial_SOCKS5_AuthRejected(t *testing.T) {
	addr := fakeSOCKS5(t, true, make(chan string, 1))
	proxy := &url.URL{Scheme: "socks5", Host: addr, User: url.UserPassword("alice", "wrong")}
	if _, err := Dial(context.Background(), proxy, unresolvableTarget); err == nil {
		t.Fatal("expected rejected SOCKS5 credentials to fail the dial")
	}
}

func TestDial_UnsupportedScheme(t *testing.T) {
	_, err := Dial(context.Background(), &url.URL{Scheme: "ftp", Host: "127.0.0.1:21"}, unresolvableTarget)
	if !errors.Is(err, ErrInvalidProxy) {
		t.Fatalf("expected ErrInvalidProxy, got %v", err)
	}
}
