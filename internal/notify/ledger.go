package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// Delivery is one notification: sent (with the id the service gave it),
// failed, or held for the end of the quiet hours.
type Delivery struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Summary string    `json:"summary"`
	Body    string    `json:"body,omitempty"`
	Urgency string    `json:"urgency"`
	ID      uint32    `json:"id,omitempty"`
	Held    bool      `json:"held,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// ledger is notify.json: the claimed events, the held ones and the last
// deliveries.
type ledger struct {
	// Sent is when each event (kind:key) or lasting kind was claimed.
	Sent map[string]time.Time `json:"sent"`
	// Held wait for the end of the quiet hours.
	Held []Delivery `json:"held,omitempty"`
	// Log is the last deliveries, newest last.
	Log []Delivery `json:"log,omitempty"`
}

// keepLog is how many deliveries the log keeps; keepSent how long a claim
// is kept, longer than any event (a stale lease, a gone supervisor) is
// seen again by a watch that restarts.
const (
	keepLog  = 20
	keepSent = 30 * 24 * time.Hour
)

func (l *ledger) log(d Delivery) {
	l.Log = append(l.Log, d)
	if len(l.Log) > keepLog {
		l.Log = l.Log[len(l.Log)-keepLog:]
	}
}

// Ledger keeps notify.json in beekeeper's state directory, changed only
// under notify.lock.
type Ledger struct {
	dir  string
	lock *flock.Flock
}

// NewLedger returns the ledger kept in dir.
func NewLedger(dir string) *Ledger {
	return &Ledger{dir: dir, lock: flock.New(filepath.Join(dir, "notify.lock"))}
}

// Path is the ledger file.
func (l *Ledger) Path() string { return filepath.Join(l.dir, "notify.json") }

// lockTimeout bounds the wait for another watch's change of the ledger.
const lockTimeout = 10 * time.Second

// update changes the ledger under its lock with one rename.
func (l *Ledger) update(ctx context.Context, fn func(*ledger)) error {
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, lockTimeout)
	defer cancel()
	ok, err := l.lock.TryLockContext(ctx, 20*time.Millisecond)
	if err != nil || !ok {
		return fmt.Errorf("%s: %w", l.lock.Path(), errors.Join(err, ctx.Err()))
	}
	defer func() { _ = l.lock.Unlock() }()
	st, err := l.load()
	if err != nil {
		return err
	}
	fn(st)
	for k, t := range st.Sent {
		if time.Since(t) > keepSent {
			delete(st.Sent, k)
		}
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.Path() + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.Path())
}

// load reads the ledger; a missing file is an empty one.
func (l *Ledger) load() (*ledger, error) {
	st := &ledger{}
	raw, err := os.ReadFile(l.Path())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, st); err != nil {
			return nil, fmt.Errorf("%s: %w", l.Path(), err)
		}
	}
	if st.Sent == nil {
		st.Sent = map[string]time.Time{}
	}
	return st, nil
}
