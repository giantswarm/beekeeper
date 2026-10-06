package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) agentDesktopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "desktop [agent]",
		Short: "Ask for an agent's desktop turn: its import goes ahead past the desktop window's focus",
		Long: `Asks for the desktop turn of an agent beekeeper started, by default the
calling one: a turn that needs the desktop's own tools. The browser does
not: a headless turn of agents start or agents wake has the Claude in
Chrome tools through the CLI's own connection, which never waits on a site
approval, and a desktop turn runs its browser steps through beekeeper
browse. A worker whose headless turn ends with its next step in the
desktop runs it before that turn ends.

The import or reopen that shows the agent in the desktop then does not wait
for the desktop's window to lose the focus: it waits for the person's typing
to pause for desktop.typingQuiet, 1 minute at most, shows the session for a
moment (which warms its desktop CLI) and switches the window back to the
session it showed. A reopen already waiting goes ahead within a second; an
agent whose turn ended with neither a CLI nor a waiting reopen (its import
missed) is shown in the desktop now. The ask holds until the desktop shows
the agent.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			me, err := a.caller()
			if err != nil && len(args) == 0 {
				return err
			}
			var ag state.Agent
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				i := slices.IndexFunc(st.Agents, func(x state.Agent) bool { return x.Is(me) })
				if len(args) > 0 {
					if i, err = findAgent(st, args[0]); err != nil {
						return nil, err
					}
				} else if i < 0 {
					return nil, refused("this session is not registered: `beekeeper agents register` first")
				}
				st.Agents[i].DesktopTurn = a.now.UTC()
				ag = st.Agents[i]
				return []state.Event{event(me, "agents.desktop", "%s asks for a desktop turn", ag.Name)}, nil
			})
			if err != nil {
				return err
			}
			return a.desktopTurn(cmd.Context(), ag)
		},
	}
}

// desktopTurn says how ag, which asked for a desktop turn, gets it, and
// shows it in the desktop at once when nothing else will.
func (a *app) desktopTurn(ctx context.Context, ag state.Agent) error {
	if ag.Session == "" {
		_, err := fmt.Fprintf(a.out, "desktop: %s is no session beekeeper started: open it in the desktop\n", ag.Name)
		return err
	}
	if u := sessionUnits(ctx, ag.Session, false); len(u) > 0 {
		if !a.hasRow(ag.Session) {
			line, err := a.importRow(ctx, ag.Session, ag.Name)
			if err != nil {
				return fmt.Errorf("desktop: %s runs a headless turn (%s) and has no row: %w", ag.Name, u[0], err)
			}
			_, err = fmt.Fprintln(a.out, "desktop: "+line)
			return err
		}
		_, err := fmt.Fprintf(a.out, "desktop: %s runs a headless turn (%s): once it ends, its reopen shows it in the desktop without waiting for the window's focus\n", ag.Name, u[0])
		return err
	}
	if ag.Import.Pending(a.now) {
		_, err := fmt.Fprintf(a.out, "desktop: the waiting reopen of %s shows it in the desktop now\n", ag.Name)
		return err
	}
	sessions, _, err := a.sessions()
	if err != nil {
		return err
	}
	if _, ok := claude.Live(sessions, ag.Party); ok {
		_, err := fmt.Fprintf(a.out, "desktop: %s runs a CLI: its next turn is the desktop's\n", ag.Name)
		return err
	}
	if _, err := fmt.Fprintf(a.out, "desktop: %s runs no CLI and no reopen waits: showing it in the desktop\n", ag.Name); err != nil {
		return err
	}
	return a.reopenSession(ctx, ag.Session)
}

// agentOfSession is the index of the roster entry of session id (or its
// desktop id), -1 when none holds it.
func agentOfSession(st *state.State, id string) int {
	return slices.IndexFunc(st.Agents, func(ag state.Agent) bool {
		return ag.Session == id || ag.HostSession == "local_"+id
	})
}

// asksDesktop reports, each time it is called, whether the agent of
// session id asked for a desktop turn.
func (a *app) asksDesktop(id string) func() bool {
	return func() bool {
		st, err := a.store.Read()
		if err != nil {
			return false
		}
		i := agentOfSession(st, id)
		return i >= 0 && !st.Agents[i].DesktopTurn.IsZero()
	}
}

// importWaits records on the agent of session id what holds its reopen,
// bounded by wait from the first hold: agents, a lease grant, a message by
// name and the watch's IMPORT WAITS read it. The first hold goes to the
// event log too.
func (a *app) importWaits(id, name string, wait time.Duration) func(error) {
	var since time.Time
	return func(held error) {
		first := since.IsZero()
		if first {
			since = time.Now().UTC()
		}
		on := reopenHeldBy(held)
		_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
			if i := agentOfSession(st, id); i >= 0 {
				st.Agents[i].Import = &state.ImportWait{On: on, Since: since, Until: since.Add(wait)}
			}
			if !first {
				return nil, nil
			}
			return []state.Event{event(state.Party{Name: name}, "agent.reopen", "waits for %s, at the latest %s", on, dur(wait))}, nil
		})
	}
}

// importEnded clears the reopen's wait on the agent of session id.
func (a *app) importEnded(id string) {
	_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
		if i := agentOfSession(st, id); i >= 0 {
			st.Agents[i].Import = nil
		}
		return nil, nil
	})
}

// awaitWarmed waits up to twinWait for the desktop's CLI of session id,
// which the show at shownAt warms, and ends the agent's ask for a desktop
// turn once it runs. A desktop at its cap of CLIs warms none for a show (its
// log says so), which the reopen reports and logs as missed: the CLI starts
// once a person opens the session, or one of the desktop's CLIs ended.
func (a *app) awaitWarmed(ctx context.Context, id, name string, shownAt time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, twinWait)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if t, err := plat.Machine.Processes(); err == nil {
			if p := desktopTwin(t, id); p != nil {
				_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
					if i := agentOfSession(st, id); i >= 0 {
						st.Agents[i].DesktopTurn = time.Time{}
					}
					return nil, nil
				})
				_, err := fmt.Fprintf(a.out, "reopen: the desktop runs its CLI (PID %d)\n", p.PID)
				return err
			}
		}
		select {
		case <-ctx.Done():
			if n, ok, _ := claude.DesktopAtCap(a.cfg.Claude.DesktopLog, shownAt); ok {
				return a.reopenMissed(name, fmt.Errorf("the desktop warmed no CLI of local_%s: it runs its cap of %s and starts none for a show; "+
					"one starts once a person opens the session or one of the desktop's CLIs ends (beekeeper free lists the stale ones)", id, capText(n)))
			}
			_, err := fmt.Fprintf(a.out, "reopen: no CLI of local_%s showed within %s\n", id, twinWait)
			return err
		case <-tick.C:
		}
	}
}

// reopenHeldBy is what a reopen held by held waits for.
func reopenHeldBy(held error) string {
	if errors.Is(held, errTyping) {
		return "the person's typing to pause"
	}
	return "the desktop's window to lose the focus"
}

// importStatus says whether a reopen waits to show ag in the desktop.
func importStatus(ag state.Agent, now time.Time) string {
	w := ag.Import
	if !w.Pending(now) {
		return "no import is pending"
	}
	s := fmt.Sprintf("its import is pending: the reopen waits for %s since %s, at the latest until %s", w.On, clock(now, w.Since), clock(now, w.Until))
	if ag.DesktopTurn.IsZero() {
		s += fmt.Sprintf(" (`beekeeper agents desktop %q` imports it now)", ag.Name)
	}
	return s
}

// noCLI says that ag runs no CLI, whether an import is pending, and how it
// gets one when none is.
func noCLI(ag state.Agent, now time.Time) string {
	s := fmt.Sprintf("no CLI of %q runs; %s", ag.Name, importStatus(ag, now))
	if !ag.Import.Pending(now) {
		s += fmt.Sprintf(": `beekeeper agents desktop %q` shows it in the desktop, `beekeeper agents wake %q \"<message>\"` resumes it headless", ag.Name, ag.Name)
	}
	return s
}

// capText is the desktop's cap of CLIs, n of them when its log named it.
func capText(n int) string {
	if n == 0 {
		return "CLIs"
	}
	return fmt.Sprintf("%d CLIs", n)
}
