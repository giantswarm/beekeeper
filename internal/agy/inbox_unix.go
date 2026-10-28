//go:build unix

package agy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// MakeInbox creates the inbox FIFO at path, keeping one that exists. Any
// persistent queue file beside it (<path>.queue) is preserved so unread
// messages survive a session restart.
func MakeInbox(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if fi, err := os.Stat(path); err == nil {
		if fi.Mode()&os.ModeNamedPipe == 0 {
			return fmt.Errorf("%s exists and is no FIFO", path)
		}
		return nil
	}
	return syscall.Mkfifo(path, 0o600)
}

// Send queues msg in the persistent inbox at path and wakes the runner on the
// FIFO. When no process holds the FIFO open for reading, Send returns
// ErrNotRunning without queuing msg.
func Send(path, msg string) error {
	msg = strings.TrimSpace(msg)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return ErrNotRunning
	}
	return withLock(path, func() error {
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // the agent's inbox under the state folder
		if errors.Is(err, syscall.ENXIO) {
			return ErrNotRunning
		}
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		q, err := readQueue(path)
		if err != nil {
			return err
		}
		q.Messages = append(q.Messages, msg)
		if err := writeQueue(path, q); err != nil {
			return err
		}
		// Wake the runner if it is idle; EAGAIN means a wakeup byte is
		// already buffered in the pipe.
		if _, err := f.Write([]byte{'\n'}); err != nil && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		return nil
	})
}

// Drain removes and returns all queued messages from the persistent inbox at
// path in FIFO order, and clears any Inflight marker (a PreInvocation hook
// runs only once agy has started a turn).
func Drain(path string) ([]string, error) {
	if _, err := os.Stat(queuePath(path)); errors.Is(err, os.ErrNotExist) { //nolint:gosec // beside the agent's inbox under the state folder
		if _, serr := os.Stat(path); errors.Is(serr, os.ErrNotExist) { //nolint:gosec // the agent's inbox under the state folder
			return nil, nil
		}
	}
	var msgs []string
	err := withLock(path, func() error {
		q, err := readQueue(path)
		if err != nil {
			return err
		}
		if q.Inflight == "" && len(q.Messages) == 0 {
			return nil
		}
		msgs = q.Messages
		q.Inflight = ""
		q.Messages = nil
		return writeQueue(path, q)
	})
	return msgs, err
}

// popInflight moves the head message of the persistent queue at path into
// Inflight and returns it. When the queue is empty, ok is false.
func popInflight(path string) (msg string, ok bool, err error) {
	err = withLock(path, func() error {
		q, err := readQueue(path)
		if err != nil {
			return err
		}
		if len(q.Messages) == 0 {
			return nil
		}
		msg, ok = q.Messages[0], true
		q.Inflight = msg
		q.Messages = q.Messages[1:]
		return writeQueue(path, q)
	})
	return msg, ok, err
}

// clearInflight clears the Inflight message once agy recorded it in the
// conversation or finished the turn.
func clearInflight(path string) error {
	return withLock(path, func() error {
		q, err := readQueue(path)
		if err != nil || q.Inflight == "" {
			return err
		}
		q.Inflight = ""
		return writeQueue(path, q)
	})
}

// restoreInflight returns an unaccepted Inflight message to the front of
// Messages when a runner starts on an inbox after a crash or restart.
func restoreInflight(path string) error {
	return withLock(path, func() error {
		q, err := readQueue(path)
		if err != nil || q.Inflight == "" {
			return err
		}
		q.Messages = append([]string{q.Inflight}, q.Messages...)
		q.Inflight = ""
		return writeQueue(path, q)
	})
}

func withLock(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { //nolint:gosec // the agent's inbox directory under the state folder
		return err
	}
	lock, err := os.OpenFile(lockPath(path), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // beside the agent's inbox under the state folder
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	return fn()
}

func readQueue(path string) (queue, error) {
	b, err := os.ReadFile(queuePath(path)) //nolint:gosec // beside the agent's inbox under the state folder
	if errors.Is(err, os.ErrNotExist) || len(b) == 0 {
		return queue{}, nil
	}
	if err != nil {
		return queue{}, err
	}
	var q queue
	if err := json.Unmarshal(b, &q); err != nil {
		return queue{}, err
	}
	return q, nil
}

func writeQueue(path string, q queue) error {
	qp := queuePath(path)
	if q.Inflight == "" && len(q.Messages) == 0 {
		if err := os.Remove(qp); err != nil && !errors.Is(err, os.ErrNotExist) { //nolint:gosec // beside the agent's inbox under the state folder
			return err
		}
		return nil
	}
	b, err := json.Marshal(q)
	if err != nil {
		return err
	}
	tmp := qp + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil { //nolint:gosec // beside the agent's inbox under the state folder
		return err
	}
	return os.Rename(tmp, qp) //nolint:gosec // beside the agent's inbox under the state folder
}

// RemoveInbox removes the inbox FIFO at path, its queue and its lock file;
// removed is false when none of them existed.
func RemoveInbox(path string) (removed bool, err error) {
	for _, p := range []string{path, queuePath(path), lockPath(path)} {
		if rerr := os.Remove(p); rerr == nil {
			removed = true
		} else if !errors.Is(rerr, os.ErrNotExist) && err == nil {
			err = rerr
		}
	}
	return removed, err
}
