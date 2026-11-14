package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// Only beekeeper starts a role's run: the holder's relay, the standby watch
// after a crash, `<role> start --new` for the person and beekeeper resume.
// Each records its session's start with the role, and only such a start, or
// the holder itself after a CLI restart, takes the role.

// startRole is the role a start of an agent with profile role records: a
// relayed role's name, "" for a worker.
func startRole(role string) string {
	for _, rl := range roles {
		if rl.name == role {
			return role
		}
	}
	return ""
}

// roleStartWait bounds how long a background run's `<role> start` waits for
// beekeeper to record its start: the claude daemon chooses its session id,
// and the start is recorded once it returned it.
const roleStartWait = 30 * time.Second

// awaitRoleStart waits up to roleStartWait while rl's open relay names me
// by its run name only and no start of me is recorded yet.
func (a *app) awaitRoleStart(ctx context.Context, rl role, me state.Party) error {
	ctx, cancel := context.WithTimeout(ctx, roleStartWait)
	defer cancel()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		st, err := a.store.Read()
		if err != nil {
			return err
		}
		r := rl.get(st)
		if rl.startedAs(st, me) || !r.Relay.Open(a.now) || r.Relay.To.Session != "" || !takesRelay(r.Relay.To, me) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil // mayTake says why
		case <-tick.C:
		}
	}
}

// goneUnrecorded reports whether r's holder runs no CLI and no record says
// since when: its first read records it, so its grace does not move.
func goneUnrecorded(r state.Role, sessions []*claude.Session) bool {
	if r.Holder == nil {
		return false
	}
	if _, live := claude.Live(sessions, r.Holder.Party); live {
		return false
	}
	return !r.CLI.Of(r.Holder) || r.CLI.Gone.IsZero()
}

// newRun starts rl's next run for by, the person or beekeeper resume: it
// takes the role over from a holder whose session runs no CLI, or takes it
// when none is recorded. A holder whose CLI runs is relieved only with
// takeOver; an open relay to a run that is starting refuses it.
func (a *app) newRun(ctx context.Context, rl role, by state.Party, takeOver bool) (state.Party, string, error) {
	sessions, _, err := a.sessions()
	if err != nil {
		return state.Party{}, "", err
	}
	st, err := a.store.Read()
	if err != nil {
		return state.Party{}, "", err
	}
	if err := a.refuseStopped(st, "starting the next "+rl.title+" run"); err != nil {
		return state.Party{}, "", err
	}
	var from state.Party
	if h := rl.get(st).Holder; h != nil {
		from = h.Party
	}
	check := func(st *state.State) error {
		r := rl.get(st)
		if r.Holder == nil && from != (state.Party{}) || r.Holder != nil && !r.Holder.Is(from) {
			return refused("the %s changed meanwhile: `beekeeper %s status`", rl.name, rl.name)
		}
		if relayPending(st, r, a.now) && r.Relay.Taken.IsZero() {
			return refused("%q is starting already: its `beekeeper %s start` takes the role (the relay is open until %s)",
				r.Relay.To.Name, rl.name, clock(a.now, r.Relay.Expires))
		}
		if r.Holder == nil {
			return nil
		}
		if _, live := claude.Live(sessions, r.Holder.Party); live && !takeOver {
			return refused("%q %s and its CLI runs: its `beekeeper %s relay` starts the next run, or relieve it with --take-over",
				r.Holder.Name, rl.verb, rl.name)
		}
		return nil
	}
	return a.startRun(ctx, rl, from, by, "", check)
}

// runNewRun is `<role> start --new`: the caller, the person or a script,
// has beekeeper start rl's next run.
func (a *app) runNewRun(ctx context.Context, rl role, takeOver bool) error {
	by, err := a.caller()
	if err != nil {
		return err
	}
	to, msg, err := a.newRun(ctx, rl, by, takeOver)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.out, "%s\n%s\n", a.runLine(rl, to), msg)
	return err
}

// runLine says where rl's run to, which beekeeper started, shows.
func (a *app) runLine(rl role, to state.Party) string {
	if to.HostSession == "" {
		short := to.Session[:min(8, len(to.Session))]
		return fmt.Sprintf("started %q (background session %s, Remote Control: claude attach %s): it runs `beekeeper %s start`, then `%s`",
			to.Name, to.Session, short, rl.name, rl.handover)
	}
	return fmt.Sprintf("started %q (session %s, desktop %s): its first turn runs `beekeeper %s start`; the standby watch gives it its desktop turn once that ends",
		to.Name, to.Session, to.HostSession, rl.name)
}

// startRunAfterResume is resume's start of a role's next run (newRun); a
// seam for the tests.
var startRunAfterResume = func(ctx context.Context, a *app, rl role, by state.Party) (state.Party, string, error) {
	return a.newRun(ctx, rl, by, false)
}

// resumeRole starts rl's next run once beekeeper resume lifted the stop,
// when a holder is recorded and its session runs no CLI (a reboot, a
// crash): "" when it runs, its run is starting, or none is recorded (a
// `<role> stop` ended it on purpose).
func (a *app) resumeRole(ctx context.Context, rl role, by state.Party) (string, error) {
	st, err := a.store.Read()
	if err != nil {
		return "", err
	}
	r := rl.get(st)
	if r.Holder == nil {
		return "", nil
	}
	sessions, _, err := a.sessions()
	if err != nil {
		return "", err
	}
	if _, live := claude.Live(sessions, r.Holder.Party); live {
		return "", nil
	}
	if relayPending(st, r, a.now) && r.Relay.Taken.IsZero() {
		return fmt.Sprintf("the %s's next run %q is starting", rl.name, r.Relay.To.Name), nil
	}
	to, _, err := startRunAfterResume(ctx, a, rl, by)
	if err != nil {
		return "", fmt.Errorf("%q %s, its session runs no CLI, and its next run did not start: %w", r.Holder.Name, rl.verb, err)
	}
	return fmt.Sprintf("%q runs no CLI: %s", r.Holder.Name, a.runLine(rl, to)), nil
}
