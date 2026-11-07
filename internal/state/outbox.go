package state

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// EventOutbox is an append committed atomically with state.json. Offset and
// Data make replay idempotent even after a partial write or an unclean exit.
type EventOutbox struct {
	Offset int64  `json:"offset"`
	Data   []byte `json:"data"`
}

func (s *FileStore) commitEvents(st *State, events []Event) error {
	unlock, err := s.commitLock(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.preserveRenewals(st); err != nil {
		return err
	}

	if len(events) == 0 {
		return writeJSON(s.path("state.json"), st)
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
	return writeJSON(s.path("state.json"), st)
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
