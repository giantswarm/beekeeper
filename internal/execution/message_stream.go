package execution

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"time"
)

// One stream belongs to the already journaled runner. SSH notifications only
// wake its inbox reader; they never start or resume a harness process.
type messageStream struct {
	s         *Server
	ref       Ref
	ctx       context.Context
	cancel    context.CancelFunc
	reader    *os.File
	writer    *os.File
	listener  net.Listener
	wake      chan struct{}
	done      chan struct{}
	listening chan struct{}
	started   bool
}

func (s *Server) openMessageStream(ctx context.Context, ref Ref) (*messageStream, error) {
	listener, err := net.Listen("unix", s.messageSocket(ref))
	if err != nil {
		return nil, errors.New("owned message runner already exists or its socket is unavailable")
	}
	ctx, cancel := context.WithCancel(ctx)
	r, w, err := os.Pipe()
	if err != nil {
		cancel()
		_ = listener.Close()
		return nil, err
	}
	m := &messageStream{s: s, ref: ref, ctx: ctx, cancel: cancel, reader: r, writer: w, listener: listener, wake: make(chan struct{}, 1), done: make(chan struct{}), listening: make(chan struct{})}
	return m, nil
}

func (m *messageStream) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *messageStream) Start(r StartRequest) {
	m.started = true
	go func() {
		defer close(m.listening)
		for {
			c, err := m.listener.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
			m.signal()
		}
	}()
	go func() {
		defer close(m.done)
		defer func() { _ = m.writer.Close() }()
		initial := Envelope{ID: r.RequestID, Text: r.Prompt}
		if _, err := m.writer.Write(userMessage(initial, r.ConversationID)); err != nil {
			return
		}
		m.signal()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-m.wake:
				if err := m.sendNext(); err != nil {
					_ = m.writer.Close()
					return
				}
			}
		}
	}()
}

func (m *messageStream) Close() {
	m.cancel()
	_ = m.listener.Close()
	_ = m.reader.Close()
	_ = m.writer.Close()
	if m.started {
		<-m.done
		<-m.listening
	}
}

func (m *messageStream) sendNext() error {
	current, err := m.s.Read(m.ref)
	if err != nil {
		return err
	}
	if current.Completed {
		_ = m.writer.Close()
		return nil
	}
	if !current.Ready {
		return nil
	}
	if _, ok := current.Messages.Next(); !ok {
		return nil
	}
	// A queued wake cannot authorize a model call after the coordinator has
	// stopped it. Loss of authority leaves it stored, without killing the
	// existing conversation or attempting a replacement process.
	if err := m.s.authorizeMessage(m.ctx, current); err != nil {
		return m.s.update(m.ctx, m.ref, func(j *Journal) error {
			j.LastError = "message remains stored: coordinating authority unavailable or stopped"
			return nil
		})
	}
	var next Envelope
	var session string
	err = m.s.update(m.ctx, m.ref, func(j *Journal) error {
		if j.Completed {
			_ = m.writer.Close()
			return nil
		}
		if j.StopRequest != nil {
			return errors.New("execution stopped before message delivery")
		}
		if !j.Ready {
			return nil
		}
		var ok bool
		next, ok = j.Messages.Next()
		if !ok {
			return nil
		}
		session = j.Handle.ConversationID
		if err := j.Messages.Attempt(next); err != nil {
			return err
		}
		j.InTurn, j.LastActive = true, m.s.now()
		return nil
	})
	if err != nil || next.ID == "" {
		return err
	}
	_, err = m.writer.Write(userMessage(next, session))
	return err
}

func (s *Server) authorizeMessage(ctx context.Context, j Journal) error {
	if s.Authorize != nil {
		return s.Authorize(ctx, j.Request)
	}
	co, ok := s.Config.Coordinators[j.Request.Coordinator]
	if !ok {
		return errors.New("execution coordinator is no longer trusted")
	}
	var decision struct {
		Stopped    bool `json:"stopped"`
		GitHubHeld bool `json:"githubHeld"`
	}
	if err := CoordinatorCall(ctx, co.Callback, string(j.Handle.Ref.Host), "guard", j.Handle.Ref, nil, j.Request.ControlKey, &decision, s.Callback); err != nil {
		return err
	}
	if decision.Stopped {
		return errors.New("execution is stopped by the coordinator")
	}
	return nil
}

func (m *messageStream) Accept(line []byte, original Journal) error {
	if acceptedMessage(line, original.Handle.ConversationID, original.Request.RequestID) {
		if err := m.s.update(m.ctx, m.ref, func(j *Journal) error {
			j.Ready, j.InTurn, j.LastActive = true, true, m.s.now()
			return nil
		}); err != nil {
			return err
		}
		m.signal()
		return nil
	}
	j, err := m.s.Read(m.ref)
	if err != nil {
		return err
	}
	for _, d := range j.Messages.Status() {
		if d.Stage != Uncertain || !acceptedMessage(line, j.Handle.ConversationID, d.Envelope.ID) {
			continue
		}
		// Commit evidence separately before the journal receipt, permitting
		// recovery even if the runner dies between these two durable writes.
		evidence := struct{ ID, Session string }{d.Envelope.ID, j.Handle.ConversationID}
		p := filepath.Join(m.s.Root, string(m.ref.Execution)+"-"+d.Envelope.ID+".accepted")
		if err := atomicFile(p, mustJSON(evidence)); err != nil {
			return err
		}
		if err := m.s.update(m.ctx, m.ref, func(j *Journal) error {
			if err := j.Messages.Accept(d.Envelope.ID); err != nil {
				return err
			}
			j.InTurn, j.LastActive = true, m.s.now()
			return nil
		}); err != nil {
			return err
		}
		m.signal()
	}
	return nil
}

// WaitAck lets an idle runner confirm storage quickly without keeping an SSH
// session alive for the model's turn. Busy or disconnected runners stay stored.
func (s *Server) waitMessageAck(ctx context.Context, ref Ref, id string) (Ack, error) {
	ctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		j, err := s.Read(ref)
		if err != nil {
			return Ack{}, err
		}
		d, ok := j.Messages.Entries[id]
		if !ok {
			return Ack{}, errors.New("message receipt disappeared")
		}
		if d.Stage == Accepted {
			return d.Ack(), nil
		}
		select {
		case <-ctx.Done():
			return d.Ack(), nil
		case <-tick.C:
		}
	}
}
