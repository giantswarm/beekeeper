package execution

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/google/uuid"
)

const (
	MessageWake     = "wake"
	userMessageType = "user"
	Stored          = "stored"
	Accepted        = "accepted"
	Uncertain       = "uncertain"
)

type Delivery struct {
	Envelope Envelope `json:"envelope"`
	Digest   string   `json:"digest"`
	Stage    string   `json:"stage"`
}

// MessageLedger keeps receipts for the execution's lifetime. Sequence only
// advances on acceptance; gaps stay stored and an uncertain send blocks replay.
type MessageLedger struct {
	Sequence uint64              `json:"sequence"`
	Entries  map[string]Delivery `json:"entries,omitempty"`
}

func (l *MessageLedger) Store(ref Ref, m Envelope) (Ack, error) {
	if m.To != ref || m.From != ref || ref.Validate() != nil || !ValidIdentity(m.ID) || m.Sequence == 0 || m.Kind != MessageWake || m.Text == "" || len(m.Text) > MaxLog {
		return Ack{}, errors.New("invalid owned message envelope")
	}
	if id, err := uuid.Parse(m.ID); err != nil || id.String() != m.ID {
		return Ack{}, errors.New("message identity must be a canonical UUID")
	}
	digest := Digest(mustJSON(m))
	if old, ok := l.Entries[m.ID]; ok {
		if old.Digest != digest || old.Envelope.Sequence != m.Sequence {
			return Ack{}, errors.New("message idempotency conflict")
		}
		return old.Ack(), nil
	}
	if m.Sequence <= l.Sequence {
		return Ack{}, errors.New("message sequence replay")
	}
	for _, old := range l.Entries {
		if old.Envelope.Sequence == m.Sequence {
			return Ack{}, errors.New("message sequence reused")
		}
	}
	if l.Entries == nil {
		l.Entries = map[string]Delivery{}
	}
	d := Delivery{Envelope: m, Digest: digest, Stage: Stored}
	l.Entries[m.ID] = d
	return d.Ack(), nil
}

func (d Delivery) Ack() Ack {
	return Ack{ID: d.Envelope.ID, Sequence: d.Envelope.Sequence, Stage: d.Stage}
}

// Status excludes message text but retains identity and evidence for operators.
func (l MessageLedger) Status() []Delivery {
	var result []Delivery
	for _, d := range l.Entries {
		d.Envelope.Text = ""
		result = append(result, d)
	}
	slices.SortFunc(result, func(a, b Delivery) int {
		if a.Envelope.Sequence < b.Envelope.Sequence {
			return -1
		}
		if a.Envelope.Sequence > b.Envelope.Sequence {
			return 1
		}
		return 0
	})
	return result
}

func (l *MessageLedger) Next() (Envelope, bool) {
	for _, d := range l.Entries {
		if d.Envelope.Sequence == l.Sequence+1 && d.Stage == Stored {
			return d.Envelope, true
		}
	}
	return Envelope{}, false
}

func (l *MessageLedger) Attempt(m Envelope) error {
	d, ok := l.Entries[m.ID]
	if !ok || d.Stage != Stored || m.Sequence != l.Sequence+1 {
		return errors.New("message cannot be attempted out of order or replayed")
	}
	d.Stage = Uncertain
	l.Entries[m.ID] = d
	return nil
}

func (l *MessageLedger) Accept(id string) error {
	d, ok := l.Entries[id]
	if ok && d.Stage == Accepted && d.Envelope.Sequence <= l.Sequence {
		return nil
	}
	if !ok || d.Envelope.Sequence != l.Sequence+1 || d.Stage != Uncertain {
		return errors.New("message has no matching in-flight acceptance")
	}
	d.Stage, d.Envelope.Text = Accepted, ""
	l.Entries[id], l.Sequence = d, d.Envelope.Sequence
	return nil
}

func (s *Server) messageSocket(ref Ref) string {
	// Linux abstract sockets avoid pathname limits and leave nothing to unlink.
	return "\x00beekeeper-" + Digest([]byte(s.Root + ref.Key()))[7:39]
}

func (s *Server) Message(ctx context.Context, ref Ref, m Envelope) (ack Ack, err error) {
	err = s.update(ctx, ref, func(j *Journal) error {
		if !j.Request.Messaging {
			return ErrUnsupported
		}
		if m.ID == j.Request.RequestID {
			return errors.New("message identity is reserved for the initial prompt")
		}
		_, known := j.Messages.Entries[m.ID]
		if j.StopRequest != nil && !known {
			return errors.New("execution has a pending stop")
		}
		var e error
		ack, e = j.Messages.Store(ref, m)
		if e == nil && ack.Stage == Stored && (j.Phase == Exited || j.Phase == Failed || j.Completed) {
			d := j.Messages.Entries[m.ID]
			d.Stage = Failed
			j.Messages.Entries[m.ID] = d
			ack = d.Ack()
		}
		if e == nil && len(mustJSON(j)) > MaxWire-MaxLog {
			return errors.New("message journal is full; no message was stored")
		}
		return e
	})
	if err != nil || ack.Stage == Accepted || ack.Stage == Failed {
		return ack, err
	}
	if err := s.reconcileMessages(ctx, ref); err != nil {
		return ack, err
	}
	// Failure to signal is not a failed store and never authorizes a launch.
	dial := net.Dialer{Timeout: 100 * time.Millisecond}
	if conn, e := dial.DialContext(ctx, "unix", s.messageSocket(ref)); e == nil {
		_ = conn.Close()
	}
	return s.waitMessageAck(ctx, ref, m.ID)
}

// reconcileMessages restores only ID-bearing evidence already synced by the
// runner. It never launches a process or guesses from message text or exit code.
func (s *Server) reconcileMessages(ctx context.Context, ref Ref) error {
	j, err := s.Read(ref)
	if err != nil {
		return err
	}
	for _, d := range j.Messages.Status() {
		if d.Stage != Uncertain {
			continue
		}
		path := filepath.Join(s.Root, string(ref.Execution)+"-"+d.Envelope.ID+".accepted")
		var evidence struct{ ID, Session string }
		if err := readJSON(path, &evidence); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if evidence.ID != d.Envelope.ID || evidence.Session != j.Handle.ConversationID {
			return errors.New("message acceptance identity mismatch")
		}
		if err := s.update(ctx, ref, func(j *Journal) error { return j.Messages.Accept(evidence.ID) }); err != nil {
			return err
		}
	}
	return nil
}

func userMessage(m Envelope, session string) []byte {
	text := m.Text
	if m.Sender != (Sender{}) {
		text = "Message from " + string(mustJSON(m.Sender)) + ":\n\n" + text
	}
	return append(mustJSON(map[string]any{"type": userMessageType, "uuid": m.ID, "session_id": session, "parent_tool_use_id": nil, "message": map[string]any{"role": userMessageType, "content": text}}), '\n')
}

func acceptedMessage(raw []byte, session, id string) bool {
	var event struct {
		Type    string `json:"type"`
		Session string `json:"session_id"`
		UUID    string `json:"uuid"`
		Replay  bool   `json:"isReplay"`
	}
	return json.Unmarshal(raw, &event) == nil && event.Type == userMessageType && event.Session == session && event.UUID == id && event.Replay
}
