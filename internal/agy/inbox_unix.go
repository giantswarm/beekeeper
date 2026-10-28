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

	"github.com/google/uuid"
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

// Send queues msg in the persistent inbox at path with a unique message ID and
// wakes the runner on the FIFO. When no process holds the FIFO open for
// reading, Send returns ErrNotRunning without queuing msg.
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
		q.NextSeq++
		q.Messages = append(q.Messages, QueuedMessage{
			ID:   fmt.Sprintf("%d-%s", q.NextSeq, uuid.NewString()[:8]),
			Text: msg,
		})
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

// Drain marks all pending messages in the persistent inbox at path inflight
// in Injected (keeping any unconfirmed Injected messages ahead of newly queued
// Messages, deduplicated by message ID) and returns their texts in FIFO order.
// Drained messages remain in Injected on disk until confirmed by Confirm (on
// a stream user_input event) or by appearing in the conversation's transcript.
func Drain(path string) ([]string, error) {
	return drainHook(path, "")
}

func drainHook(path, transcriptPath string) ([]string, error) {
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
		highSeq := q.reconcile(transcriptPath)
		if q.Inflight != nil && q.Inflight.InputSeq > highSeq {
			highSeq = q.Inflight.InputSeq
		}
		pending := dedupMessages(q.Delivered, q.Injected, q.Messages)
		q.Messages = nil
		for i := range pending {
			pending[i].InputSeq = highSeq + i + 1
		}
		q.Injected = pending
		if inputs, ok := transcriptUserInputs(q.Transcript); ok {
			q.TranscriptInputs = len(inputs)
		}
		if len(pending) > 0 {
			msgs = make([]string, len(pending))
			for i, m := range pending {
				msgs[i] = m.Text
			}
		}
		return writeQueue(path, q)
	})
	return msgs, err
}

// Confirm records that agy accepted one user_input step on the stream for the
// inbox at path, confirming the corresponding Inflight or head Injected
// message and recording its ID in Delivered.
func Confirm(path string) error {
	return withLock(path, func() error {
		q, err := readQueue(path)
		if err != nil {
			return err
		}
		q.StreamInputs++
		q.reconcile("")
		return writeQueue(path, q)
	})
}

// popInflight moves the head message of the persistent queue at path into
// Inflight and returns it. When the queue is empty, ok is false.
func popInflight(path string) (msg string, ok bool, err error) {
	err = withLock(path, func() error {
		q, err := readQueue(path)
		if err != nil {
			return err
		}
		highSeq := q.reconcile("")
		q.Messages = dedupMessages(q.Delivered, q.Messages)
		if len(q.Messages) == 0 {
			return writeQueue(path, q)
		}
		head := q.Messages[0]
		head.InputSeq = highSeq + 1
		q.Inflight = &head
		q.Messages = q.Messages[1:]
		if inputs, tok := transcriptUserInputs(q.Transcript); tok {
			q.TranscriptInputs = len(inputs)
		}
		msg, ok = head.Text, true
		return writeQueue(path, q)
	})
	return msg, ok, err
}

// finishTurn reconciles the queue at the end of an agy turn (result event):
// the turn's initial stdin message (Inflight) is confirmed, any Injected
// messages confirmed in the transcript are marked delivered, and any
// remaining unconfirmed Injected messages (e.g. from a timed-out or killed
// PreInvocation hook on the final model call) are restored to the front of
// Messages so the runner delivers them on stdin as the next turn.
func finishTurn(path string) error {
	return withLock(path, func() error {
		q, err := readQueue(path)
		if err != nil {
			return err
		}
		if q.Inflight != nil {
			if q.Inflight.InputSeq > q.StreamInputs {
				q.StreamInputs = q.Inflight.InputSeq
			}
			q.markDelivered(q.Inflight.ID)
			q.Inflight = nil
		}
		q.reconcile("")
		if len(q.Injected) > 0 {
			for i := range q.Injected {
				q.Injected[i].InputSeq = 0
			}
			q.Messages = dedupMessages(q.Delivered, q.Injected, q.Messages)
			q.Injected = nil
		}
		return writeQueue(path, q)
	})
}

// restoreInflight reconciles the transcript and returns any unaccepted
// Inflight or Injected messages to the front of Messages (deduplicated against
// Delivered) when a runner starts on an inbox after a crash or restart.
func restoreInflight(path string) error {
	return withLock(path, func() error {
		q, err := readQueue(path)
		if err != nil {
			return err
		}
		q.reconcile("")
		q.StreamInputs = 0
		var inflight []QueuedMessage
		if q.Inflight != nil {
			q.Inflight.InputSeq = 0
			inflight = append(inflight, *q.Inflight)
			q.Inflight = nil
		}
		for i := range q.Injected {
			q.Injected[i].InputSeq = 0
		}
		q.Messages = dedupMessages(q.Delivered, inflight, q.Injected, q.Messages)
		q.Injected = nil
		if inputs, ok := transcriptUserInputs(q.Transcript); ok {
			q.TranscriptInputs = len(inputs)
		}
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
	if q.Inflight == nil && len(q.Injected) == 0 && len(q.Messages) == 0 && len(q.Delivered) == 0 {
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
