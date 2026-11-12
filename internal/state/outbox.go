package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/giantswarm/beekeeper/internal/execution"
)

// EventOutbox is an append committed atomically with state.json. Offset and
// Data make replay idempotent even after a partial write or an unclean exit.
type EventOutbox struct {
	Offset int64  `json:"offset"`
	Data   []byte `json:"data"`
}

// begin reads a transaction's state under the exclusive state lock, without
// the commit lock. The only writer without the state lock, a lease renewal,
// replaces state.json in one rename, and commitEvents carries its grant
// forward: a renewal never waits for this decode and copy of the whole
// document. A pending outbox is still recovered under the commit lock. base,
// open until the commit, pins the file the state was read from, so the commit
// can tell whether a renewal replaced it since; nil when unknown.
func (s *FileStore) begin() (st *State, base *os.File, err error) {
	f, err := os.Open(s.path("state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return &State{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	st, err = s.readSnapshot(f)
	if err == nil && st.EventOutbox == nil {
		return st, f, nil
	}
	_ = f.Close()
	if err != nil {
		return nil, nil, err
	}
	st, err = s.recoverEvents()
	return st, nil, err
}

// commitEvents writes a transaction's state and events under the commit lock.
// What needs no commit lock happens before it: the event log changes only
// under the exclusive state lock the caller holds (appends, rotation and
// outbox recovery all take it), and both documents are encoded ahead. A lease
// renewal waits briefly for the commit lock (execution.ErrCoordinatorBusy):
// it must never wait for an encoding of the whole state. Only a renewal that
// replaced state.json since begin makes the commit encode again, under the
// lock, with the renewed grant carried forward.
func (s *FileStore) commitEvents(st *State, events []Event, leasesBefore map[execution.Ref]execution.LeaseGrant, base *os.File) error {
	var outbox *EventOutbox
	var log *os.File
	if len(events) > 0 {
		if err := s.rotateEvents(); err != nil {
			return err
		}
		f, err := s.openEventLog()
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		if outbox, err = newOutbox(f, events); err != nil {
			return err
		}
		log = f
	}
	staged, final, err := encodeCommit(st, outbox)
	if err != nil {
		return err
	}
	unlock, err := s.commitLock(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	renewed, err := s.preserveRenewals(st, leasesBefore, base)
	if err != nil {
		return err
	}
	if renewed {
		if staged, final, err = encodeCommit(st, outbox); err != nil {
			return err
		}
	}
	if outbox == nil {
		return s.replaceState(final)
	}
	if err := writeRaw(s.path("state.json"), staged); err != nil {
		return err
	}
	if err := appendOutbox(outbox, log); err != nil {
		return err
	}
	return s.replaceState(final)
}

// newOutbox is events' append at the end of the log f, after a newline that
// completes a torn last line.
func newOutbox(f *os.File, events []Event) (*EventOutbox, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var data bytes.Buffer
	if info.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, info.Size()-1); err != nil {
			return nil, err
		}
		if last[0] != '\n' {
			data.WriteByte('\n')
		}
	}
	for _, e := range events {
		e.At = e.At.UTC()
		if err := json.NewEncoder(&data).Encode(e); err != nil {
			return nil, err
		}
	}
	return &EventOutbox{Offset: info.Size(), Data: data.Bytes()}, nil
}

// encodeCommit is what a commit writes: final, the state without an outbox,
// and with an outbox staged, the state carrying it, written before the
// append so a crash cannot lose it. st keeps no outbox.
func encodeCommit(st *State, outbox *EventOutbox) (staged, final []byte, err error) {
	st.EventOutbox = nil
	if final, err = encodeState(st); err != nil || outbox == nil {
		return nil, final, err
	}
	staged, err = stageOutbox(final, outbox)
	return staged, final, err
}

const outboxMember = "eventOutbox"

// stageOutbox is final, an encoded state without an outbox, with outbox added
// as its last member: the document the state with outbox encodes to, but for
// the members' order, which no reader depends on. The state is encoded once,
// not twice, under the state lock.
func stageOutbox(final []byte, outbox *EventOutbox) ([]byte, error) {
	member, err := json.MarshalIndent(outbox, "  ", "  ")
	if err != nil {
		return nil, err
	}
	body := bytes.TrimRight(final, " \t\r\n")
	if len(body) < 2 || body[0] != '{' || body[len(body)-1] != '}' {
		return nil, errors.New("encoded state is not a JSON object")
	}
	body = bytes.TrimRight(body[:len(body)-1], " \t\r\n")
	staged := make([]byte, 0, len(body)+len(member)+len(outboxMember)+16)
	staged = append(staged, body...)
	if len(body) > 1 {
		staged = append(staged, ',')
	}
	staged = append(staged, "\n  \""+outboxMember+"\": "...)
	staged = append(staged, member...)
	return append(staged, "\n}\n"...), nil
}

func encodeState(st *State) ([]byte, error) {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// recoverEvents runs under the exclusive state lock. A failure keeps the
// durable outbox; another command can retry after the underlying fault is fixed.
func (s *FileStore) recoverEvents() (*State, error) {
	unlock, err := s.commitLock(context.Background())
	if err != nil {
		return nil, err
	}
	defer unlock()

	st, err := s.load()
	if err != nil || st.EventOutbox == nil {
		return st, err
	}
	f, err := s.openEventLog()
	if err != nil {
		return nil, fmt.Errorf("recover event outbox: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := s.flushEvents(st, f); err != nil {
		return nil, fmt.Errorf("recover event outbox: %w", err)
	}
	return st, nil
}

// recoverPendingEvents checks only the outbox field before appending an event
// that changes no state, avoiding full State allocation when no outbox is pending.
// The answer is kept for the state file's identity: an append on every build's
// path must not scan the whole document again while it is unchanged.
func (s *FileStore) recoverPendingEvents() error {
	pending, err := s.outboxPending()
	if err != nil || !pending {
		return err
	}
	_, err = s.recoverEvents()
	return err
}

func (s *FileStore) outboxPending() (bool, error) {
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	f, err := os.Open(s.path("state.json")) //nolint:gosec // the store's own state file
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if sameSnapshot(s.outboxFile, info) {
		return s.outboxPendingSeen, nil
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return false, err
	}
	var pending struct {
		EventOutbox *EventOutbox `json:"eventOutbox"`
	}
	if err := json.Unmarshal(raw, &pending); err != nil {
		return false, err
	}
	s.outboxFile, s.outboxPendingSeen = info, pending.EventOutbox != nil
	return s.outboxPendingSeen, nil
}

type outboxFile interface {
	io.Writer
	io.ReaderAt
	Stat() (os.FileInfo, error)
	Sync() error
	Close() error
}

// flushEvents completes the outbox of st in the log f, then saves st without
// it.
func (s *FileStore) flushEvents(st *State, f outboxFile) error {
	if err := appendOutbox(st.EventOutbox, f); err != nil {
		return err
	}
	st.EventOutbox = nil
	return s.writeState(st)
}

// appendOutbox appends what of p the log f lacks, then syncs and closes f. It
// accepts only the exact prefix of the committed append: it never truncates
// unrelated history, and never duplicates a complete recovered event.
func appendOutbox(p *EventOutbox, f outboxFile) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	n := info.Size() - p.Offset
	if p.Offset < 0 || n < 0 || n > int64(len(p.Data)) {
		return fmt.Errorf("event outbox does not match log size; retained for recovery")
	}
	prefix := make([]byte, int(n))
	if _, err := f.ReadAt(prefix, p.Offset); err != nil && err != io.EOF {
		return err
	}
	if !bytes.Equal(prefix, p.Data[:int(n)]) {
		return fmt.Errorf("event outbox does not match log contents; retained for recovery")
	}
	if _, err := io.Copy(f, bytes.NewReader(p.Data[int(n):])); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

// writeState replaces state.json with st (replaceState). The caller holds the
// commit lock.
func (s *FileStore) writeState(st *State) error {
	raw, err := encodeState(st)
	if err != nil {
		return err
	}
	return s.replaceState(raw)
}

// replaceState replaces state.json with raw unless the document would stay
// byte for byte the same. A save that changes nothing must not change the
// file's identity: every process's snapshot cache keys on it, and the loop,
// the guide and lease waits wake on its replacement. The transaction loaded
// the current document through the snapshot cache, which keeps its digest.
// The caller holds the commit lock.
func (s *FileStore) replaceState(raw []byte) error {
	if s.unchangedSnapshot(raw) {
		return nil
	}
	return writeRaw(s.path("state.json"), raw)
}

// Windows does not expose a portable directory fsync; file data is still
// synced before rename, as for the execution journal's directory sync.
func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}
