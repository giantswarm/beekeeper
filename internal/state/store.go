package state

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// NoteLookup is the optional complete history of a store that retains closed notes.
type NoteLookup interface {
	LookupNote(int) (*Note, *Event, error)
}

// NoteHistory is the optional history of a store that retains central note
// closures beyond the late-answer window. File-backed stores need not implement it.
type NoteHistory interface {
	NoteClosure(int) (*Event, error)
}

// Store is the seam every command reads and changes the state through.
type Store interface {
	// Dir is the machine-local directory beside the state: the merge
	// gate's and the timers' side files.
	Dir() string
	// Read returns the current state.
	Read() (*State, error)
	// Peek returns the current state without waiting on a writer.
	Peek() (*State, error)
	// Update runs fn on the state and saves the result together with the
	// events fn returns; when fn fails nothing is saved.
	Update(fn func(*State) ([]Event, error)) error
	// UpdateWithin is Update for a caller that must not wait long, a
	// session's start for one: a lock busy past wait fails it.
	UpdateWithin(wait time.Duration, fn func(*State) ([]Event, error)) error
	// Exclusive runs fn while no other process holds the state, writing
	// nothing.
	Exclusive(fn func() error) error
	// Log appends events that change no state, giving up on a busy lock.
	Log(events ...Event) error
	// Record appends events that change no state and must not be lost,
	// waiting for the lock as an update does.
	Record(events ...Event) error
	// RecordWithin is Record for a caller that must not wait long, an
	// agent's hand-over note for one: a lock busy past wait fails it with
	// a LockBusyError, the events not appended.
	RecordWithin(wait time.Duration, events ...Event) error
	// Events returns the last n events keep accepts, oldest first.
	Events(n int, keep func(Event) bool) ([]Event, error)
	// Follow returns every event keep accepts, oldest first, as Events(0,
	// keep); a reader that calls it again under the same name, with the
	// same keep, may be handed what it read before and only what was logged
	// since read.
	Follow(name string, keep func(Event) bool) ([]Event, error)
	// ReadFile decodes a JSON side file; found is false when it is missing.
	ReadFile(name string, v any) (found bool, err error)
	// WriteFile replaces a JSON side file.
	WriteFile(name string, v any) error
}

// FileStore is the Store in a directory of the machine: state.json under a
// flock, events.jsonl beside it.
type FileStore struct {
	dir string
	// version is the binary's, which every save stamps or judges.
	version string
	// follows are the Follow readers' places in the log, by name; refused
	// says a save was refused and logged, once per process.
	mu            sync.Mutex
	follows       map[string]*followed
	refused       bool
	stopMu        sync.Mutex
	stopFile      os.FileInfo
	stopRequested bool
	eventsMu      sync.Mutex
	events        []*cachedEventFile
	snapshotMu    sync.Mutex
	snapshotFile  os.FileInfo
	snapshot      *State
	snapshotSum   [32]byte

	// outboxFile is the state file whose outbox outboxPendingSeen answers.
	outboxMu          sync.Mutex
	outboxFile        os.FileInfo
	outboxPendingSeen bool
}

var _ Store = (*FileStore)(nil)

// Open returns the file store in dir, creating the directory, saving as
// this binary's version.
func Open(dir string) (*FileStore, error) { return OpenVersion(dir, project.Version()) }

// OpenVersion is Open saving as version: a release stamps the state and is
// refused by a newer one's, a build without a release version does neither.
func OpenVersion(dir, version string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &FileStore{dir: dir, version: version}, nil
}

// Dir is the store's directory.
func (s *FileStore) Dir() string { return s.dir }

func (s *FileStore) path(name string) string { return filepath.Join(s.dir, name) }

// Read opens the current state under a shared lock, then decodes or clones
// the cached snapshot after releasing it. Large read-only views must not hold up writers while
// allocating and decoding the retained history. Pending outbox recovery still
// re-reads the latest state under the exclusive lock before changing anything.
func (s *FileStore) Read() (*State, error) {
	unlock, err := s.lock(true, lockWait)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(s.path("state.json"))
	unlock()
	if errors.Is(err, os.ErrNotExist) {
		return &State{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := s.readSnapshot(f)
	if err != nil || st.EventOutbox == nil {
		return st, err
	}
	unlock, err = s.lock(false, lockWait)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.recoverEvents()
}

// Peek returns the current state without taking the lock, for a caller
// that must never wait on it (a permission hook): a write replaces the file
// in one rename, so the document read is always a whole one.
func (s *FileStore) Peek() (*State, error) { return s.load() }

// StopRequested reads only stop intent from the atomic snapshot. Frequent
// cancellation probes must not allocate every retained record and note.
func (s *FileStore) StopRequested() (bool, error) {
	// Keep this independent of Follow's mutex: a cold event-log scan must
	// never delay cancellation. Atomic replacement changes file identity.
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
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
	if old := s.stopFile; old != nil && os.SameFile(old, info) && old.Size() == info.Size() && old.ModTime().Equal(info.ModTime()) {
		return s.stopRequested, nil
	}
	var intent struct {
		Stop *Stop `json:"stop"`
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(raw, &intent); err != nil {
		return false, err
	}
	s.stopFile, s.stopRequested = info, intent.Stop != nil
	return s.stopRequested, nil
}

// Update runs fn on the state under the exclusive lock and writes the result
// back with the events fn returns under the same lock. When fn fails nothing
// is written. Newer release state refuses the callback with StaleWriterError.
// Event-log open failures leave state unchanged. Once state commits, its durable
// outbox retains the events until their append is synced. Reads and later writes
// recover an interrupted append before using the state.
func (s *FileStore) Update(fn func(*State) ([]Event, error)) error {
	return s.UpdateWithin(lockWait, fn)
}

// UpdateWithin is Update waiting at most wait for the lock.
func (s *FileStore) UpdateWithin(wait time.Duration, fn func(*State) ([]Event, error)) error {
	unlock, err := s.lock(false, wait)
	if err != nil {
		return err
	}
	defer unlock()
	st, base, err := s.begin()
	if err != nil {
		return err
	}
	if base != nil {
		defer func() { _ = base.Close() }()
	}
	if err := s.checkWriter(st, clock.Now()); err != nil {
		return err
	}
	leasesBefore := make(map[execution.Ref]execution.LeaseGrant)
	for _, start := range st.Starts {
		if r := start.Execution; r != nil && r.Lease != nil {
			leasesBefore[r.Request.Ref] = *r.Lease
		}
	}
	events, err := fn(st)
	if err != nil {
		return err
	}
	if err := s.stamp(st, clock.Now()); err != nil {
		return err
	}
	return s.commitEvents(st, events, leasesBefore, base)
}

// ReadFile decodes a JSON side file of the store (the last snapshot);
// found is false when it does not exist yet.
func (s *FileStore) ReadFile(name string, v any) (found bool, err error) {
	raw, err := os.ReadFile(s.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, v)
}

// WriteFile atomically replaces a JSON side file of the store.
func (s *FileStore) WriteFile(name string, v any) error {
	return writeJSON(s.path(name), v)
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeRaw(path, append(raw, '\n'))
}

// writeRaw atomically replaces path with raw.
func writeRaw(path string, raw []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
