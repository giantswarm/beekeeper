package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/proc"
)

const (
	stateQueue = "state.lock.queue"
	lockPoll   = 10 * time.Millisecond
)

// Every acquisition joins the same cross-process queue before attempting the
// existing state.lock. A writer waits behind earlier readers and writers;
// readers may pass earlier readers, but never an earlier writer. Keep the
// ticket until the state lock is acquired, so short holders cannot repeatedly
// reacquire ahead of a waiting writer. Older binaries still share state.lock,
// but must be restarted on this protocol for the fairness guarantee to hold.
func (s *FileStore) lock(shared bool, wait time.Duration) (func(), error) {
	timing := startLockClock("", shared)
	unlock, err := s.acquireLock(shared, wait, timing)
	var busy *LockBusyError
	if errors.As(err, &busy) {
		timing.busy()
	}
	return unlock, err
}

func (s *FileStore) acquireLock(shared bool, wait time.Duration, timing *lockClock) (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if ctx.Err() != nil {
		return nil, s.busy(wait)
	}
	ticket, err := s.enqueueLock(shared)
	if err != nil {
		return nil, err
	}
	defer ticket.leave()
	if err := ticket.await(ctx, shared); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, s.busy(wait)
		}
		return nil, err
	}
	l := flock.New(s.path("state.lock"))
	take := l.TryLockContext
	if shared {
		take = l.TryRLockContext
	}
	ok, err := take(ctx, lockPoll)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	if !ok {
		return nil, s.busy(wait)
	}
	timing.acquire()
	if shared {
		return func() {
			_ = l.Unlock()
			timing.release()
		}, nil
	}
	holder := s.path(holderFile)
	_ = writeJSON(holder, lockHolder{PID: os.Getpid(), Command: holderCommand(), At: timing.acquired})
	return func() {
		_ = os.Remove(holder)
		_ = l.Unlock()
		timing.release()
	}, nil
}

type lockTicket struct {
	dir, name string
	lock      *flock.Flock
}

// Publish an already locked file: a process that dies leaves an unlocked
// ticket, which other waiters can safely reap without relying on PID reuse or
// an expiry. The sequence follows every visible ticket even if the clock moves
// backwards. Concurrent enqueues may tie; their unique suffix breaks the tie.
// A fresh enqueue always follows a writer already waiting in the queue.
func (s *FileStore) enqueueLock(shared bool) (*lockTicket, error) {
	dir := s.path(stateQueue)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, ".ticket-")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Close(); err != nil {
		return nil, err
	}
	l := flock.New(tmp, flock.SetFlag(os.O_RDWR))
	ok, err := l.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("new state lock ticket is already locked")
	}
	ticket := &lockTicket{dir: dir, lock: l}
	if err := ticket.publish(tmp, shared); err != nil {
		_ = l.Unlock()
		return nil, err
	}
	return ticket, nil
}

func (t *lockTicket) publish(tmp string, shared bool) error {
	entries, err := os.ReadDir(t.dir)
	if err != nil {
		return err
	}
	var last uint64
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		prefix, _, _ := strings.Cut(e.Name(), "-")
		n, err := strconv.ParseUint(prefix, 10, 64)
		if err != nil {
			return fmt.Errorf("read state lock ticket sequence: %w", err)
		}
		last = max(last, n)
	}
	if last+1 == 0 {
		return errors.New("state lock ticket sequence exhausted")
	}
	mode := "w"
	if shared {
		mode = "r"
	}
	t.name = fmt.Sprintf("%020d-%s.%s", last+1, strings.TrimPrefix(filepath.Base(tmp), "."), mode)
	// Link publishes atomically without overwriting another ticket, even in
	// the unlikely event of a temporary-name collision after a rename/removal.
	return os.Link(tmp, filepath.Join(t.dir, t.name))
}

func (t *lockTicket) leave() {
	if t.name == "" {
		return
	}
	name := t.name
	t.name = ""
	_ = os.Remove(filepath.Join(t.dir, name))
	_ = t.lock.Unlock()
}

func (t *lockTicket) await(ctx context.Context, shared bool) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ready, err := t.ready(shared)
		if err != nil || ready {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-clock.After(lockPoll):
		}
	}
}

func (t *lockTicket) ready(shared bool) (bool, error) {
	entries, err := os.ReadDir(t.dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || name >= t.name {
			continue
		}
		// Do not recreate tickets that departed after ReadDir. Unique names
		// ensure a stale probe can never remove a later waiter's ticket.
		path := filepath.Join(t.dir, name)
		l := flock.New(path, flock.SetFlag(os.O_RDWR))
		ok, err := l.TryLock()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if ok {
			err = os.Remove(path)
			_ = l.Unlock()
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return false, err
			}
			continue
		}
		if !shared || !strings.HasSuffix(name, ".r") {
			return false, nil
		}
	}
	return true, nil
}

// lockWait bounds every wait for the state lock but Log's: a holder that
// stops (a frozen or stopped process) fails the commands behind it with its
// PID and age (LockBusyError) instead of starving them.
const lockWait = 2 * time.Minute

// LockBusyError is a wait for the state lock that ended before its holder
// released it: who holds it, since when, as the holder recorded it; PID 0
// is a holder without a record (a reader, or an older beekeeper).
type LockBusyError struct {
	PID     int
	Command string
	Since   time.Time
	Waited  time.Duration
}

func (e *LockBusyError) Error() string {
	if e.PID == 0 {
		return fmt.Sprintf("the state lock is busy: still held after %s by a process without a holder record (a reader, or a beekeeper before the record)", e.Waited.Round(time.Millisecond))
	}
	return fmt.Sprintf("the state lock is busy: held by pid %d (%s) for %s, waited %s; a stopped or frozen holder releases it when it resumes or ends",
		e.PID, e.Command, clock.Since(e.Since).Round(time.Second), e.Waited.Round(time.Millisecond))
}

// lockHolder is the exclusive holder's record beside the lock, removed
// before the lock is released.
type lockHolder struct {
	PID     int       `json:"pid"`
	Command string    `json:"command"`
	At      time.Time `json:"at"`
}

const holderFile = "state.lock.holder"

// busy is the LockBusyError of a wait that ended after waited: a record
// of a process that ended is a holder without one.
func (s *FileStore) busy(waited time.Duration) error {
	e := &LockBusyError{Waited: waited}
	var h lockHolder
	if raw, err := os.ReadFile(s.path(holderFile)); err == nil && json.Unmarshal(raw, &h) == nil && proc.Alive(h.PID) {
		e.PID, e.Command, e.Since = h.PID, h.Command, h.At
	}
	return e
}

// Exclusive runs fn under the exclusive state lock, writing nothing: what
// must not happen while a process holds the lock, freezing a session's
// processes for one.
func (s *FileStore) Exclusive(fn func() error) error {
	unlock, err := s.lock(false, lockWait)
	if err != nil {
		return err
	}
	defer unlock()
	commitUnlock, err := s.commitLock(context.Background())
	if err != nil {
		return err
	}
	defer commitUnlock()
	return fn()
}

// Quiesce holds this process's writes for an exit that does not wait for
// them: the state lock when it frees within wait, so no append or update is
// under way, and always the commit lock, so no commit is half written. A
// state lock still busy after wait is no failure: an update holding it has
// not begun its commit, and its commit then waits behind the commit lock.
// release frees both; an exit without it frees them too.
func (s *FileStore) Quiesce(wait time.Duration) (release func(), err error) {
	unlock, err := s.lock(false, wait)
	var busy *LockBusyError
	switch {
	case errors.As(err, &busy):
		unlock = func() {}
	case err != nil:
		return nil, err
	}
	commitUnlock, err := s.commitLock(context.Background())
	if err != nil {
		unlock()
		return nil, err
	}
	return func() {
		commitUnlock()
		unlock()
	}, nil
}

// processCommand names this process in the state's diagnostics (SetCommand).
var processCommand atomic.Pointer[string]

// SetCommand names this process in the state lock's holder record, its lock
// trace and stale-writer reports: the resolved subcommand path and the names
// of the flags given, never an argument or a flag's value, which carry a
// note's text, a start's brief, a configuration path or a session's name.
// Unset or empty, the command is guessed from the process's arguments.
func SetCommand(c string) {
	processCommand.Store(&c)
}

// holderCommand is this process's command for diagnostics.
func holderCommand() string {
	if c := processCommand.Load(); c != nil && *c != "" {
		return *c
	}
	return command(os.Args)
}

// command is a process's beekeeper command, its subcommand words only: the
// arguments behind them are a timer's text, a note, a start's whole brief.
// "beekeeper agents start". A flag before the subcommand ends it, its value
// unknown: SetCommand names such a process.
func command(args []string) string {
	words := []string{filepath.Base(args[0])}
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "-") || len(words) == 3 {
			break
		}
		words = append(words, a)
	}
	return strings.Join(words, " ")
}
