package sandbox

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
)

// The egress proxy is the agent sandbox's only way out. The policy points
// Claude Code's httpProxyPort and socksProxyPort at the same port, so the
// sandbox's HTTP and SOCKS5 proxies both end here, and the proxy tells
// them apart by their first byte. It reaches only the allow list's hosts,
// tunnels them as they are, and terminates TLS for GitHub's hosts to set
// the GitHub token's Authorization header itself: the sandbox never holds
// the token, and no request body is read or rewritten.

// egressDialTimeout bounds a connection to a target.
const egressDialTimeout = 30 * time.Second

// Egress is the sandbox's egress proxy.
type Egress struct {
	// Allow are the hosts commands reach (the policy's Domains).
	Allow []string
	// Inject are the hosts whose requests get the token on port 443.
	Inject []string
	// Token is the GitHub token now, "" while there is none.
	Token func() string
	// CA issues the injected hosts' certificates.
	CA *CA
	// Peer reports whether a connection comes from a process the proxy
	// serves; nil serves every one.
	Peer func(net.Conn) error
	// Say reports a refused or failed connection, never a header.
	Say func(string)
	// Upstream sends the injected hosts' requests; nil is the default
	// transport, its connections held to the allow list's address rules.
	Upstream http.RoundTripper

	once  sync.Once
	mitm  *mitmListener
	plain http.RoundTripper
}

// Serve answers ln's connections until ctx ends.
func (e *Egress) Serve(ctx context.Context, ln net.Listener) error {
	e.init()
	srv := &http.Server{
		Handler:           e.injector(),
		ReadHeaderTimeout: time.Minute,
		IdleTimeout:       5 * time.Minute,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if t, ok := c.(*tls.Conn); ok {
				if m, ok := t.NetConn().(*mitmConn); ok {
					return context.WithValue(ctx, targetKey{}, m.host)
				}
			}
			return ctx
		},
	}
	go func() { _ = srv.Serve(e.mitm) }()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		_ = srv.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go e.handle(ctx, c)
	}
}

func (e *Egress) init() {
	e.once.Do(func() {
		e.mitm = &mitmListener{ch: make(chan net.Conn), done: make(chan struct{}), ca: e.CA.Leaf}
		e.plain = &http.Transport{DialContext: e.dial, Proxy: nil, ForceAttemptHTTP2: false, MaxIdleConnsPerHost: 4, IdleConnTimeout: time.Minute}
		if e.Upstream == nil {
			e.Upstream = &http.Transport{DialContext: e.dial, Proxy: nil, ForceAttemptHTTP2: true, MaxIdleConnsPerHost: 8, IdleConnTimeout: time.Minute,
				TLSHandshakeTimeout: egressDialTimeout}
		}
	})
}

func (e *Egress) say(format string, a ...any) {
	if e.Say != nil {
		e.Say(fmt.Sprintf(format, a...))
	}
}

// handle serves one connection from the sandbox.
func (e *Egress) handle(ctx context.Context, c net.Conn) {
	if e.Peer != nil {
		if err := e.Peer(c); err != nil {
			e.say("egress: refused a connection from %s: %v", c.RemoteAddr(), err)
			_ = c.Close()
			return
		}
	}
	br := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(time.Minute))
	first, err := br.Peek(1)
	if err != nil {
		_ = c.Close()
		return
	}
	conn := &bufConn{Conn: c, r: br}
	if first[0] == socksVersion {
		e.socks(ctx, conn)
		return
	}
	e.http(ctx, conn)
}

// http serves an HTTP proxy connection: CONNECT tunnels, absolute-form
// requests forwarded as plain HTTP, never with the token.
func (e *Egress) http(ctx context.Context, c *bufConn) {
	for {
		req, err := http.ReadRequest(c.r)
		if err != nil {
			_ = c.Close()
			return
		}
		_ = c.SetReadDeadline(time.Time{})
		if req.Method == http.MethodConnect {
			host, port, err := net.SplitHostPort(req.Host)
			if err != nil {
				reply(c, http.StatusBadRequest, "CONNECT wants host:port")
				_ = c.Close()
				return
			}
			if !e.Allowed(host, port) {
				e.say("egress: refused %s (not on the sandbox's allow list)", net.JoinHostPort(host, port))
				reply(c, http.StatusForbidden, net.JoinHostPort(host, port)+" is not on the sandbox's allow list")
				_ = c.Close()
				return
			}
			if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
				_ = c.Close()
				return
			}
			// target owns the connection from here
			e.target(ctx, c, host, port)
			return
		}
		if !e.forward(c, req) {
			_ = c.Close()
			return
		}
	}
}

// forward sends one absolute-form request as plain HTTP and writes its
// response back, reporting whether the connection serves another.
func (e *Egress) forward(c *bufConn, req *http.Request) bool {
	if req.URL.Scheme != "http" || req.URL.Host == "" {
		reply(c, http.StatusBadRequest, "the sandbox's proxy forwards http:// requests and tunnels the rest (CONNECT)")
		return false
	}
	host, port := req.URL.Hostname(), req.URL.Port()
	if port == "" {
		port = "80"
	}
	if !e.Allowed(host, port) {
		e.say("egress: refused %s (not on the sandbox's allow list)", net.JoinHostPort(host, port))
		reply(c, http.StatusForbidden, net.JoinHostPort(host, port)+" is not on the sandbox's allow list")
		return false
	}
	req.RequestURI = ""
	for _, h := range []string{"Proxy-Authorization", "Proxy-Connection"} {
		req.Header.Del(h)
	}
	resp, err := e.plain.RoundTrip(req)
	if err != nil {
		reply(c, http.StatusBadGateway, err.Error())
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if err := resp.Write(c); err != nil {
		return false
	}
	return !req.Close && !resp.Close
}

// reply answers an HTTP proxy request with code and text, closing.
func reply(w io.Writer, code int, text string) {
	_, _ = fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s\n",
		code, http.StatusText(code), len(text)+1, text)
}

// target serves an allowed tunnel to host:port: TLS terminated for an
// injected host on 443, an opaque tunnel otherwise.
func (e *Egress) target(ctx context.Context, c *bufConn, host, port string) {
	host = canonical(host)
	if port == "443" && slices.Contains(e.Inject, host) {
		_ = c.SetDeadline(time.Time{})
		select {
		case e.mitm.ch <- &mitmConn{bufConn: c, host: host}:
		case <-ctx.Done():
			_ = c.Close()
		}
		return
	}
	defer func() { _ = c.Close() }()
	up, err := e.dial(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		e.say("egress: %s: %v", net.JoinHostPort(host, port), err)
		return
	}
	_ = c.SetDeadline(time.Time{})
	splice(c, up)
}

// splice copies between a and b until both directions end.
func splice(a, b net.Conn) {
	defer func() { _ = b.Close() }()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(b, a)
		closeWrite(b)
		close(done)
	}()
	_, _ = io.Copy(a, b)
	closeWrite(a)
	<-done
}

func closeWrite(c net.Conn) {
	type cw interface{ CloseWrite() error }
	if b, ok := c.(*bufConn); ok {
		c = b.Conn
	}
	if w, ok := c.(cw); ok {
		_ = w.CloseWrite()
		return
	}
	_ = c.Close()
}

// Allowed reports whether host:port is on the allow list: an entry is a
// host, a host:port, or *.domain for the domain's subdomains.
func (e *Egress) Allowed(host, port string) bool {
	host = canonical(host)
	if host == "" {
		return false
	}
	for _, a := range e.Allow {
		ah, ap, err := net.SplitHostPort(a)
		if err != nil {
			ah, ap = a, ""
		}
		ah = canonical(ah)
		if ap != "" && ap != port {
			continue
		}
		if d, ok := strings.CutPrefix(ah, "*."); ok {
			if strings.HasSuffix(host, "."+d) {
				return true
			}
			continue
		}
		if ah == host {
			return true
		}
	}
	return false
}

// canonical is host as the allow list compares it: lower case, no trailing
// dot, an IPv6 literal without brackets.
func canonical(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
}

// dial connects to an allowed target. A name never reaches a loopback,
// link-local, unspecified or multicast address, so no name on the list
// rebinds onto the host's own listeners; an address is dialled only as
// listed (127.0.0.1:<port> for a lab's API server), a loopback name only
// with its port listed.
func (e *Egress) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: egressDialTimeout, KeepAlive: 30 * time.Second}
	if net.ParseIP(canonical(host)) == nil && !config.Loopback(host) {
		d.Control = func(_, address string, _ syscall.RawConn) error {
			h, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if !byName(net.ParseIP(h)) {
				return fmt.Errorf("%s resolves to %s, which the sandbox never reaches by name", host, h)
			}
			return nil
		}
	}
	return d.DialContext(ctx, network, addr)
}

// byName reports whether a name may resolve to ip: no loopback,
// link-local, unspecified or multicast address.
func byName(ip net.IP) bool {
	return ip != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsUnspecified() && !ip.IsMulticast()
}

// injector is the handler of the TLS-terminated requests: each goes to the
// host its tunnel named, with the token's Authorization header in place of
// whatever the request carried, its body untouched.
func (e *Egress) injector() http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			host, _ := pr.In.Context().Value(targetKey{}).(string)
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = "https", host, host
			pr.Out.Header.Del("Authorization")
			if t := e.Token(); t != "" {
				pr.Out.Header.Set("Authorization", Authorization(host, t))
			}
		},
		Transport:     e.Upstream,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			e.say("egress: %s: %v", r.Host, err)
			http.Error(w, "the sandbox's proxy reached no "+r.Host, http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _ := r.Context().Value(targetKey{}).(string)
		if host == "" || r.Method == http.MethodTrace || r.Method == http.MethodConnect {
			http.Error(w, "not through the sandbox's proxy", http.StatusMethodNotAllowed)
			return
		}
		rp.ServeHTTP(w, r)
	})
}

// Authorization is the header value the token goes to host with: git's
// endpoint on github.com takes Basic only, the API a token.
func Authorization(host, token string) string {
	if host == "github.com" {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	}
	return "token " + token
}

type targetKey struct{}

// mitmConn is a tunnel to an injected host, handed to the TLS listener.
type mitmConn struct {
	*bufConn
	host string
}

// mitmListener hands the injected hosts' tunnels to the HTTP server,
// TLS-terminated with the CA's certificate for the tunnel's host.
type mitmListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
	ca   func(string) (*tls.Certificate, error)
}

func (l *mitmListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		m, _ := c.(*mitmConn)
		return tls.Server(m, &tls.Config{
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"http/1.1"},
			// the tunnel's host, whatever name the client sends
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return l.ca(m.host) },
		}), nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *mitmListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *mitmListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

// bufConn is a connection whose first bytes were read into r.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// SOCKS5 (RFC 1928): no authentication or a username and password
// (RFC 1929), which the sandbox's clients may send and the proxy ignores;
// CONNECT only.
const (
	socksVersion   = 5
	socksNoAuth    = 0
	socksUserPass  = 2
	socksNoMethod  = 0xff
	socksConnect   = 1
	socksIPv4      = 1
	socksDomain    = 3
	socksIPv6      = 4
	socksOK        = 0
	socksNotAllow  = 2
	socksNoCommand = 7
	socksNoAddress = 8
)

func (e *Egress) socks(ctx context.Context, c *bufConn) {
	host, port, err := socksHandshake(c)
	if err != nil {
		_ = c.Close()
		return
	}
	if !e.Allowed(host, port) {
		e.say("egress: refused %s (not on the sandbox's allow list)", net.JoinHostPort(host, port))
		_ = socksReply(c, socksNotAllow)
		_ = c.Close()
		return
	}
	if err := socksReply(c, socksOK); err != nil {
		_ = c.Close()
		return
	}
	e.target(ctx, c, host, port)
}

// socksHandshake reads a client's greeting and request, answering what it
// refuses, and returns the target.
func socksHandshake(c *bufConn) (string, string, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return "", "", err
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(c.r, methods); err != nil {
		return "", "", err
	}
	switch {
	case slices.Contains(methods, socksNoAuth):
		if _, err := c.Write([]byte{socksVersion, socksNoAuth}); err != nil {
			return "", "", err
		}
	case slices.Contains(methods, socksUserPass):
		if _, err := c.Write([]byte{socksVersion, socksUserPass}); err != nil {
			return "", "", err
		}
		if err := socksSkipUserPass(c); err != nil {
			return "", "", err
		}
	default:
		_, _ = c.Write([]byte{socksVersion, socksNoMethod})
		return "", "", errors.New("socks: no acceptable method")
	}
	var req [4]byte
	if _, err := io.ReadFull(c.r, req[:]); err != nil {
		return "", "", err
	}
	if req[0] != socksVersion || req[1] != socksConnect {
		_ = socksReply(c, socksNoCommand)
		return "", "", errors.New("socks: CONNECT only")
	}
	var host string
	switch req[3] {
	case socksIPv4, socksIPv6:
		ip := make([]byte, map[byte]int{socksIPv4: 4, socksIPv6: 16}[req[3]])
		if _, err := io.ReadFull(c.r, ip); err != nil {
			return "", "", err
		}
		host = net.IP(ip).String()
	case socksDomain:
		n, err := c.r.ReadByte()
		if err != nil {
			return "", "", err
		}
		name := make([]byte, n)
		if _, err := io.ReadFull(c.r, name); err != nil {
			return "", "", err
		}
		host = string(name)
	default:
		_ = socksReply(c, socksNoAddress)
		return "", "", errors.New("socks: unknown address type")
	}
	var port [2]byte
	if _, err := io.ReadFull(c.r, port[:]); err != nil {
		return "", "", err
	}
	return host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:]))), nil
}

// socksSkipUserPass reads a username and password sub-negotiation and
// accepts it: the proxy authenticates by the connection's peer.
func socksSkipUserPass(c *bufConn) error {
	ver, err := c.r.ReadByte()
	if err != nil || ver != 1 {
		return errors.New("socks: bad username/password request")
	}
	for range 2 {
		n, err := c.r.ReadByte()
		if err != nil {
			return err
		}
		if _, err := c.r.Discard(int(n)); err != nil {
			return err
		}
	}
	_, err = c.Write([]byte{1, 0})
	return err
}

func socksReply(c net.Conn, code byte) error {
	_, err := c.Write([]byte{socksVersion, code, 0, socksIPv4, 0, 0, 0, 0, 0, 0})
	return err
}
