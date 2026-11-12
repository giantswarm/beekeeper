package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/clock"
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
