package guard

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// The build slots' queue: a waiter for a slot (a run, a start that builds)
// holds a ticket, a file <slotDir>/queue/<unix nanoseconds>-<pid> its
// process keeps flocked while it waits, and may take a slot only while
// fewer tickets than slots are ahead of it. The waiters take the slots in
// the order they came, first in first out, instead of whichever polls the
// moment a slot frees. A ticket whose lock is free is a waiter that ended
// without leaving: the next look removes it. The memcap shell wrapper takes
// no ticket.

// queueDir is the slot directory's queue.
func queueDir(slotDir string) string { return filepath.Join(slotDir, "queue") }

// slotPoll is how often a waiter looks again; tests shorten it.
var slotPoll = 5 * time.Second

// ticket is a waiter's place in the queue.
type ticket struct {
	dir, name string
	lock      *flock.Flock
}

// enqueue takes a ticket at the end of the queue; what it waits for goes
// into the file, which the waits of the others name. The ticket is created
// under a dot name, locked, then renamed into the queue, so that no other
// waiter sees it unlocked and takes it for an ended one.
func enqueue(slotDir, what string) (*ticket, error) {
	dir := queueDir(slotDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%020d-%d", time.Now().UnixNano(), os.Getpid())
	tmp := filepath.Join(dir, "."+name)
	if err := os.WriteFile(tmp, []byte(oneLine(what)+"\n"), 0o600); err != nil {
		return nil, err
	}
	l := flock.New(tmp)
	if ok, err := l.TryLock(); err != nil || !ok {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("lock the slot queue's ticket %s: %v", tmp, err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		_ = l.Unlock()
		_ = os.Remove(tmp)
		return nil, err
	}
	return &ticket{dir: dir, name: name, lock: l}, nil
}

// ahead is the live tickets ahead of t, oldest first, each as its file
// says what it waits for; an ended waiter's ticket is removed.
func (t *ticket) ahead() []string { return waiters(t.dir, t.name) }

// waiters is the live tickets in the queue dir, oldest first, each as its
// file says what it waits for: those before the ticket name before, or
// every one for "". An ended waiter's ticket is removed.
func waiters(dir, before string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, ".") || (before != "" && n >= before) {
			continue
		}
		p := filepath.Join(dir, n)
		l := flock.New(p)
		if ok, err := l.TryLock(); err == nil && ok {
			_ = os.Remove(p)
			_ = l.Unlock()
			continue
		}
		raw, _ := os.ReadFile(filepath.Clean(p))
		out = append(out, strings.TrimSpace(string(raw)))
	}
	return out
}

// leave gives up the ticket; a nil ticket has none.
func (t *ticket) leave() {
	if t == nil {
		return
	}
	_ = os.Remove(filepath.Join(t.dir, t.name))
	_ = t.lock.Unlock()
}

// turn reports whether t may take a slot: fewer tickets than slots ahead.
// Otherwise it says who is ahead, for the wait's line. A nil ticket (the
// queue could not be joined) always has its turn.
func (t *ticket) turn(slots int) (bool, string) {
	if t == nil {
		return true, ""
	}
	a := t.ahead()
	if len(a) < slots {
		return true, ""
	}
	return false, aheadText(a)
}

// aheadText names the waiters a ahead in the queue, one numbered line each.
func aheadText(a []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d ahead in the build slot queue:\n", len(a))
	for i, w := range a {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, w)
	}
	return b.String()
}

// PeekSlot reports how a start that builds would find the build slots now,
// judged as AwaitSlot judges its turn, without joining the queue or holding
// a slot: free when fewer waiters than slots are queued and a slot is free.
// Otherwise wait says what the start would wait for: the slots' holders
// and the waiters ahead of it, one indented line each.
func PeekSlot(o Options) (free bool, wait string, err error) {
	if err := os.MkdirAll(o.SlotDir, 0o750); err != nil {
		return false, "", err
	}
	a := waiters(queueDir(o.SlotDir), "")
	lock, _, occupied := acquire(o)
	if lock != nil {
		_ = lock.Unlock()
		if len(a) < o.Slots {
			return true, "", nil
		}
	}
	var s string
	if lock == nil {
		s = fmt.Sprintf("a build slot (all %d held):\n%s%s", o.Slots, holders(o), occupied)
	}
	if len(a) > 0 {
		s += aheadText(a)
	}
	return false, s, nil
}

// AwaitSlot waits in the slot queue for a start that builds, within o.Wait:
// its turn comes once fewer waiters than slots are ahead of it and a slot
// is free, and it leaves the queue then without holding the slot, which the
// next waiter takes. One line when the wait starts and one when it ends, on
// o.Stderr. It reports whether the turn came; ahead is how many waited
// ahead of it when the wait ran out.
func AwaitSlot(o Options, what string) (ok bool, ahead int, err error) {
	logf := func(format string, a ...any) { _, _ = fmt.Fprintf(o.Stderr, LogPrefix+format+"\n", a...) }
	if err := os.MkdirAll(o.SlotDir, 0o750); err != nil {
		return false, 0, err
	}
	t, err := enqueue(o.SlotDir, what)
	if err != nil {
		return false, 0, err
	}
	defer t.leave()
	deadline := time.Now().Add(o.Wait)
	t0 := time.Now()
	waited := false
	for {
		turn, reason := t.turn(o.Slots)
		if turn {
			lock, slot, occupied := acquire(o)
			if lock != nil {
				_ = lock.Unlock()
				if waited {
					logf("slot %d free for %s after %ds", slot, what, int(time.Since(t0).Seconds()))
				}
				return true, 0, nil
			}
			reason = fmt.Sprintf("a build slot (all %d held):\n%s%s", o.Slots, holders(o), occupied)
		}
		now := time.Now()
		if !now.Before(deadline) {
			logf("gave up after %s waiting for %s", o.waitText(), reason)
			return false, len(t.ahead()), nil
		}
		if !waited {
			logf("%s waits up to %s for %s", what, o.waitText(), reason)
			waited = true
		}
		time.Sleep(min(deadline.Sub(now), slotPoll))
	}
}

// queuedRun is what a run's ticket says it waits for: the session, the
// process and the command.
func queuedRun(argv []string) string {
	s := "run " + strconv.Itoa(os.Getpid())
	if id := sessionID(); id != "" {
		s += " of session " + id
	}
	return s + ": " + commandHead(argv, 100)
}
