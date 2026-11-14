package pgtest

import (
	"io"
	"net"
	"sync"
)

// proxy forwards TCP connections to Postgres and can cut them: while down it closes every forwarded
// connection and every new one at once, so clients see Postgres as unreachable without waiting on a
// timeout. It keeps listening while down, so its port can't be taken by anyone else.
type proxy struct {
	ln     net.Listener
	target string

	mu     sync.Mutex
	down   bool
	closed bool
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
}

func newProxy(target string) (*proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &proxy{ln: ln, target: target, conns: map[net.Conn]struct{}{}}
	p.wg.Add(1)
	go p.serve()
	return p, nil
}

func (p *proxy) addr() string { return p.ln.Addr().String() }

func (p *proxy) serve() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		if !p.track(c) {
			_ = c.Close() // refused: nothing was sent on it
			continue
		}
		p.wg.Add(1)
		go p.forward(c)
	}
}

// track registers c; it reports false when the proxy is down or closed.
func (p *proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down || p.closed {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *proxy) untrack(cs ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range cs {
		delete(p.conns, c)
		_ = c.Close() // the copy that used it is over
	}
}

func (p *proxy) forward(client net.Conn) {
	defer p.wg.Done()
	server, err := net.Dial("tcp", p.target)
	if err != nil {
		p.untrack(client)
		return
	}
	if !p.track(server) {
		_ = server.Close() // refused: nothing was sent on it
		p.untrack(client)
		return
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(server, client)
		p.untrack(client, server)
		close(done)
	}()
	_, _ = io.Copy(client, server)
	p.untrack(client, server)
	<-done
}

// setDown cuts (true) or restores (false) the connection to Postgres.
func (p *proxy) setDown(down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = down
	if down {
		for c := range p.conns {
			_ = c.Close() // cutting it is the point; how it ends doesn't matter
			delete(p.conns, c)
		}
	}
}

func (p *proxy) isDown() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.down
}

func (p *proxy) close() {
	p.mu.Lock()
	p.closed = true
	for c := range p.conns {
		_ = c.Close() // shutting down: how it ends doesn't matter
		delete(p.conns, c)
	}
	p.mu.Unlock()
	_ = p.ln.Close() // shutting down: it only stops serve's Accept
	p.wg.Wait()
}
