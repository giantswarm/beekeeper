package sandbox

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
)

// GitHub's hosts the tests reach.
const (
	hostGitHub = "github.com"
	hostAPI    = "api.github.com"
)

// egressFixture is a proxy whose injected hosts all reach the echo server
// upstream, and whose log lines are collected.
type egressFixture struct {
	addr   string
	ca     *CA
	echo   *httptest.Server
	seen   chan http.Header
	mu     sync.Mutex
	lines  []string
	tunnel string // an allowed plain TCP echo listener, host:port
}

func newEgress(t *testing.T) *egressFixture {
	t.Helper()
	f := &egressFixture{seen: make(chan http.Header, 8)}
	// GitHub's stand-in echoes the request body, as POST /markdown does
	f.echo = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.seen <- r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		_, _ = fmt.Fprintf(w, "%s %s %s", r.Host, r.URL.Path, b) //nolint:gosec // the test's echo server
	}))
	t.Cleanup(f.echo.Close)
	echoAddr := f.echo.Listener.Addr().String()
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tl.Close() })
	go func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	f.tunnel = tl.Addr().String()
	ca, err := NewCA(config.GitHubHosts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f.ca = ca
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.addr = ln.Addr().String()
	e := &Egress{
		Allow:  []string{hostGitHub, "*.github.com", f.tunnel},
		Inject: config.GitHubHosts,
		Token:  func() string { return testToken },
		CA:     ca,
		Say: func(s string) {
			f.mu.Lock()
			f.lines = append(f.lines, s)
			f.mu.Unlock()
		},
		Upstream: &http.Transport{
			// the stand-in's own certificate, for example.com
			TLSClientConfig: &tls.Config{RootCAs: echoRoots(f.echo), ServerName: "example.com", MinVersion: tls.VersionTLS12},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, echoAddr)
			},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = e.Serve(ctx, ln) }()
	return f
}

func echoRoots(s *httptest.Server) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(s.Certificate())
	return pool
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// client is an HTTPS client through the proxy's HTTP side, trusting its
// CA alone.
func (f *egressFixture) client() *http.Client {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(f.ca.PEM())
	return &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(mustURL("http://" + f.addr)),
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}
}

func TestEgressInjectsTheTokenInHeadersOnly(t *testing.T) {
	f := newEgress(t)
	c := f.client()
	for host, want := range map[string]string{
		hostAPI:    "token " + testToken,
		hostGitHub: Authorization(hostGitHub, testToken),
	} {
		req, _ := http.NewRequest(http.MethodPost, "https://"+host+"/markdown", strings.NewReader("echo "+testToken[:4]+" placeholder"))
		req.Header.Set("Authorization", "token sandbox-placeholder")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		h := <-f.seen
		if got := h.Get("Authorization"); got != want {
			t.Errorf("%s: Authorization = %q, want the proxy's", host, got)
		}
		if strings.Contains(string(body), testToken) || string(body) != host+" /markdown echo "+testToken[:4]+" placeholder" {
			t.Errorf("%s: body = %q: want the request's body as sent", host, body)
		}
	}
	req, _ := http.NewRequest(http.MethodTrace, "https://api.github.com/", nil)
	if resp, err := c.Do(req); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("TRACE: %v %v", resp, err)
	}
}

func TestEgressRefusesHostsOffTheList(t *testing.T) {
	f := newEgress(t)
	for _, u := range []string{"https://example.com/", "http://example.com/", "https://github.com.example.com/"} {
		resp, err := f.client().Get(u)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s: %s", u, resp.Status)
			}
		} else if !strings.Contains(err.Error(), "Forbidden") {
			t.Errorf("%s: %v, want 403", u, err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.lines) == 0 || !strings.Contains(f.lines[0], "example.com") {
		t.Errorf("lines = %q", f.lines)
	}
}

// socks connects to host:port through the proxy's SOCKS5 side, with a
// username and password as Claude Code's clients may send.
func socks(t *testing.T, proxy, host string, port int) (net.Conn, byte) {
	t.Helper()
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(c)
	if _, err := c.Write([]byte{5, 1, socksUserPass}); err != nil {
		t.Fatal(err)
	}
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil || b[1] != socksUserPass {
		t.Fatalf("method %v %v", b, err)
	}
	_, _ = c.Write([]byte{1, 1, 'u', 1, 'p'})
	if _, err := io.ReadFull(r, b[:]); err != nil || b[1] != 0 {
		t.Fatalf("auth %v %v", b, err)
	}
	req := append([]byte{5, socksConnect, 0, socksDomain, byte(len(host))}, host...) //nolint:gosec // a test host name, short
	req = binary.BigEndian.AppendUint16(req, uint16(port))                           //nolint:gosec // a test port
	_, _ = c.Write(req)
	var rep [10]byte
	if _, err := io.ReadFull(r, rep[:]); err != nil {
		t.Fatal(err)
	}
	return &bufConn{Conn: c, r: r}, rep[1]
}

func TestEgressSOCKS(t *testing.T) {
	f := newEgress(t)
	host, p, _ := net.SplitHostPort(f.tunnel)
	port, _ := strconv.Atoi(p)
	c, code := socks(t, f.addr, host, port)
	if code != socksOK {
		t.Fatalf("listed %s: reply %d", f.tunnel, code)
	}
	_, _ = c.Write([]byte("ping"))
	got := make([]byte, 4)
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "ping" {
		t.Errorf("tunnel: %q %v", got, err)
	}
	_ = c.Close()
	// loopback only on the listed port
	if c, code := socks(t, f.addr, host, port+1); code != socksNotAllow {
		t.Errorf("unlisted loopback port: reply %d", code)
		_ = c.Close()
	}
	if c, code := socks(t, f.addr, "example.com", 443); code != socksNotAllow {
		t.Errorf("example.com: reply %d", code)
		_ = c.Close()
	}
	// TLS through SOCKS to an injected host is terminated as well
	c, code = socks(t, f.addr, "api.github.com", 443)
	if code != socksOK {
		t.Fatalf("api.github.com: reply %d", code)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(f.ca.PEM())
	tc := tls.Client(c, &tls.Config{ServerName: "api.github.com", RootCAs: pool, MinVersion: tls.VersionTLS12})
	_, _ = io.WriteString(tc, "GET /user HTTP/1.1\r\nHost: api.github.com\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /user: %v %v", resp, err)
	}
	if h := <-f.seen; h.Get("Authorization") != "token "+testToken {
		t.Errorf("Authorization = %q", h.Get("Authorization"))
	}
}

func TestAllowed(t *testing.T) {
	e := &Egress{Allow: []string{"github.com", "*.github.com", "proxy.golang.org", "127.0.0.1:6443", "[::1]:7000"}}
	for target, want := range map[string]bool{
		"github.com:443":         true,
		"GitHub.com.:443":        true,
		"api.github.com:443":     true,
		"a.b.github.com:22":      true,
		"evilgithub.com:443":     false,
		"github.com.evil.io:443": false,
		"proxy.golang.org:443":   true,
		"sum.golang.org:443":     false,
		"127.0.0.1:6443":         true,
		"127.0.0.1:6444":         false,
		"[::1]:7000":             true,
		"localhost:6443":         false,
	} {
		h, p, _ := net.SplitHostPort(target)
		if got := e.Allowed(h, p); got != want {
			t.Errorf("Allowed(%s) = %v, want %v", target, got, want)
		}
	}
}

func TestByName(t *testing.T) {
	for ip, want := range map[string]bool{
		"140.82.121.4": true, "10.1.2.3": true, "127.0.0.1": false, "::1": false, "0.0.0.0": false,
		"169.254.169.254": false, "fe80::1": false, "224.0.0.1": false,
	} {
		if got := byName(net.ParseIP(ip)); got != want {
			t.Errorf("byName(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestCAIsNameConstrained(t *testing.T) {
	ca, err := NewCA(config.GitHubHosts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.PEM())
	for host, want := range map[string]bool{"api.github.com": true, "github.com": true, "example.com": false} {
		l, err := ca.Leaf(host)
		if err != nil {
			t.Fatal(err)
		}
		cert, _ := x509.ParseCertificate(l.Certificate[0])
		_, err = cert.Verify(x509.VerifyOptions{DNSName: host, Roots: pool})
		if (err == nil) != want {
			t.Errorf("%s verifies: %v, want %v", host, err, want)
		}
	}
}
