package state

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"sync"
	"time"
)

// Event is one line of events.jsonl. At is written in UTC (RFC 3339 with a
// trailing Z), so a reader can compare it as a string.
type Event struct {
	At     time.Time `json:"at"`
	By     Party     `json:"by"`
	Verb   string    `json:"verb"`
	Detail string    `json:"detail"`
	// Subject is the issue or pull request the event concerns, owner/repo#n,
	// on the events the sink may dispatch (config.SinkVerbs); empty on the
	// rest.
	Subject string `json:"subject,omitempty"`
	// NoteTeam identifies the note owner independently of the event actor.
	NoteTeam string `json:"noteTeam,omitempty"`
	// NoteSnapshot retains the complete note for file-backed reopen after closure.
	NoteSnapshot *Note `json:"noteSnapshot,omitempty"`
}

// followed is how far a Follow reader has read the log, and what it kept.
type followed struct {
	mu     sync.Mutex
	file   os.FileInfo
	offset int64
	kept   []Event
	anchor string
	// published* are the checkpoint FollowSnapshot last wrote: when, and
	// up to where in which file.
	publishedAt     time.Time
	publishedFile   os.FileInfo
	publishedOffset int64
}

// Log appends events that change no state, a build's run for one, under
// the state lock. It waits at most logWait for the lock, so that a caller on
// every build's path never queues behind a slow update.
func (s *FileStore) Log(events ...Event) error {
	unlock, err := s.lock(false, logWait)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.recoverPendingEvents(); err != nil {
		return err
	}
	return s.append(events)
}

const logWait = time.Second

// Record appends events that change no state and must not be lost, a secret
// call's audit entry for one, under the state lock, waiting for it as an
// update does: a busy lock delays the entry, and a lock busy past lockWait
// fails the call instead of dropping it.
func (s *FileStore) Record(events ...Event) error {
	return s.RecordWithin(lockWait, events...)
}

// RecordWithin is Record waiting at most wait for the lock.
func (s *FileStore) RecordWithin(wait time.Duration, events ...Event) error {
	unlock, err := s.lock(false, wait)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.recoverPendingEvents(); err != nil {
		return err
	}
	return s.append(events)
}

func (s *FileStore) append(events []Event) error {
	if len(events) == 0 {
		return nil
	}
	if err := s.rotateEvents(); err != nil {
		return err
	}
	f, err := s.openEventLog()
	if err != nil {
		return err
	}
	return appendEvents(f, events)
}

func (s *FileStore) openEventLog() (*os.File, error) {
	return os.OpenFile(s.path("events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
}

// appendEvents consumes and closes f, including on failure.
func appendEvents(f *os.File, events []Event) error {
	if err := endLine(f); err != nil {
		_ = f.Close()
		return err
	}
	enc := json.NewEncoder(f)
	for _, e := range events {
		e.At = e.At.UTC()
		if err := enc.Encode(e); err != nil {
			_ = f.Close()
			return err
		}
	}
	return f.Close()
}

// endLine writes a newline when the file does not end in one: an unclean
// shutdown can leave an append's range as NUL bytes or a torn line, and the
// next event must not share that line.
func endLine(f *os.File) error {
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return err
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, fi.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	_, err = f.Write([]byte{'\n'})
	return err
}

// Events returns the last n events keep accepts, oldest first (all when
// n <= 0, every event when keep is nil).
func (s *FileStore) Events(n int, keep func(Event) bool) ([]Event, error) {
	return s.EventsContext(context.Background(), n, keep)
}

// EventsContext abandons history replay when its caller stops. Partial history
// is never returned as a successful read.
func (s *FileStore) EventsContext(ctx context.Context, n int, keep func(Event) bool) ([]Event, error) {
	return s.cachedEventsContext(ctx, n, keep)
}

// Follow returns every event keep accepts, oldest first. It remembers under
// name how far it read and what it kept, and reads only the lines appended
// since: a watch that follows the log every poll parses each line once. A
// log that was replaced or shrank is read again from its start.
func (s *FileStore) Follow(name string, keep func(Event) bool) ([]Event, error) {
	return s.FollowContext(context.Background(), name, keep)
}

func (s *FileStore) FollowContext(ctx context.Context, name string, keep func(Event) bool) ([]Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	at := s.eventReader(name)
	at.mu.Lock()
	defer at.mu.Unlock()
	return s.followContext(ctx, at, keep)
}

func (s *FileStore) eventReader(name string) *followed {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.follows == nil {
		s.follows = map[string]*followed{}
	}
	if s.follows[name] == nil {
		s.follows[name] = &followed{}
	}
	return s.follows[name]
}

// followContext runs with this reader's mutex held. Unrelated readers never
// queue behind a cold history scan. A cursor names an inode, so sealing the
// active log into an archive preserves its position and its retained projection.
func (s *FileStore) followContext(ctx context.Context, at *followed, keep func(Event) bool) ([]Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	files, err := s.eventFilesContext(ctx)
	if err != nil {
		return nil, err
	}
	defer closeEventFiles(files)
	// kept only grows by append or is replaced, so its elements up to the
	// saved length stay as they are: a cancelled read restores the slice
	// without copying everything kept on every call.
	savedFile, savedOffset, savedKept := at.file, at.offset, at.kept
	restore := func() {
		at.file, at.offset, at.kept = savedFile, savedOffset, savedKept
	}
	start := 0
	if at != nil && at.file != nil {
		start = -1
		for i, f := range files {
			fi, err := f.Stat()
			if err != nil {
				return nil, err
			}
			if os.SameFile(at.file, fi) && fi.Size() >= at.offset {
				start = i
				break
			}
		}
	}
	if at.file == nil || start < 0 {
		at.file, at.offset, at.kept = nil, 0, nil
		start = 0
	}
	for _, f := range files[start:] {
		if err := ctx.Err(); err != nil {
			restore()
			return nil, err
		}
		fi, err := f.Stat()
		if err != nil {
			return nil, err
		}
		if at.file == nil || !os.SameFile(at.file, fi) {
			at.offset = 0
		}
		at.file = fi
		if _, err := f.Seek(at.offset, io.SeekStart); err != nil {
			return nil, err
		}
		reader := bufio.NewReader(f)
		for {
			if err := ctx.Err(); err != nil {
				restore()
				return nil, err
			}
			line, readErr := reader.ReadBytes('\n')
			if err := ctx.Err(); err != nil {
				restore()
				return nil, err
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				return slices.Clone(at.kept), readErr
			}
			var e Event
			lineTrimmed := bytes.TrimLeft(line, "\x00")
			if json.Unmarshal(lineTrimmed, &e) == nil && (keep == nil || keep(e)) {
				if err := ctx.Err(); err != nil {
					restore()
					return nil, err
				}
				at.kept = append(at.kept, e)
			}
			if err := ctx.Err(); err != nil {
				restore()
				return nil, err
			}
			at.offset += int64(len(line))
		}
	}
	if err := ctx.Err(); err != nil {
		restore()
		return nil, err
	}
	return slices.Clone(at.kept), nil
}
