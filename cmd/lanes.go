package cmd

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// laneView is one lane's queue with its installation and hold.
type laneView struct {
	merge.Lane
	Installation string      `json:"installation,omitempty"`
	Hold         *state.Hold `json:"hold,omitempty"`
}

func (a *app) lanesCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "lanes",
		Short: "The merge lanes: the running merge, the settling one and who is next",
		Long: `A lane is a set of repositories whose merges roll the same components of an
installation (lanes in the configuration; a repository in no lane is a lane
of its own). The gate the PreToolUse hook puts in front of devctl pr merge
runs one merge per lane at a time, in the order the merges joined, and the
next once the installation's HelmReleases of the lane's charts are Ready and
the previous merge's release has rolled.

A merge run outside the gate (one in flight when the gate went live, one run
without the hook) is registered with lanes settle: it heads its lane until it
merges, and then settles the lane like a gated merge.

Without a subcommand, lists every configured lane and every other lane with
a merge in it: running, settling, and the waiting merges in turn order.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			views := a.laneViews(st)
			if a.json {
				return a.printJSON(views)
			}
			a.printLanes(views)
			return nil
		},
	}
	var forName string
	queue := &cobra.Command{
		Use:   "queue <owner/repo> <n> --for <session>",
		Short: "Queue a merge on a session's behalf, at the end of its lane",
		Long: `queue gives a session's merge its place in the lane now, before the session
runs it, so an agreed order carries over. The place holds until the session's
own devctl pr merge of that repository and number arrives and runs; a hold's
refusal does not lose it. It is kept for merge.seedTTL (12h) from the seeding
or the session's last arrival.`,
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			pr, err := prArgs(args)
			if err != nil {
				return err
			}
			if strings.TrimSpace(forName) == "" {
				return usageErr("--for <session name> is required")
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			lane := a.cfg.LaneOf(args[0]).Name
			pos := 0
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				if pos = merge.Queue(st, lane).Position(args[0], pr); pos > 0 {
					return nil, nil
				}
				st.Merges = append(st.Merges, state.Merge{Repo: args[0], PR: pr, Lane: lane, By: state.Party{Name: forName},
					Phase: state.Waiting, Seeded: true, Joined: a.now.UTC(), Seen: a.now.UTC()})
				pos = -merge.Queue(st, lane).Position(args[0], pr)
				return []state.Event{event(me, "merge.queued", "%s#%d in lane %s for %q", args[0], pr, lane, forName)}, nil
			})
			if err != nil {
				return err
			}
			if pos > 0 {
				_, err = fmt.Fprintf(a.out, "%s#%d is already queued in lane %s at position %d\n", args[0], pr, lane, pos)
				return err
			}
			_, err = fmt.Fprintf(a.out, "queued %s#%d for %q in lane %s at position %d\n", args[0], pr, forName, lane, -pos)
			return err
		},
	}
	queue.Flags().StringVar(&forName, "for", "", "the session whose merge this is")
	drop := &cobra.Command{
		Use:   "drop <owner/repo> <n>",
		Short: "Take a waiting merge out of its lane's queue",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			pr, err := strconv.Atoi(args[1])
			if err != nil {
				return usageErr("%s is not a pull request number", args[1])
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			found := false
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Merges = slices.DeleteFunc(st.Merges, func(m state.Merge) bool {
					hit := m.Repo == args[0] && m.PR == pr && m.Phase == state.Waiting
					found = found || hit
					return hit
				})
				if !found {
					return nil, nil
				}
				return []state.Event{event(me, "merge.dropped", "%s#%d", args[0], pr)}, nil
			})
			if err != nil {
				return err
			}
			if !found {
				_, err = fmt.Fprintf(a.out, "%s#%d is not waiting in any lane\n", args[0], pr)
				return err
			}
			_, err = fmt.Fprintf(a.out, "dropped %s#%d from its lane\n", args[0], pr)
			return err
		},
	}
	c.AddCommand(queue, a.settleCmd(), drop, &cobra.Command{
		Use:   "clear <lane>",
		Short: "Free a lane whose settling merge will not roll",
		Long: `clear drops the lane's settling merge, after its installation was checked
by hand: a release that is not going to roll (a HelmRelease pinned since,
a failed upgrade rolled back) otherwise refuses the lane's next merge once
merge.settleTimeout has passed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			var cleared []string
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Merges = slices.DeleteFunc(st.Merges, func(m state.Merge) bool {
					if m.Lane == args[0] && m.Phase == state.Settling {
						cleared = append(cleared, m.Key())
						return true
					}
					return false
				})
				if len(cleared) == 0 {
					return nil, nil
				}
				return []state.Event{event(me, "lane.clear", "%s: %v", args[0], cleared)}, nil
			})
			if err != nil {
				return err
			}
			if len(cleared) == 0 {
				_, err = fmt.Fprintf(a.out, "nothing settles in lane %s\n", args[0])
				return err
			}
			_, err = fmt.Fprintf(a.out, "cleared lane %s: %v no longer settles\n", args[0], cleared)
			return err
		},
	})
	return c
}

// prArgs reads <owner/repo> <n>.
func prArgs(args []string) (int, error) {
	pr, err := strconv.Atoi(args[1])
	if err != nil || pr < 1 || !strings.Contains(args[0], "/") {
		return 0, usageErr("want <owner/repo> <number>, got %s %s", args[0], args[1])
	}
	return pr, nil
}

func (a *app) settleCmd() *cobra.Command {
	var forName string
	c := &cobra.Command{
		Use:   "settle <owner/repo> <n> [--for <session>]",
		Short: "Register a merge run outside the gate as its lane's head",
		Long: `settle registers a merge the gate did not wrap (one in flight when the gate
went live, one run without the hook) in its lane. Until the pull request is
merged, it heads the lane: its own devctl pr merge passes the gate as the
lane's next merge, and the lane's other merges wait behind it. Once it is
merged, through the gate or outside it, the lane frees by the rule of a gated
merge: the merge's release rolled and the lane's HelmReleases Ready. GitHub
reports a merge outside the gate without its release: the lane then settles
for merge.settle from the merge.

settle asks GitHub for the pull request's state once; while the entry waits
without its merge, a merge waiting behind it asks again at most once a minute.
The entry is kept for merge.seedTTL; lanes drop takes it out.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			pr, err := prArgs(args)
			if err != nil {
				return err
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			by := me
			if strings.TrimSpace(forName) != "" {
				by = state.Party{Name: forName}
			}
			pull, err := github.PullState(cmd.Context(), args[0], pr)
			if err != nil {
				return err
			}
			if pull.State == github.Closed {
				return refused("%s#%d is closed without a merge: nothing settles", args[0], pr)
			}
			lane := a.cfg.LaneOf(args[0]).Name
			busy := ""
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				for _, m := range st.Merges {
					if m.Repo == args[0] && m.PR == pr && m.Phase != state.Waiting {
						busy = m.Phase
						return nil, nil
					}
				}
				i := slices.IndexFunc(st.Merges, func(m state.Merge) bool { return m.Repo == args[0] && m.PR == pr })
				if i < 0 {
					st.Merges = append(st.Merges, state.Merge{Repo: args[0], PR: pr, Lane: lane, By: by, Phase: state.Waiting, Joined: a.now.UTC(), Seen: a.now.UTC()})
					i = len(st.Merges) - 1
				}
				m := &st.Merges[i]
				m.Seeded, m.Outside, m.Checked = true, true, a.now.UTC()
				if strings.TrimSpace(forName) != "" {
					m.By = by
				}
				if pull.State == github.Merged {
					merge.Merged(m, pull.MergedAt)
				}
				return []state.Event{event(me, "merge.settle", "%s outside the gate in lane %s for %q, %s", m.Key(), lane, by.Name, strings.ToLower(pull.State))}, nil
			})
			switch {
			case err != nil:
				return err
			case busy != "":
				return refused("%s#%d is already %s in lane %s", args[0], pr, busy, lane)
			case pull.State == github.Merged:
				_, err = fmt.Fprintf(a.out, "%s#%d merged at %s outside the gate: lane %s settles until %s, then frees once its HelmReleases are Ready\n",
					args[0], pr, clock(a.now, pull.MergedAt), lane, clock(a.now, pull.MergedAt.Add(a.cfg.Merge.Settle.Duration)))
			default:
				_, err = fmt.Fprintf(a.out, "%s#%d heads lane %s for %q until it merges: its devctl pr merge passes the gate, the lane's other merges wait\n",
					args[0], pr, lane, by.Name)
			}
			return err
		},
	}
	c.Flags().StringVar(&forName, "for", "", "the session whose merge this is (default: you)")
	return c
}

// laneViews are the configured lanes, then every other lane with a merge.
func (a *app) laneViews(st *state.State) []laneView {
	var out []laneView
	seen := map[string]bool{}
	add := func(name, installation string) {
		if seen[name] {
			return
		}
		seen[name] = true
		v := laneView{Lane: merge.Queue(st, name), Installation: installation}
		for _, h := range st.Holds {
			if h.Target == merge.LanePrefix+name && h.Active(a.now) {
				v.Hold = &h
			}
		}
		out = append(out, v)
	}
	for _, l := range a.cfg.Lanes {
		add(l.Name, l.Installation)
	}
	for _, m := range st.Merges {
		add(m.Lane, a.cfg.LaneOf(m.Repo).Installation)
	}
	return out
}

// settlingText says what a settling merge waits for.
func (a *app) settlingText(m state.Merge) string {
	switch {
	case m.Outside:
		return fmt.Sprintf("settling %s, merged outside the gate at %s, until %s and its HelmReleases are Ready", m.Key(),
			clock(a.now, m.Finished), clock(a.now, m.Finished.Add(a.cfg.Merge.Settle.Duration)))
	case m.Release == "":
		return fmt.Sprintf("settling %s until an unknown release rolls", m.Key())
	}
	return fmt.Sprintf("settling %s until %s rolls", m.Key(), m.Release)
}

func (a *app) printLanes(views []laneView) {
	if len(views) == 0 {
		_, _ = fmt.Fprintln(a.out, "no lanes are configured and nothing merges")
		return
	}
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format+"\n", args...) }
	for _, v := range views {
		where := ""
		if v.Installation != "" {
			where = " (" + v.Installation + ")"
		}
		status := "free"
		switch {
		case v.Running != nil:
			status = fmt.Sprintf("running %s by %q since %s", v.Running.Key(), v.Running.By.Name, clock(a.now, v.Running.Started))
		case v.Settling != nil:
			status = a.settlingText(*v.Settling)
		}
		p("%s%s: %s", v.Name, where, status)
		also := v.AllSettling
		if v.Running == nil && len(also) > 0 {
			also = also[:len(also)-1]
		}
		for _, m := range also {
			p("  also %s", a.settlingText(*m))
		}
		if v.Hold != nil {
			except := ""
			if v.Hold.Except != "" {
				except = ", except " + v.Hold.Except
			}
			p("  held by %q until %s%s: %s", v.Hold.By.Name, untilText(a, *v.Hold), except, v.Hold.Reason)
		}
		for i, m := range v.Waiting {
			label := "      "
			if i == 0 {
				label = "  next"
			}
			how := "waiting"
			switch {
			case proc.Alive(m.PID):
			case m.Outside:
				how = fmt.Sprintf("settled outside the gate, not merged at %s, heads the lane", clock(a.now, m.Checked))
			default:
				how = "not arrived, queued"
			}
			p("%s %d. %s by %q, %s since %s", label, i+1, m.Key(), m.By.Name, how, clock(a.now, m.Joined))
		}
	}
}
