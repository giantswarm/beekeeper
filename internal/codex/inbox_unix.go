//go:build unix

package codex

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/google/uuid"
)

// MakeInbox creates the named pipe at inbox (mode 0600) and its lock file.
func MakeInbox(inbox string) error {
	if err := os.MkdirAll(filepath.Dir(inbox), 0o700); err != nil {
		return err
	}
	if fi, err := os.Lstat(inbox); err == nil {
		if fi.Mode()&os.ModeNamedPipe == 0 {
			if err := os.Remove(inbox); err != nil {
				return err
			}
		} else {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syscall.Mkfifo(inbox, 0o600)
}

// RemoveInbox removes the inbox FIFO and its companion files (.queue, .lock, .state).
func RemoveInbox(inbox string) (bool, error) {
	fi, err := os.Lstat(inbox)
	if errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(QueuePath(inbox))
		_ = os.Remove(LockPath(inbox))
		_ = os.Remove(StatePath(inbox))
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if fi.Mode()&os.ModeNamedPipe == 0 {
		return false, nil
	}
	if err := os.Remove(inbox); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	_ = os.Remove(QueuePath(inbox))
	_ = os.Remove(LockPath(inbox))
	_ = os.Remove(StatePath(inbox))
	return true, nil
}

// withLock runs fn while holding an exclusive flock on LockPath(inbox).
func withLock(inbox string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(inbox), 0o700); err != nil {
		return err
	}
	lf, err := os.OpenFile(LockPath(inbox), os.O_CREATE|os.O_RDWR|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = lf.Close() }()
	syscall.CloseOnExec(int(lf.Fd()))
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) }()
	return fn()
}

// Send appends text to the persistent queue of inbox and signals the FIFO so
// an idle codex-run wakes at once. If no process holds the FIFO open for
// reading, Send returns ErrNotRunning without modifying the queue.
func Send(inbox, text string) error {
	return withLock(inbox, func() error {
		f, err := os.OpenFile(inbox, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0) //nolint:gosec // inbox path under state directory
		if err != nil {
			if errors.Is(err, syscall.ENXIO) || errors.Is(err, os.ErrNotExist) {
				return ErrNotRunning
			}
			return err
		}
		defer func() { _ = f.Close() }()

		q, err := readQueue(inbox)
		if err != nil {
			return err
		}
		q.Messages = append(q.Messages, QueuedMessage{
			ID:   uuid.NewString(),
			Text: text,
		})
		if err := writeQueue(inbox, q); err != nil {
			return err
		}
		// Best-effort 1-byte wake signal into the non-blocking FIFO.
		_, _ = f.Write([]byte{'\n'})
		return nil
	})
}

// Drain atomically pops the next message from inbox's persistent queue and
// marks it Inflight until Confirm is called. If a previous Inflight message
// was never confirmed (for example, codex exited before accepting it), Drain
// returns that Inflight message first so it is not lost. Any unconfirmed
// Injected messages from a prior turn are restored to the front of the queue.
func Drain(inbox string) ([]string, error) {
	var msg string
	var hasMsg bool
	err := withLock(inbox, func() error {
		q, err := readQueue(inbox)
		if err != nil {
			return err
		}
		restoreUnconfirmedInjected(&q)
		if q.Inflight != "" {
			if isDelivered(&q, q.InflightID) {
				q.Inflight = ""
				q.InflightID = ""
			} else {
				if q.InflightID == "" {
					q.InflightID = uuid.NewString()
				}
				msg, hasMsg = q.Inflight, true
				return writeQueue(inbox, q)
			}
		}
		for len(q.Messages) > 0 {
			next := q.Messages[0]
			q.Messages = q.Messages[1:]
			if isDelivered(&q, next.ID) {
				continue
			}
			q.Inflight = next.Text
			q.InflightID = next.ID
			msg, hasMsg = next.Text, true
			return writeQueue(inbox, q)
		}
		return writeQueue(inbox, q)
	})
	if err != nil || !hasMsg {
		return nil, err
	}
	return []string{msg}, nil
}

// DrainHook returns all pending mid-turn messages for the PreToolUse hook,
// keeping them inflight in q.Injected until ConfirmInjected records that the
// tool call started or the turn completed.
func DrainHook(inbox string) ([]QueuedMessage, error) {
	var out []QueuedMessage
	err := withLock(inbox, func() error {
		q, err := readQueue(inbox)
		if err != nil {
			return err
		}
		var active []QueuedMessage
		for _, m := range q.Injected {
			if !isDelivered(&q, m.ID) {
				active = append(active, m)
			}
		}
		for _, m := range q.Messages {
			if !isDelivered(&q, m.ID) {
				active = append(active, m)
			}
		}
		q.Messages = nil
		q.Injected = active
		out = active
		return writeQueue(inbox, q)
	})
	return out, err
}

// Confirm clears the Inflight message from inbox's persistent queue once codex
// has accepted the turn on stdin.
func Confirm(inbox string) error {
	return withLock(inbox, func() error {
		q, err := readQueue(inbox)
		if err != nil {
			return err
		}
		if q.Inflight == "" && q.InflightID == "" {
			return nil
		}
		markDelivered(&q, q.InflightID)
		q.Inflight = ""
		q.InflightID = ""
		return writeQueue(inbox, q)
	})
}

// ConfirmInjected marks all currently Injected mid-turn messages as delivered
// once the runner observes the tool call start or complete on the codex stream.
func ConfirmInjected(inbox string) error {
	return withLock(inbox, func() error {
		q, err := readQueue(inbox)
		if err != nil {
			return err
		}
		if len(q.Injected) == 0 {
			return nil
		}
		for _, m := range q.Injected {
			markDelivered(&q, m.ID)
		}
		q.Injected = nil
		return writeQueue(inbox, q)
	})
}

// openInboxRDWR opens the FIFO O_RDWR|O_CLOEXEC so codex-run holds both a
// reader and writer reference (keeping Send's O_WRONLY|O_NONBLOCK open check
// alive) while ensuring no child codex exec process inherits the descriptor.
// Do not call f.Fd() on the returned file: Go's os.OpenFile already sets
// O_CLOEXEC and registers the FIFO with the runtime poller.
func openInboxRDWR(inbox string) (*os.File, error) {
	return os.OpenFile(inbox, os.O_RDWR|syscall.O_CLOEXEC, 0) //nolint:gosec // inbox path under state directory
}
