package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) supervisorCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   supervisorRole.name,
		Short: "Start, stop or show the supervisor session",
		Long: `The supervisor is the one session that watches the others. From its
start, leases are claimed only on its grant and merges wait for its word,
until a deliberate ` + "`beekeeper supervisor stop`" + ` or a successor's start: a
supervisor whose CLI crashed keeps the rule in force, its grants and queue
stay recorded, and every claim waits for the successor. People and scripts
(--as) stay ungated. A CLI back under the same session within
supervisor.restartGrace (30s) of beekeeper first seeing it gone is a restart
and keeps the role. Past the grace, with no relay open, the watch says
SUPERVISOR GONE and notifies (no-supervisor, critical) until a supervisor
is back. The role moves to a successor in two steps that leave no gap: the
supervisor names it (relay), the successor starts.

Without a subcommand, shows the supervisor and its context in tokens
against supervisor.relayAt, at which watch says RELAY DUE (exit 3 when none
runs, 4 in the session a relay relieved).`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return a.roleStatus(supervisorRole) },
	}
	var takeOver bool
	start := &cobra.Command{
		Use:   "start",
		Short: "Make the calling session the supervisor, or take the role relayed to it",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return a.runStart(supervisorRole, takeOver) },
	}
	start.Flags().BoolVar(&takeOver, "take-over", false, "replace a supervisor whose session still runs without its relay")
	var force bool
	stop := &cobra.Command{
		Use:   "stop",
		Short: "End supervision on purpose: grants and merge words no longer gate anyone",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return a.runStop(supervisorRole, force) },
	}
	stop.Flags().BoolVar(&force, "force", false, "end the watch of another session")
	status := &cobra.Command{
		Use:   "status",
		Short: "Show the supervisor (exit 3 when none runs, 4 when a relay relieved you)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return a.roleStatus(supervisorRole) },
	}
	c.AddCommand(start, a.relayCmd(supervisorRole, supervisorRelayLong), a.supervisorSpareCmd(), a.supervisorReopenCmd(), stop, status)
	return c
}

// supervisorRelayLong is the supervisor relay's help.
const supervisorRelayLong = `Name the session that takes over the watch. Its ` + "`beekeeper supervisor start`" + `
takes the role, the grant queue and the pending grants in one step; until
then you stay the supervisor and the grant rule stays yours, so no claim
goes ungated in between. Any other session's start stays refused while you
run. The relay stays open for supervisor.relayTTL (default 15m), then
expires and you simply keep supervising; --cancel withdraws it earlier.

Once the successor has started, ` + "`beekeeper supervisor status`" + ` in your session
exits 4: you have been relieved. The successor is a name, a unique part of
one, a session id or a PID.`

// relayCmd is rl's relay: its holder names the successor.
func (a *app) relayCmd(rl role, long string) *cobra.Command {
	var cancel bool
	grants := ""
	if rl.grants {
		grants = " and the grants"
	}
	c := &cobra.Command{
		Use:   "relay <successor> | --cancel",
		Short: "Hand the role to a successor: its `" + rl.name + " start` takes it" + grants,
		Long:  long,
		Args: func(_ *cobra.Command, args []string) error {
			if cancel != (len(args) == 0) {
				return usageErr("name the successor, or --cancel without one")
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			sessions, _, err := a.sessions()
			if err != nil {
				return err
			}
			var to state.Party
			if !cancel {
				s, err := claude.Resolve(sessions, args[0])
				if err != nil {
					return usageErr("%v", err)
				}
				to = s.Party()
			}
			var msg string
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				var evs []state.Event
				var err error
				if cancel {
					msg, evs, err = rl.cancelRelay(st, me, a.now)
				} else {
					msg, evs, err = rl.relay(st, me, to, a.now, rl.cfg(a.cfg).RelayTTL.Duration)
				}
				if err != nil {
					return nil, err
				}
				_, cli := rl.observeCLI(st, sessions, a.now)
				return append(evs, cli...), nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, msg)
			return err
		},
	}
	c.Flags().BoolVar(&cancel, "cancel", false, "withdraw the open relay: you keep "+rl.ing)
	return c
}

// runStart makes the calling session rl's holder, or takes the role
// relayed to it.
func (a *app) runStart(rl role, takeOver bool) error {
	me, err := a.caller()
	if err != nil {
		return err
	}
	if me.Session == "" {
		return refused("the %s is a Claude Code session: run this inside one, without --as", rl.name)
	}
	sessions, _, err := a.sessions()
	if err != nil {
		return err
	}
	dir := lease.Dir(a.cfg.LeaseDir)
	var msg string
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		if rl.grants {
			holders, err := dir.List()
			if err != nil {
				return nil, err
			}
			lease.Prune(st, heldMap(holders), a.now, a.cfg.GrantTTL.Duration)
		}
		// A holder restarting its CLI still holds the role.
		sv := readHolder(rl.get(st), sessions, a.now, rl.cfg(a.cfg).RestartGrace.Duration)
		var evs []state.Event
		msg, evs, err = rl.start(st, me, sv.live || sv.restarting(), takeOver, a.now)
		if err != nil {
			return nil, err
		}
		_, cli := rl.observeCLI(st, sessions, a.now)
		return append(evs, cli...), nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.out, msg)
	return err
}

// runStop ends rl's term on purpose, force for another session's.
func (a *app) runStop(rl role, force bool) error {
	me, err := a.caller()
	if err != nil {
		return err
	}
	var msg string
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		r := rl.get(st)
		if r.Holder == nil {
			msg = fmt.Sprintf("no %s was recorded", rl.name)
			return nil, nil
		}
		if !r.Holder.Is(me) && !force {
			return nil, refused("%q %s, not you: --force ends another session's watch", r.Holder.Name, rl.verb)
		}
		msg = fmt.Sprintf("%q no longer %s", r.Holder.Name, rl.verb)
		if r.Relay.Open(a.now) {
			msg += fmt.Sprintf("; the relay to %q is cancelled", r.Relay.To.Name)
		}
		r.Holder, r.Relay, r.CLI = nil, nil, nil
		rl.set(st, r)
		return []state.Event{event(me, rl.name+".stop", "%s", msg)}, nil
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
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	sessions, _, err := a.sessions()
	if err != nil {
		return err
	}
	r := rl.get(st)
	renameHolder(r.Holder, sessions)
	sv := readHolder(r, sessions, a.now, rl.cfg(a.cfg).RestartGrace.Duration)
	v := a.viewRole(rl, r, sessions, sv)
	if v == nil {
		v = &supervisorView{}
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
				now = fmt.Sprintf("; no %s is recorded now", rl.name)
			case !r.Holder.Is(rf.By):
				now = fmt.Sprintf("; %q %s now", r.Holder.Name, rl.verb)
			}
			return &exitError{code: ExitRelieved, msg: fmt.Sprintf("you have been relieved: %q took the role at %s (relayed at %s)%s",
				rf.By.Name, clock(a.now, rf.Taken), clock(a.now, rf.At), now)}
		}
	}
	if r.Holder == nil {
		return refused("no %s runs", rl.name)
	}
	if a.json {
		_ = a.printJSON(v)
	}
	if sv.down() {
		return refused("no %s runs: %q (since %s) is gone since %s; %s",
			rl.name, r.Holder.Name, clock(a.now, r.Holder.Since), clock(a.now, sv.gone), rl.gone)
	}
	if a.json {
		return nil
	}
	restart := ""
	if sv.restarting() {
		restart = fmt.Sprintf("; its CLI is restarting (gone since %s, grace until %s)", stamp(sv.gone), stamp(sv.until))
	}
	relay := ""
	if r.Relay.Open(a.now) {
		relay = fmt.Sprintf(", relaying to %q until %s", r.Relay.To.Name, clock(a.now, r.Relay.Expires))
	}
	spare := ""
	if st.Spare != nil && rl.name == supervisorRole.name {
		spare = fmt.Sprintf(", spare %q", st.Spare.Name)
	}
	_, err = fmt.Fprintf(a.out, "%q %s since %s%s%s%s%s\n", r.Holder.Name, rl.verb, clock(a.now, r.Holder.Since), v.contextText(), relay, spare, restart)
	return err
}

// stamp renders t as local "15:04:05", for times seconds apart.
func stamp(t time.Time) string { return t.Local().Format(time.TimeOnly) }
