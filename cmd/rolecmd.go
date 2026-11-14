package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

// relayCmd is rl's relay: its holder starts the next run, which takes the
// role.
func (a *app) relayCmd(rl role, long string) *cobra.Command {
	var cancel bool
	c := &cobra.Command{
		Use:   "relay [--cancel]",
		Short: "Start the next " + rl.Title + " run as a fresh session: its `" + rl.Name + " start` takes the role",
		Long:  long,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			if !cancel {
				return a.relayToSuccessor(cmd.Context(), rl, me)
			}
			sessions, _, err := a.sessions()
			if err != nil {
				return err
			}
			var msg string
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				var evs []state.Event
				var err error
				msg, evs, err = rl.CancelRelay(st, me, a.now)
				if err != nil {
					return nil, err
				}
				_, cli := rl.ObserveCLI(st, sessions, a.now)
				return append(evs, cli...), nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, msg)
			return err
		},
	}
	c.Flags().BoolVar(&cancel, "cancel", false, "withdraw the open relay: you keep "+rl.Ing)
	return c
}

// relayToSuccessor has the holder me start rl's next run (successorDir).
func (a *app) relayToSuccessor(ctx context.Context, rl role, me state.Party) error {
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	if err := rl.MustHold(rl.Get(st), me); err != nil {
		return err
	}
	to, msg, err := a.startSuccessor(ctx, rl, me, me, "")
	if err != nil {
		return err
	}
	if to.HostSession == "" {
		// a background successor's one session takes the role and goes on
		// with it: no desktop turn to hand it
		_, err := fmt.Fprintf(a.out, "started %q (background session %s): it runs `beekeeper %s start`, then `%s`, in its own turn; send it nothing\n%s\n", to.Name, to.Session, rl.Name, rl.Handover, msg)
		return err
	}
	if _, err := fmt.Fprintf(a.out, "started %q (session %s, desktop %s): its first turn runs `beekeeper %s start`\n%s\n", to.Name, to.Session, to.HostSession, rl.Name, msg); err != nil {
		return err
	}
	return a.handTurn(ctx, rl, me, to)
}

// firstTurnWait bounds how long a relay waits for its successor's headless
// first turn to end.
const firstTurnWait = 5 * time.Minute

// handTurn tells the relaying holder me how its successor to gets its
// desktop turn: the holder runs in a desktop CLI, whose session messaging
// starts the successor's desktop CLI at once, whatever the window's focus,
// the person's typing or the desktop's cap. It waits for the successor's
// headless first turn to end (the desktop's CLI must not run beside it) and
// makes room under the desktop's cap first.
func (a *app) handTurn(ctx context.Context, rl role, me, to state.Party) error {
	if err := awaitTurnEnd(ctx, to.Session, firstTurnWait); err != nil {
		return fmt.Errorf("%q: %w: send it nothing, the standby watch gives it its desktop turn once it ends", to.Name, err)
	}
	if err := a.makeRoom(ctx, to.HostSession, me.HostSession); err != nil {
		return fmt.Errorf("%q gets no desktop turn now: %w", to.Name, err)
	}
	turn := rl.ResumeMessage("the desktop runs your CLI now, started by this message: this desktop turn keeps " + rl.Duty)
	_, err := fmt.Fprintf(a.out, "%q's first turn ended. Give it its desktop turn now: call SendMessage once with to %q and the message %q. "+
		"The desktop starts its CLI for it (room is made under its cap); never resume it headless.\n", to.Name, to.HostSession, turn)
	return err
}

// awaitTurnEnd waits up to wait for the headless turn of session id to end.
func awaitTurnEnd(ctx context.Context, id string, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if t, err := plat.Machine.Processes(); err == nil && headlessTurn(t, id) == "" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("its headless first turn still runs after %s", wait)
		case <-tick.C:
		}
	}
}

// runStart makes the calling session rl's holder, or takes the role
// relayed to it.
func (a *app) runStart(ctx context.Context, rl role, takeOver bool) error {
	me, err := a.caller()
	if err != nil {
		return err
	}
	if me.Session == "" {
		return refused("the %s is a Claude Code session: run this inside one, without --as", rl.Name)
	}
	sessions, _, err := a.sessions()
	if err != nil {
		return err
	}
	if err := a.awaitRoleStart(ctx, rl, me); err != nil {
		return err
	}
	var msg string
	var run int
	var waiting []string
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		if err := rl.MayTake(st, me); err != nil {
			return nil, err
		}
		// The recorded holder's CLI is read as the watch reads it: gone
		// since it was first seen gone, never since now.
		_, before := rl.ObserveCLI(st, sessions, a.now)
		// A holder restarting its CLI still holds the role.
		sv := readHolder(rl.Get(st), sessions, a.now, rl.Cfg(a.cfg).RestartGrace.Duration)
		var evs []state.Event
		prev := rl.Get(st).Holder
		msg, evs, err = rl.Take(st, me, sv.Live || sv.Restarting(), takeOver, a.now)
		if err != nil {
			return nil, err
		}
		if prev != nil && !prev.Is(me) {
			handed, err := handClaims(st, lease.Dir(a.cfg.LeaseDir), prev.Party, rl.Get(st).Holder.Party)
			if err != nil {
				return nil, err
			}
			if len(handed) > 0 {
				evs = append(evs, event(me, "lease.handover", "%s", strings.Join(handed, ", ")))
			}
		}
		evs = append(before, evs...)
		// The start titles me with the run's name: never one another live
		// session carries.
		if err := titleTaken(sessions, rl.RunName(rl.Get(st).Run), me); err != nil {
			return nil, err
		}
		run = rl.Get(st).Run
		waiting = takeRoleLines(st, rl)
		_, cli := rl.ObserveCLI(st, sessions, a.now)
		return append(evs, cli...), nil
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(a.out, msg); err != nil {
		return err
	}
	for _, line := range waiting {
		if _, err := fmt.Fprintf(a.out, "waiting for the %s: %s\n", rl.Name, line); err != nil {
			return err
		}
	}
	if rl.Name == guideRole.Name {
		st, _ := a.store.Read()
		for _, line := range a.guideSubscriptionLines(ctx, st, sessions) {
			if _, err := fmt.Fprintf(a.out, "subscription: %s\n", line); err != nil {
				return err
			}
		}
	}
	if line := a.titleRun(ctx, me, rl.RunName(run)); line != "" {
		_, err := fmt.Fprintln(a.out, line)
		return err
	}
	return nil
}

// titleRun has a steward title me's desktop session with its run name
// when it carries another; "" when there is nothing to title. A running
// CLI keeps the name it takes messages under until the desktop runs it
// again.
func (a *app) titleRun(ctx context.Context, me state.Party, name string) string {
	host := desktopID(me)
	title := func() string {
		if r, ok := claude.ReadRecord(a.cfg, host); ok {
			return r.Title
		}
		return ""
	}
	// A session without a desktop record has no title to restore: a
	// background role run must not wait on a steward for one.
	if _, ok := claude.ReadRecord(a.cfg, host); !ok || title() == name {
		return ""
	}
	// Not the caller itself: it is in this turn, so a request to it would
	// wait for the turn to end.
	find := func(_ context.Context, tried []string) (steward, error) {
		return a.findSteward(host, append(tried, host))
	}
	line, err := retitle(ctx, host, name, title, find, a.peerSend, retitleWait)
	if err != nil {
		return fmt.Sprintf("the desktop title stays: %v", err)
	}
	return line + fmt.Sprintf("; peers reach this CLI as %q until the desktop runs it again", me.Name)
}

// runStop ends rl's term on purpose, force for another session's.
func (a *app) runStop(rl role, force bool) error {
	me, err := a.caller()
	if err != nil {
		return err
	}
	var msg string
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		r := rl.Get(st)
		if r.Holder == nil {
			msg = fmt.Sprintf("no %s was recorded", rl.Name)
			return nil, nil
		}
		if !r.Holder.Is(me) && !force {
			return nil, refused("%q %s, not you: --force ends another session's watch", r.Holder.Name, rl.Verb)
		}
		msg = fmt.Sprintf("%q no longer %s", r.Holder.Name, rl.Verb)
		if r.Relay.Open(a.now) {
			msg += fmt.Sprintf("; the relay to %q is cancelled", r.Relay.To.Name)
		}
		r.Holder, r.Relay, r.CLI = nil, nil, nil
		rl.Set(st, r)
		return []state.Event{event(me, rl.Name+".stop", "%s", msg)}, nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.out, msg)
	return err
}

// roleStatus says who holds rl, with its context against relayAt: exit 3
// when nobody does, 4 in the session a relay relieved.
func (a *app) roleStatus(rl role) error {
	if !a.json && !a.cfg.Hooks.GuardsEnabled() {
		_, _ = fmt.Fprintln(a.out, "guards: off (hooks.guards=false)")
	}
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	sessions, _, err := a.sessions()
	if err != nil {
		return err
	}
	r := rl.Get(st)
	if goneUnrecorded(r, sessions) {
		// The grace runs from the first read that saw the CLI gone: with no
		// watch recording it, every read would start it anew.
		if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
			_, evs := rl.ObserveCLI(st, sessions, a.now)
			r = rl.Get(st)
			return evs, nil
		}); err != nil {
			return err
		}
	}
	renameHolder(r.Holder, sessions)
	sv := readHolder(r, sessions, a.now, rl.Cfg(a.cfg).RestartGrace.Duration)
	v := a.viewRole(rl, r, sessions, sv)
	if v == nil {
		v = &roleView{}
	}
	if me, err := a.caller(); err == nil {
		if rf := relievedIn(r, me); rf != nil {
			v.Relieved = true
			if a.json {
				_ = a.printJSON(v)
			}
			now := ""
			switch {
			case r.Holder == nil:
				now = fmt.Sprintf("; no %s is recorded now", rl.Name)
			case !r.Holder.Is(rf.By):
				now = fmt.Sprintf("; %q %s now", r.Holder.Name, rl.Verb)
			}
			return &exitError{code: ExitRelieved, msg: fmt.Sprintf("you have been relieved: %q took the role at %s (relayed at %s)%s",
				rf.By.Name, clock(a.now, rf.Taken), clock(a.now, rf.At), now)}
		}
	}
	if r.Holder == nil {
		return refused("no %s runs", rl.Name)
	}
	if a.json {
		_ = a.printJSON(v)
	}
	if sv.Down() {
		return refused("no %s runs: %q (since %s) is gone since %s; %s",
			rl.Name, r.Holder.Name, clock(a.now, r.Holder.Since), clock(a.now, sv.Gone), rl.Gone)
	}
	if a.json {
		return nil
	}
	restart := ""
	if sv.Restarting() {
		restart = fmt.Sprintf("; its session runs no CLI since %s: it keeps the role if its CLI is back by %s, else it is gone", stamp(sv.Gone), stamp(sv.Until))
	}
	relay := ""
	if r.Relay.Open(a.now) {
		relay = fmt.Sprintf(", relaying to %q until %s", r.Relay.To.Name, clock(a.now, r.Relay.Expires))
	}
	as := ""
	if run := rl.RunName(r.Run); r.Run > 0 && run != r.Holder.Name {
		as = " as " + run
	}
	_, err = fmt.Fprintf(a.out, "%q %s%s since %s%s%s%s\n", r.Holder.Name, rl.Verb, as, clock(a.now, r.Holder.Since), v.contextText(), relay, restart)
	return err
}

// stamp renders t as local "15:04:05", for times seconds apart.
func stamp(t time.Time) string { return t.Local().Format(time.TimeOnly) }
