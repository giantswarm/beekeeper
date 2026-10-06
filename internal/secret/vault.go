package secret

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/guard"
)

// The vault session of secret.session lives in one process only: the
// broker on the host (beekeeper sandbox broker), outside every agent
// session. It signs in by itself through secret.signinCommand, when it
// starts and whenever a call waits on the vault, holds the session in
// memory for secret.sessionLifetime, never in a file, a keyring entry or an
// environment an agent reads, and gives it to its own op calls alone. No
// agent command opens or completes the sign-in.

// Locked is the message of a call that waits on the person's unlock.
const Locked = "vault locked: waiting for the broker's sign-in"

// ErrLocked is a vault the person did not unlock in time.
var ErrLocked = errors.New("vault locked")

// Keeper holds the vault session in memory, for its lifetime at most.
type Keeper struct {
	mu       sync.Mutex
	unlocked bool
	env      string // OP_SESSION_<id>=<token>
	since    time.Time
	until    time.Time
	lifetime time.Duration
	// end locks the session when its lifetime passes.
	end *time.Timer
	// changed hears the state after every unlock and lock.
	changed func(VaultState)
	// ready is closed when the vault unlocks and replaced by the next
	// lock, so that every waiting call wakes at once.
	ready chan struct{}
}

// NewKeeper is a locked keeper that holds a session for lifetime and tells
// changed (nil: no one) each new state.
func NewKeeper(lifetime time.Duration, changed func(VaultState)) *Keeper {
	if changed == nil {
		changed = func(VaultState) {}
	}
	return &Keeper{lifetime: lifetime, changed: changed, ready: make(chan struct{})}
}

// Unlock takes the session a sign-in printed, until its lifetime passes.
func (k *Keeper) Unlock(name, token string, now time.Time) error {
	if !sessionName.MatchString(name) || token == "" || strings.ContainsAny(token, "\x00\n") {
		return errors.New("no op session: want OP_SESSION_<id> and its token")
	}
	k.unlock(name+"="+token, now)
	return nil
}

func (k *Keeper) unlock(env string, now time.Time) {
	k.mu.Lock()
	if !k.unlocked {
		close(k.ready)
	}
	k.unlocked, k.env, k.since, k.until = true, env, now, now.Add(k.lifetime)
	if k.end != nil {
		k.end.Stop()
	}
	k.end = time.AfterFunc(k.lifetime, func() { k.lockSession(now) })
	st := k.stateLocked()
	k.mu.Unlock()
	k.changed(st)
}

// Lock forgets the session: the person's lock, or one op no longer takes.
func (k *Keeper) Lock() {
	k.mu.Lock()
	k.lockLocked()
}

// lockSession forgets the session that unlocked at since, at the end of its
// lifetime; a later session stays.
func (k *Keeper) lockSession(since time.Time) {
	k.mu.Lock()
	if !k.since.Equal(since) {
		k.mu.Unlock()
		return
	}
	k.lockLocked()
}

// lockLocked forgets the session with k.mu held, and releases it.
func (k *Keeper) lockLocked() {
	if !k.unlocked {
		k.mu.Unlock()
		return
	}
	k.unlocked, k.env, k.since, k.until = false, "", time.Time{}, time.Time{}
	if k.end != nil {
		k.end.Stop()
	}
	k.ready = make(chan struct{})
	st := k.stateLocked()
	k.mu.Unlock()
	k.changed(st)
}

// State is whether the keeper holds a session, since when and until when.
func (k *Keeper) State() VaultState {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.stateLocked()
}

func (k *Keeper) stateLocked() VaultState {
	return VaultState{Unlocked: k.unlocked, Since: k.since, Until: k.until}
}

// Env is the session's environment entry for op, "" while locked.
func (k *Keeper) Env() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.env
}

// Wait blocks until the keeper holds a session or ctx ends (ErrLocked).
func (k *Keeper) Wait(ctx context.Context) error {
	k.mu.Lock()
	ch := k.ready
	k.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ErrLocked
	}
}

var (
	// sessionName is op's session variable.
	sessionName = regexp.MustCompile(`^OP_SESSION_[A-Za-z0-9_]+$`)
	// signinLine is the line op signin prints for a shell to export.
	signinLine = regexp.MustCompile(`(?m)^\s*(?:export\s+|\$env:)?(OP_SESSION_[A-Za-z0-9_]+)="?([^"\s]+)"?`)
	// expired is op's answer for a session it no longer takes.
	expired = regexp.MustCompile(`(?i)not currently signed in|session expired|invalid session|you are not signed in|authorization prompt dismissed|account is not signed in`)
)

// ParseSignin reads the session op signin printed: its variable's name and
// the token.
func ParseSignin(out []byte) (string, string, error) {
	m := signinLine.FindSubmatch(out)
	if m == nil {
		return "", "", errors.New("op signin printed no session")
	}
	return string(m[1]), string(m[2]), nil
}

// Expired reports whether op's error says the session is gone.
func Expired(stderr string) bool { return expired.MatchString(stderr) }

// NeedsVault reports whether a beekeeper secret call's arguments name the
// shared vault: an op:// reference, a flag's value included.
func NeedsVault(args []string) bool {
	for _, a := range args {
		if _, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(a, "--") {
			a = v
		}
		if strings.HasPrefix(a, guard.OpRef) {
			return true
		}
	}
	return false
}
