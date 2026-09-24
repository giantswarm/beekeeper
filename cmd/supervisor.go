package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) supervisorCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "supervisor",
		Short: "Start, stop or show the supervisor session",
		Long: `The supervisor is the one session that watches the others. While its
session runs, leases are claimed only on its grant and merges wait for its
word. When its CLI is gone the rule lifts by itself: nobody is stuck behind
a supervisor that crashed.

Without a subcommand, shows the supervisor (exit 3 when none runs).`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return a.supervisorStatus() },
	}
	var takeOver bool
	start := &cobra.Command{
		Use:   "start",
		Short: "Make the calling session the supervisor",
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
			var msg string
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				if prev := st.Supervisor; prev != nil && !prev.Is(me) {
					if _, live := claude.Live(sessions, prev.Party); live && !takeOver {
						return nil, refused("%q supervises since %s and still runs: take over with --take-over", prev.Name, clock(a.now, prev.Since))
					}
					msg = fmt.Sprintf(" (taking over from %q)", prev.Name)
				}
				st.Supervisor = &state.Supervisor{Party: me, Since: a.now.UTC()}
				msg = fmt.Sprintf("%q supervises now%s", me.Name, msg)
				return []state.Event{event(me, "supervisor.start", "%s", msg)}, nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, msg)
			return err
		},
	}
	start.Flags().BoolVar(&takeOver, "take-over", false, "replace a supervisor whose session still runs (the hand-over)")
	var force bool
	stop := &cobra.Command{
		Use:   "stop",
		Short: "End the watch: grants and merge words no longer gate anyone",
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
				st.Supervisor = nil
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
		Short: "Show the supervisor (exit 3 when none runs)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return a.supervisorStatus() },
	}
	c.AddCommand(start, stop, status)
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
	if st.Supervisor == nil {
		return refused("no supervisor runs")
	}
	_, live := claude.Live(sessions, st.Supervisor.Party)
	if a.json {
		_ = a.printJSON(supervisorView{Supervisor: *st.Supervisor, Live: live})
	}
	if !live {
		return refused("no supervisor runs (%q was recorded at %s; its session is gone)", st.Supervisor.Name, clock(a.now, st.Supervisor.Since))
	}
	if !a.json {
		_, err = fmt.Fprintf(a.out, "%q supervises since %s\n", st.Supervisor.Name, clock(a.now, st.Supervisor.Since))
	}
	return err
}
