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
// session. It holds the session in memory, never in a file, a keyring entry
// or an environment an agent reads, and gives it to its own op calls alone.
// The person hands it over with beekeeper secret unlock in their own
// terminal; no agent command opens or completes the unlock.

// Locked is the message of a call that waits on the person's unlock.
const Locked = "vault locked: waiting for the person's approval (they unlock it with `beekeeper secret unlock` in their own terminal)"

// ErrLocked is a vault the person did not unlock in time.
var ErrLocked = errors.New("vault locked")

// Keeper holds the vault session in memory.
type Keeper struct {
	mu    sync.Mutex
	env   string // OP_SESSION_<id>=<token>
	since time.Time
	// unlocked is closed when a session arrives and replaced by the next
	// lock, so that every waiting call wakes at once.
	unlocked chan struct{}
}

// NewKeeper is a locked keeper.
func NewKeeper() *Keeper { return &Keeper{unlocked: make(chan struct{})} }

// Unlock takes the session the person's sign-in answered.
func (k *Keeper) Unlock(name, token string, now time.Time) error {
	if !sessionName.MatchString(name) || token == "" || strings.ContainsAny(token, "\x00\n") {
		return errors.New("no op session: want OP_SESSION_<id> and its token")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	wasLocked := k.env == ""
	k.env, k.since = name+"="+token, now
	if wasLocked {
		close(k.unlocked)
	}
	return nil
}

// Lock forgets the session: the person's lock, or one op no longer takes.
func (k *Keeper) Lock() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.env != "" {
		k.env, k.since = "", time.Time{}
		k.unlocked = make(chan struct{})
	}
}

// Status is whether the keeper holds a session, and since when.
func (k *Keeper) Status() (bool, time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.env != "", k.since
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
	ch := k.unlocked
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
