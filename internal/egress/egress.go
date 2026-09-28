// Package egress centralises how the switcher reaches the internet on behalf of an account:
// proxy URL validation, fail-closed HTTP clients and raw TCP dialing for CONNECT tunnels.
//
// Every path fails closed. A proxy that cannot be honoured is an error, never a silent fallback
// to a direct connection from the operator's real IP, which would link the account to that IP.
package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidProxy marks a proxy URL that cannot be used. Callers must refuse the egress rather
// than connect directly.
var ErrInvalidProxy = errors.New("invalid proxy URL")

// handshakeTimeout bounds the proxy handshake (CONNECT or SOCKS5) after the TCP dial succeeds.
const handshakeTimeout = 15 * time.Second

var dialer = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}

// ValidateProxyURL reports whether raw is a proxy URL the outbound transports can honour.
// An empty value is allowed and means "no proxy".
func ValidateProxyURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	_, err := ParseProxyURL(raw)
	return err
}

// ParseProxyURL parses and validates a non-empty proxy URL. Accepted schemes are http, https,
// socks5 and socks5h, and a host is required. Errors wrap ErrInvalidProxy.
func ParseProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%w: empty", ErrInvalidProxy)
	}
	// Error messages never echo raw or url.Parse's error text: both can contain the password, and
	// these errors reach logs and the dashboard. Without "://" the would-be scheme may even be the
	// username (user:pass@host), so that case is reported generically too.
	if !strings.Contains(raw, "://") {
		return nil, fmt.Errorf("%w: missing scheme (use http://, https://, socks5:// or socks5h://)%s", ErrInvalidProxy, formatHint(raw))
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: not a valid URL", ErrInvalidProxy)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("%w: unsupported scheme %q (use http, https, socks5 or socks5h, e.g. http://user:pass@host:port)%s",
			ErrInvalidProxy, u.Scheme, formatHint(raw))
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("%w: missing host:port", ErrInvalidProxy)
	}
	if p := u.Port(); p != "" {
		if n, pErr := strconv.Atoi(p); pErr != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%w: invalid port %q", ErrInvalidProxy, p)
		}
	}
	return u, nil
}

// ParseProxyLine parses one line of a bulk proxy list and returns the proxy in URL form. It accepts
// a proxy URL (scheme://[user:pass@]host:port), the provider-list form host:port:user:pass (the
// password may itself contain ':'), and a bare host:port; the last two default to http. Errors
// wrap ErrInvalidProxy and never contain the line's credentials.
func ParseProxyLine(line string) (*url.URL, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, fmt.Errorf("%w: empty", ErrInvalidProxy)
	}
	if strings.Contains(line, "://") {
		return ParseProxyURL(line)
	}
	parts := strings.Split(line, ":")
	var u *url.URL
	switch {
	case len(parts) == 2:
		u = &url.URL{Scheme: "http", Host: net.JoinHostPort(parts[0], parts[1])}
	case len(parts) >= 4:
		user, pass := parts[2], strings.Join(parts[3:], ":")
		if user == "" || pass == "" {
			return nil, fmt.Errorf("%w: empty username or password in host:port:user:pass", ErrInvalidProxy)
		}
		u = &url.URL{Scheme: "http", Host: net.JoinHostPort(parts[0], parts[1]), User: url.UserPassword(user, pass)}
	default:
		return nil, fmt.Errorf("%w: expected scheme://user:pass@host:port, host:port:user:pass or host:port", ErrInvalidProxy)
	}
	// Round-trip through ParseProxyURL so host and port get the same checks as a typed URL.
	return ParseProxyURL(u.String())
}

// CheckResult is the outcome of routing one request through a proxy.
type CheckResult struct {
	ExitIP  string        // the address the echo service saw, i.e. the proxy's exit IP
	Latency time.Duration // time to the echo service's answer
}

// Check sends one GET to echoURL through proxy and reports the proxy's exit IP. echoURL must answer
// with the caller's IP as plain text, like https://api.ipify.org. It uses the same dialer as the
// CONNECT tunnels, so host names are resolved by the proxy. Errors never contain the proxy's
// credentials.
func Check(ctx context.Context, proxy *url.URL, echoURL string) (CheckResult, error) {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return Dial(ctx, proxy, addr)
		},
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: handshakeTimeout,
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, echoURL, nil)
	if err != nil {
		return CheckResult{}, fmt.Errorf("build check request: %w", err)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		// Drop the `Get "<echo URL>":` wrapper; the cause is what the user needs to see.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return CheckResult{}, scrubCredentials(err, proxy)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	latency := time.Since(start)
	if err != nil {
		return CheckResult{}, scrubCredentials(fmt.Errorf("read echo response: %w", err), proxy)
	}
	if resp.StatusCode != http.StatusOK {
		return CheckResult{}, fmt.Errorf("echo service answered %s", resp.Status)
	}
	ip := strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		return CheckResult{}, errors.New("echo service did not answer with an IP address")
	}
	return CheckResult{ExitIP: ip, Latency: latency}, nil
}

// scrubCredentials replaces the proxy's username and password wherever they appear in err's text,
// as a last line of defence for messages produced by the network stack.
func scrubCredentials(err error, proxy *url.URL) error {
	if err == nil || proxy == nil || proxy.User == nil {
		return err
	}
	msg := err.Error()
	if pass, ok := proxy.User.Password(); ok && pass != "" {
		msg = strings.ReplaceAll(msg, pass, "***")
	}
	if user := proxy.User.Username(); user != "" {
		msg = strings.ReplaceAll(msg, user, "***")
	}
	return errors.New(msg)
}

// formatHint recognises the provider-list format host:port:user:pass (as exported by Webshare and
// similar providers) and suggests the URL form, since pasting it verbatim is the most likely mistake.
func formatHint(raw string) string {
	if strings.Contains(raw, "://") {
		return ""
	}
	if parts := strings.Split(raw, ":"); len(parts) == 4 {
		// Credentials are deliberately not echoed: this message can reach logs and the dashboard.
		return fmt.Sprintf(" (looks like host:port:user:pass; use http://<user>:<pass>@%s:%s)", parts[0], parts[1])
	}
	return ""
}

// MaskProxyURL renders a proxy URL for display without credentials: scheme://***@host:port, or
// scheme://host:port when it has none. An empty raw yields "" with ok true. ok is false when raw is
// not a usable proxy URL; nothing of it is returned then, because credentials may sit in any
// position of an unparseable value (e.g. host:port:user:pass).
func MaskProxyURL(raw string) (masked string, ok bool) {
	if strings.TrimSpace(raw) == "" {
		return "", true
	}
	u, err := ParseProxyURL(raw)
	if err != nil {
		return "", false
	}
	if u.User != nil {
		return u.Scheme + "://***@" + u.Host, true
	}
	return u.Scheme + "://" + u.Host, true
}

// FailClosedClient returns an *http.Client that refuses every request with cause. It stands in for
// an account whose proxy is unusable, so its traffic errors out instead of leaving directly.
func FailClosedClient(cause error) *http.Client {
	return &http.Client{Transport: failClosedTransport{cause: cause}}
}

type failClosedTransport struct{ cause error }

func (t failClosedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		_ = r.Body.Close()
	}
	return nil, fmt.Errorf("egress blocked: %w", t.cause)
}

// IsLoopbackHost reports whether hostport (host or host:port) names the local machine. Loopback
// traffic never leaves the host, so it is exempt from per-account proxying.
func IsLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Dial opens a TCP connection to addr (host:port). A nil proxy dials directly; otherwise the
// connection is tunnelled through the proxy (HTTP CONNECT for http/https, SOCKS5 for socks5 and
// socks5h). Host names are always resolved by the proxy, never locally, so DNS does not leak
// either. ctx bounds the TCP dial; the proxy handshake has its own deadline.
func Dial(ctx context.Context, proxy *url.URL, addr string) (net.Conn, error) {
	if proxy == nil {
		return dialer.DialContext(ctx, "tcp", addr)
	}
	switch proxy.Scheme {
	case "http", "https":
		return dialHTTPConnect(ctx, proxy, addr)
	case "socks5", "socks5h":
		return dialSOCKS5(ctx, proxy, addr)
	default:
		return nil, fmt.Errorf("%w: unsupported scheme %q", ErrInvalidProxy, proxy.Scheme)
	}
}

// proxyAddr returns the proxy's host:port, filling in the scheme's default port.
func proxyAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// handshakeDeadline is the earlier of ctx's deadline and now+handshakeTimeout.
func handshakeDeadline(ctx context.Context) time.Time {
	d := time.Now().Add(handshakeTimeout)
	if cd, ok := ctx.Deadline(); ok && cd.Before(d) {
		return cd
	}
	return d
}

func dialHTTPConnect(ctx context.Context, u *url.URL, addr string) (net.Conn, error) {
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddr(u))
	if err != nil {
		return nil, fmt.Errorf("dial proxy %s: %w", u.Host, err)
	}
	_ = conn.SetDeadline(handshakeDeadline(ctx))

	if u.Scheme == "https" {
		tc := tls.Client(conn, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("TLS handshake with proxy %s: %w", u.Host, err)
		}
		conn = tc
	}

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if u.User != nil {
		pass, _ := u.User.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + pass))
		req.Header.Set("Proxy-Authorization", "Basic "+cred)
	}
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("send CONNECT to proxy %s: %w", u.Host, err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read CONNECT response from proxy %s: %w", u.Host, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy %s refused CONNECT to %s: %s", u.Host, addr, resp.Status)
	}

	_ = conn.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		// The proxy sent tunnel bytes right after its response; keep them.
		return &bufferedConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

// bufferedConn serves bytes already read into r before reading from the underlying conn.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

const (
	socksVersion      = 0x05
	socksAuthNone     = 0x00
	socksAuthPassword = 0x02
	socksNoAcceptable = 0xFF
	socksCmdConnect   = 0x01
	socksAtypIPv4     = 0x01
	socksAtypDomain   = 0x03
	socksAtypIPv6     = 0x04
)

// dialSOCKS5 performs an RFC 1928 CONNECT, with RFC 1929 username/password auth when the proxy URL
// carries credentials. Domain names are sent to the proxy as-is (remote resolution) for both
// socks5 and socks5h.
func dialSOCKS5(ctx context.Context, u *url.URL, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid target %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid target port %q", portStr)
	}

	conn, err := dialer.DialContext(ctx, "tcp", proxyAddr(u))
	if err != nil {
		return nil, fmt.Errorf("dial proxy %s: %w", u.Host, err)
	}
	_ = conn.SetDeadline(handshakeDeadline(ctx))

	if err := socks5Handshake(conn, u, host, port); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("SOCKS5 proxy %s: %w", u.Host, err)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func socks5Handshake(conn net.Conn, u *url.URL, host string, port int) error {
	methods := []byte{socksAuthNone}
	if u.User != nil {
		methods = append(methods, socksAuthPassword)
	}
	if _, err := conn.Write(append([]byte{socksVersion, byte(len(methods))}, methods...)); err != nil {
		return fmt.Errorf("send greeting: %w", err)
	}
	var sel [2]byte
	if _, err := io.ReadFull(conn, sel[:]); err != nil {
		return fmt.Errorf("read method selection: %w", err)
	}
	if sel[0] != socksVersion {
		return fmt.Errorf("unexpected version %d", sel[0])
	}
	switch sel[1] {
	case socksAuthNone:
	case socksAuthPassword:
		if u.User == nil {
			return errors.New("proxy requires credentials")
		}
		user := u.User.Username()
		pass, _ := u.User.Password()
		if len(user) > 255 || len(pass) > 255 {
			return errors.New("username or password longer than 255 bytes")
		}
		msg := []byte{0x01, byte(len(user))}
		msg = append(msg, user...)
		msg = append(msg, byte(len(pass)))
		msg = append(msg, pass...)
		if _, err := conn.Write(msg); err != nil {
			return fmt.Errorf("send credentials: %w", err)
		}
		var st [2]byte
		if _, err := io.ReadFull(conn, st[:]); err != nil {
			return fmt.Errorf("read auth status: %w", err)
		}
		if st[1] != 0x00 {
			return errors.New("authentication rejected")
		}
	case socksNoAcceptable:
		return errors.New("no acceptable authentication method")
	default:
		return fmt.Errorf("unsupported authentication method %d", sel[1])
	}

	req := []byte{socksVersion, socksCmdConnect, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, socksAtypIPv4)
			req = append(req, ip4...)
		} else {
			req = append(req, socksAtypIPv6)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return errors.New("target host name longer than 255 bytes")
		}
		req = append(req, socksAtypDomain, byte(len(host)))
		req = append(req, host...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("send connect request: %w", err)
	}

	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return fmt.Errorf("read connect reply: %w", err)
	}
	if head[1] != 0x00 {
		return fmt.Errorf("connect to %s:%d failed (reply code %d)", host, port, head[1])
	}
	var skip int
	switch head[3] {
	case socksAtypIPv4:
		skip = 4
	case socksAtypIPv6:
		skip = 16
	case socksAtypDomain:
		var l [1]byte
		if _, err := io.ReadFull(conn, l[:]); err != nil {
			return fmt.Errorf("read bound address: %w", err)
		}
		skip = int(l[0])
	default:
		return fmt.Errorf("unexpected address type %d in reply", head[3])
	}
	if _, err := io.ReadFull(conn, make([]byte, skip+2)); err != nil {
		return fmt.Errorf("read bound address: %w", err)
	}
	return nil
}
