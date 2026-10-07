package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/state"
)

// reopenPoll is how often a waiting reopen reads the roster: whether its
// agent asked for a desktop turn, and whether it still has a reason to show
// the session. Each read parses the whole state, the bulk of a waiting
// reopen's CPU. Tests shorten it.
var reopenPoll = 15 * time.Second

const (
	// reopenTwinPoll is how often a waiting reopen looks for a desktop CLI
	// of its session that started meanwhile; a multiple of reopenPoll.
	reopenTwinPoll = 30 * time.Second
	// lockPoll is how often a wait for the desktop asks whether the screen
	// is locked, which reads the whole process table.
	lockPoll = 15 * time.Second
)

// reopenEnd ends a waiting reopen that has nothing left to show: why says
// what made it so; twin, that a desktop CLI of the session runs already.
type reopenEnd struct {
	why  string
	twin bool
}

func (e reopenEnd) Error() string { return e.why }

// holdReopen takes session id's reopen lock, which one reopen holds while
// it waits and shows the session: one waits per session, however often the
// session's turns end meanwhile. waits is true when another reopen holds
// it; release gives it back.
func (a *app) holdReopen(id string) (release func(), waits bool, err error) {
	dir := filepath.Join(a.cfg.StateDir, "reopen")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, err
	}
	l := flock.New(filepath.Join(dir, id+".lock"))
	ok, err := l.TryLock()
	if err != nil {
		return nil, false, fmt.Errorf("taking the reopen lock of %s: %w", id, err)
	}
	if !ok {
		return nil, true, nil
	}
	return func() { _ = l.Unlock() }, false, nil
}

// watchReopen watches the roster for the reopen of session id (arg as its
// unit named it) every reopenPoll, and the process table every
// reopenTwinPoll, until ctx ends or stop is called. The context it returns
// ends with a reopenEnd as its cause once the agent reported its work done,
// left the roster or is a relieved role run, or a desktop CLI of the
// session runs. urgent reports whether the agent asked for a desktop turn.
func (a *app) watchReopen(ctx context.Context, arg, id string) (wctx context.Context, urgent func() bool, stop func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	var asks atomic.Bool
	ended := func(twin bool) bool {
		why, turn := a.reopenGone(arg, id)
		asks.Store(turn)
		if why == "" && twin {
			if t, err := plat.Machine.Processes(); err == nil {
				if p := desktopTwin(t, id); p != nil {
					cancel(reopenEnd{why: fmt.Sprintf("the desktop runs its CLI (PID %d)", p.PID), twin: true})
					return true
				}
			}
		}
		if why != "" {
			cancel(reopenEnd{why: why})
		}
		return why != ""
	}
	if !ended(true) {
		poll := reopenPoll
		go func() {
			tick := time.NewTicker(poll)
			defer tick.Stop()
			for n := 1; ; n++ {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
				if ended(n%int(reopenTwinPoll/poll) == 0) {
					return
				}
			}
		}()
	}
	return ctx, asks.Load, func() { cancel(nil) }
}

// reopenGone says why the reopen of session id has nothing left to show,
// empty while it has, and whether its agent asked for a desktop turn. It
// peeks at the state, never waiting on a writer.
func (a *app) reopenGone(arg, id string) (why string, urgent bool) {
	st, err := a.store.Peek()
	if err != nil {
		return "", false
	}
	name, ok := reopens(st, arg)
	if !ok {
		return "it left the roster", false
	}
	if pastRun(st, state.Party{Session: id, HostSession: "local_" + id, Name: name}, time.Now()) {
		return "it is a relieved role run", false
	}
	i := agentOfSession(st, id)
	if i < 0 {
		return "", false
	}
	if st.Agents[i].Done {
		return "it reported its work done", false
	}
	return "", !st.Agents[i].DesktopTurn.IsZero()
}

// every answers f's last answer until d passed since f was asked: a check
// too costly to make on every poll. Not safe for concurrent use.
func every(d time.Duration, f func() bool) func() bool {
	var at time.Time
	var v bool
	return func() bool {
		if now := time.Now(); at.IsZero() || now.Sub(at) >= d {
			v, at = f(), now
		}
		return v
	}
}
