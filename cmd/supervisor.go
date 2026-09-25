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
		Use:   "supervisor",
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
		RunE: func(*cobra.Command, []string) error { return a.supervisorStatus() },
	}
	var takeOver bool
	start := &cobra.Command{
		Use:   "start",
		Short: "Make the calling session the supervisor, or take the role relayed to it",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			if me.Session == "" {
				return refused("the supervisor is a Claude Code session: run this inside one, without --as")
			}
			sessions, _, err := a.sessions()
			if err != nil {
				return err
			}
			dir := lease.Dir(a.cfg.LeaseDir)
			var msg string
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				holders, err := dir.List()
				if err != nil {
					return nil, err
				}
				lease.Prune(st, heldMap(holders), a.now, a.cfg.GrantTTL.Duration)
				// A supervisor restarting its CLI still holds the role.
				sv := a.supervision(st, sessions)
				var evs []state.Event
				msg, evs, err = startRole(st, me, sv.live || sv.restarting(), takeOver, a.now)
				if err != nil {
					return nil, err
				}
				_, cli := observeCLI(st, sessions, a.now)
				return append(evs, cli...), nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, msg)
			return err
		},
	}
	start.Flags().BoolVar(&takeOver, "take-over", false, "replace a supervisor whose session still runs without its relay")
	var force bool
	stop := &cobra.Command{
		Use:   "stop",
		Short: "End supervision on purpose: grants and merge words no longer gate anyone",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			var msg string
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				if st.Supervisor == nil {
					msg = "no supervisor was recorded"
					return nil, nil
				}
				if !st.Supervisor.Is(me) && !force {
					return nil, refused("%q supervises, not you: --force ends another session's watch", st.Supervisor.Name)
				}
				msg = fmt.Sprintf("%q no longer supervises", st.Supervisor.Name)
				if st.Relay.Open(a.now) {
					msg += fmt.Sprintf("; the relay to %q is cancelled", st.Relay.To.Name)
				}
				st.Supervisor, st.Relay, st.SupervisorCLI = nil, nil, nil
				return []state.Event{event(me, "supervisor.stop", "%s", msg)}, nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, msg)
			return err
		},
	}
	stop.Flags().BoolVar(&force, "force", false, "end the watch of another session")
	status := &cobra.Command{
		Use:   "status",
		Short: "Show the supervisor (exit 3 when none runs, 4 when a relay relieved you)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return a.supervisorStatus() },
	}
	c.AddCommand(start, a.supervisorRelayCmd(), a.supervisorSpareCmd(), a.supervisorReopenCmd(), stop, status)
	return c
}

func (a *app) supervisorRelayCmd() *cobra.Command {
	var cancel bool
	c := &cobra.Command{
		Use:   "relay <successor> | --cancel",
		Short: "Hand the role to a successor: its `supervisor start` takes it and the grants",
		Long: `Name the session that takes over the watch. Its ` + "`beekeeper supervisor start`" + `
takes the role, the grant queue and the pending grants in one step; until
then you stay the supervisor and the grant rule stays yours, so no claim
goes ungated in between. Any other session's start stays refused while you
run. The relay stays open for supervisor.relayTTL (default 15m), then
expires and you simply keep supervising; --cancel withdraws it earlier.

Once the successor has started, ` + "`beekeeper supervisor status`" + ` in your session
exits 4: you have been relieved. The successor is a name, a unique part of
one, a session id or a PID.`,
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
					msg, evs, err = cancelRelay(st, me, a.now)
				} else {
					msg, evs, err = relayRole(st, me, to, a.now, a.cfg.Supervisor.RelayTTL.Duration)
				}
				if err != nil {
					return nil, err
				}
				_, cli := observeCLI(st, sessions, a.now)
				return append(evs, cli...), nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, msg)
			return err
		},
	}
	c.Flags().BoolVar(&cancel, "cancel", false, "withdraw the open relay: you keep supervising")
	return c
}

func (a *app) supervisorStatus() error {
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	sessions, _, err := a.sessions()
	if err != nil {
		return err
	}
	sv := a.supervision(st, sessions)
	v := a.viewSupervisor(st, sessions, sv)
	if v == nil {
		v = &supervisorView{}
	}
	if me, err := a.caller(); err == nil {
		if r := relievedBy(st, me); r != nil {
			v.Relieved = true
			if a.json {
				_ = a.printJSON(v)
			}
			now := ""
			switch {
			case st.Supervisor == nil:
				now = "; no supervisor is recorded now"
			case !st.Supervisor.Is(r.By):
				now = fmt.Sprintf("; %q supervises now", st.Supervisor.Name)
			}
			return &exitError{code: ExitRelieved, msg: fmt.Sprintf("you have been relieved: %q took the role at %s (relayed at %s)%s",
				r.By.Name, clock(a.now, r.Taken), clock(a.now, r.At), now)}
		}
	}
	if st.Supervisor == nil {
		return refused("no supervisor runs")
	}
	if a.json {
		_ = a.printJSON(v)
	}
	if sv.down() {
		return refused("no supervisor runs: %q (since %s) is gone since %s; claims wait for a successor's `beekeeper supervisor start`",
			st.Supervisor.Name, clock(a.now, st.Supervisor.Since), clock(a.now, sv.gone))
	}
	if a.json {
		return nil
	}
	restart := ""
	if sv.restarting() {
		restart = fmt.Sprintf("; its CLI is restarting (gone since %s, grace until %s)", stamp(sv.gone), stamp(sv.until))
	}
	relay := ""
	if st.Relay.Open(a.now) {
		relay = fmt.Sprintf(", relaying to %q until %s", st.Relay.To.Name, clock(a.now, st.Relay.Expires))
	}
	spare := ""
	if st.Spare != nil {
		spare = fmt.Sprintf(", spare %q", st.Spare.Name)
	}
	_, err = fmt.Fprintf(a.out, "%q supervises since %s%s%s%s%s\n", st.Supervisor.Name, clock(a.now, st.Supervisor.Since), v.contextText(), relay, spare, restart)
	return err
}

// stamp renders t as local "15:04:05", for times seconds apart.
func stamp(t time.Time) string { return t.Local().Format(time.TimeOnly) }
