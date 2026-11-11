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

func (s *FileStore) commitEvents(st *State, events []Event, leasesBefore map[execution.Ref]execution.LeaseGrant) error {
	unlock, err := s.commitLock(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.preserveRenewals(st, leasesBefore); err != nil {
		return err
	}

	if len(events) == 0 {
		return s.writeState(st)
	}
	if err := s.rotateEvents(); err != nil {
		return err
	}
	f, err := s.openEventLog()
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	var data bytes.Buffer
	if info.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, info.Size()-1); err != nil {
			return err
		}
		if last[0] != '\n' {
			data.WriteByte('\n')
		}
	}
	for _, e := range events {
		e.At = e.At.UTC()
		if err := json.NewEncoder(&data).Encode(e); err != nil {
			return err
		}
	}
	st.EventOutbox = &EventOutbox{Offset: info.Size(), Data: data.Bytes()}
	if err := writeJSON(s.path("state.json"), st); err != nil {
		return err
	}
	return s.flushEvents(st, f)
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
	f, err := os.Open(s.path("state.json"))
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

// flushEvents accepts only the exact prefix of the committed append. It never
// truncates unrelated history, and never duplicates a complete recovered event.
func (s *FileStore) flushEvents(st *State, f outboxFile) error {
	p := st.EventOutbox
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
	if err := f.Close(); err != nil {
		return err
	}
	st.EventOutbox = nil
	return s.writeState(st)
}

// writeState replaces state.json unless the document would stay byte for byte
// the same. A save that changes nothing must not change the file's identity:
// every process's snapshot cache keys on it, and the loop, the guide and lease
// waits wake on its replacement. The caller holds the commit lock.
func (s *FileStore) writeState(st *State) error {
	path := s.path("state.json")
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, raw) {
		return nil
	}
	return writeRaw(path, raw)
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
