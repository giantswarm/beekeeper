//go:build scenario

package clock

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ackTimeout bounds how long an advance waits for the processes to
// acknowledge it.
var ackTimeout = 10 * time.Second

// Server is a scenario world's clock: the system clock moved by an offset,
// served on a Unix socket to every process of the scenario (Env names it)
// and moved forward by Advance.
type Server struct {
	offset atomic.Int64
	// armed is when a process last noted that it armed a timer, ticker or
	// sleep, in the system's Unix nanoseconds.
	armed atomic.Int64

	// mu orders advances and connections: a process connecting during an
	// advance gets the offset after it.
	mu    sync.Mutex
	seq   int64
	conns map[*conn]struct{}

	ln net.Listener
	wg sync.WaitGroup
}

// conn is one process's connection.
type conn struct {
	c     net.Conn
	enc   *json.Encoder
	hello hello
	acks  chan ack
	gone  chan struct{}
}

// NewServer is a clock at start, or at the system's time for a zero start.
func NewServer(start time.Time) *Server {
	s := &Server{conns: map[*conn]struct{}{}}
	if !start.IsZero() {
		s.offset.Store(int64(time.Until(start)))
	}
	return s
}

// Now is the world's time.
func (s *Server) Now() time.Time {
	return time.Now().Add(time.Duration(s.offset.Load()))
}

// Serve listens on the Unix socket at path until Close.
func (s *Server) Serve(path string) error {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	s.ln = ln
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.serve(c)
			}()
		}
	}()
	return nil
}

func (s *Server) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	dec := json.NewDecoder(c)
	_ = c.SetDeadline(time.Now().Add(dialTimeout))
	var h hello
	if err := dec.Decode(&h); err != nil {
		return
	}
	p := &conn{c: c, enc: json.NewEncoder(c), hello: h, acks: make(chan ack, 16), gone: make(chan struct{})}
	s.mu.Lock()
	err := p.enc.Encode(offset{Seq: s.seq, Offset: s.offset.Load()})
	if err == nil {
		s.conns[p] = struct{}{}
	}
	s.mu.Unlock()
	if err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	defer func() {
		// Say the process is gone before taking the lock: an advance
		// waiting for its acknowledgement holds the lock until it has
		// one, the process's end or ackTimeout.
		close(p.gone)
		s.mu.Lock()
		delete(s.conns, p)
		s.mu.Unlock()
	}()
	for {
		var a ack
		if err := dec.Decode(&a); err != nil {
			return
		}
		if a.Armed {
			s.armed.Store(time.Now().UnixNano())
			continue
		}
		select {
		case p.acks <- a:
		default:
		}
	}
}

// LastArmed is when a process last armed a timer, ticker or sleep on the
// world's clock, in the system's time; zero before the first. A desk whose
// processes arm nothing and call nothing for a while is quiet: the work an
// advance woke is done and waits on the clock again.
func (s *Server) LastArmed() time.Time {
	n := s.armed.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// Advanced is what an advance did.
type Advanced struct {
	// Now is the world's time after it.
	Now time.Time `json:"now"`
	// Processes acknowledged it; Fired timers, tickers and sleeps were due
	// in them and fired.
	Processes int `json:"processes"`
	Fired     int `json:"fired"`
}

// Advance moves the world's time forward by d and returns once every
// connected process has fired what was then due and acknowledged it. A
// process that ends meanwhile counts if it acknowledged before it ended and
// is skipped at once otherwise; one that does not answer within ackTimeout
// is an error naming it.
func (s *Server) Advance(d time.Duration) (Advanced, error) {
	if d < 0 {
		return Advanced{}, fmt.Errorf("clock: the world's time does not go back (advance %s)", d)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	o := offset{Seq: s.seq, Offset: s.offset.Add(int64(d))}
	res := Advanced{Now: time.Now().Add(time.Duration(o.Offset))}
	var sent []*conn
	for p := range s.conns {
		_ = p.c.SetWriteDeadline(time.Now().Add(dialTimeout))
		if err := p.enc.Encode(o); err != nil {
			continue
		}
		sent = append(sent, p)
	}
	deadline := time.NewTimer(ackTimeout)
	defer deadline.Stop()
	expired := false
	var late []string
	for _, p := range sent {
		switch a, outcome := p.await(o.Seq, deadline.C, &expired); outcome {
		case awaitAcked:
			res.Processes++
			res.Fired += a.Fired
		case awaitLate:
			late = append(late, fmt.Sprintf("pid %d (%s)", p.hello.PID, p.hello.Command))
		}
	}
	if len(late) > 0 {
		slices.Sort(late)
		return res, fmt.Errorf("clock: no acknowledgement of the advance by %s within %s from %s", d, ackTimeout, strings.Join(late, ", "))
	}
	return res, nil
}

// The outcomes of waiting for one process's acknowledgement.
const (
	awaitAcked = iota
	awaitGone
	awaitLate
)

// await waits for p's acknowledgement of seq until deadline; once it has
// passed (expired), it only takes an acknowledgement already there, so
// that one late process does not cost the others their answers.
func (p *conn) await(seq int64, deadline <-chan time.Time, expired *bool) (ack, int) {
	for {
		if *expired {
			select {
			case a := <-p.acks:
				if a.Seq == seq {
					return a, awaitAcked
				}
				continue
			case <-p.gone:
				return p.last(seq)
			default:
				return ack{}, awaitLate
			}
		}
		select {
		case a := <-p.acks:
			if a.Seq == seq {
				return a, awaitAcked
			}
		case <-p.gone:
			return p.last(seq)
		case <-deadline:
			*expired = true
		}
	}
}

// last is the outcome for p once it has gone: its acknowledgement of seq if
// it sent one before it ended, which its handler queued before it said the
// process was gone, else gone.
func (p *conn) last(seq int64) (ack, int) {
	for {
		select {
		case a := <-p.acks:
			if a.Seq == seq {
				return a, awaitAcked
			}
		default:
			return ack{}, awaitGone
		}
	}
}

// Processes are the processes connected now, by PID and command line.
func (s *Server) Processes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.conns))
	for p := range s.conns {
		out = append(out, fmt.Sprintf("pid %d (%s)", p.hello.PID, p.hello.Command))
	}
	slices.Sort(out)
	return out
}

// Close stops serving and ends every connection; the processes keep the
// last offset.
func (s *Server) Close() error {
	if s.ln == nil {
		return nil
	}
	err := s.ln.Close()
	s.mu.Lock()
	for p := range s.conns {
		_ = p.c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
