package cmd

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/upgrade"
)

// laneView is one lane's queue with its installation and hold.
type laneView struct {
	merge.Lane
	Installation string      `json:"installation,omitempty"`
	Hold         *state.Hold `json:"hold,omitempty"`
	// Stall is set when the lane's first arrived merge has waited longer
	// than merge.stallAfter behind places whose merges are not in the gate.
	Stall *merge.Stall `json:"stall,omitempty"`
}

func (a *app) lanesCmd() *cobra.Command {
	var full bool
	c := &cobra.Command{
		Use:   "lanes",
		Short: "The merge lanes: the running merge, the settling one and who is next",
		Long: `A lane is a set of repositories whose merges roll the same components of an
installation (lanes in the configuration; a repository in no lane is a lane
of its own). The gate the PreToolUse hook puts in front of devctl pr merge
runs one merge per lane at a time, in the order the merges joined, and the
next once the installation's HelmReleases of the lane's charts are Ready and
the previous merge's release has rolled; watch drops a settling merge once
that holds, and a lane with no installation has nothing to settle. The lane
never idles for a merge
that is not there: an arrived merge runs ahead of a seeded place whose merge
has not arrived (seeds keep their order among themselves), and a merge that
ended with nothing merged keeps its place, "retrying", so its session's
retry runs before the merges behind it; it holds the lane for
merge.queueTTL after the failure and keeps its place for merge.seedTTL.

A merge run outside the gate (one in flight when the gate went live, one run
without the hook) is registered with lanes settle: it heads its lane until it
merges, and then settles the lane like a gated merge.

A lane is stalled when no merge runs and its first arrived merge (its gate
call waiting now) has waited longer than merge.stallAfter (5m) behind places
whose merges are not in the gate: seeds that have not arrived, merges that
left it. lanes names the waiting merge and those places, and watch says it
as LANE STALLED; lanes drop takes out a place that will not arrive.

Without a subcommand, lists every configured lane and every other lane with
a merge in it: running, settling, and the waiting merges in turn order. A
caller that has read the lanes before gets only the lines that changed
since, or one "no change" line; --full prints everything.`,
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
			return a.delta("lanes", full, textFacts("lane", a.capture(func() { a.printLanes(views) })),
				func() { a.printLanes(views) })
		},
	}
	fullFlag(c, &full)
	var forName string
	queue := &cobra.Command{
		Use:   "queue <owner/repo> <n> --for <session>",
		Short: "Queue a merge on a session's behalf, at the end of its lane",
		Long: `queue gives a session's merge its place in the lane now, before the session
runs it, so an agreed order carries over. The place holds until the session's
own devctl pr merge of that repository and number arrives and runs; a hold's
refusal does not lose it. It is kept for merge.seedTTL (12h) from the seeding
or the session's last arrival. Seeds keep their order among themselves, but a
seed whose merge has not arrived holds up no other merge in a free lane: not
an arrived unseeded merge, not an earlier pull request of its own session.`,
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
		Use:   "drop <owner/repo> <n>|promote",
		Short: "Take a waiting merge or promotion out of its lane, or end a running merge whose pull request merged",
		Long: `drop takes a waiting merge out of its lane's queue, or with promote in place
of the number the repository's waiting devctl release promote. A gate that
waits for the place, a queued run of its own included, refuses (exit 77)
and wakes its owner: nothing runs. A running merge whose
pull request GitHub reports merged or closed while its devctl runs on (hung
after the merge) has its devctl ended; its outcome is recorded as when its
gate ends it: a merge settles its lane by the settle rule, its release
unconfirmed. A running merge whose pull request is open is refused: devctl
merges it on. The watch ends such a hung run by itself merge.hungAfter (45m)
after the merge.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			pr, err := dropArg(args[1])
			if err != nil {
				return err
			}
			key := state.Merge{Repo: args[0], PR: pr}.Key()
			me, err := a.caller()
			if err != nil {
				return err
			}
			found, running := false, false
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				running = slices.ContainsFunc(st.Merges, func(m state.Merge) bool {
					return m.Repo == args[0] && m.PR == pr && m.Phase == state.Running
				})
				st.Merges = slices.DeleteFunc(st.Merges, func(m state.Merge) bool {
					hit := m.Repo == args[0] && m.PR == pr && m.Phase == state.Waiting
					found = found || hit
					return hit
				})
				if !found {
					return nil, nil
				}
				return []state.Event{event(me, "merge.dropped", "%s", key)}, nil
			})
			switch {
			case err != nil:
				return err
			case found:
				_, err = fmt.Fprintf(a.out, "dropped %s from its lane\n", key)
				return err
			case running && pr == 0:
				return refused("%s is running: devctl dispatched it or is about to, a promotion has no pull request to end it by", key)
			case running:
				return a.dropRunning(cmd.Context(), me, args[0], pr)
			}
			_, err = fmt.Fprintf(a.out, "%s is not waiting in any lane\n", key)
			return err
		},
	}
	c.AddCommand(queue, a.settleCmd(), a.urgentCmd(), drop, a.centralLanesCmd(), a.laneLeaveCmd(), &cobra.Command{
		Use:   "clear <lane>",
		Short: "Free a lane whose settling merge will not roll",
		Long: `clear drops the lane's settling merge, after its installation was checked
by hand: a release that is not going to roll (a HelmRelease pinned since,
a failed upgrade rolled back) otherwise refuses the lane's next merge once
merge.settleTimeout has passed. A running merge whose gate process is gone
counts as settling: watch settles it too, with a MERGE LOST line.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			var cleared []string
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				merge.Lost(st, a.now, proc.Alive)
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

// promoteArg names a promotion's place in lanes drop.
const promoteArg = "promote"

// dropArg reads lanes drop's place: a pull request number, or promote for
// the repository's promotion (0).
func dropArg(arg string) (int, error) {
	if arg == promoteArg {
		return 0, nil
	}
	pr, err := strconv.Atoi(arg)
	if err != nil || pr < 1 {
		return 0, usageErr("%s is neither a pull request number nor promote", arg)
	}
	return pr, nil
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
		if s, ok := v.Stalled(a.now, a.cfg.Merge.StallAfter.Duration, a.present, proc.Alive, arrived); ok {
			v.Stall = &s
		}
		for _, h := range st.Holds {
			if h.Target == merge.LanePrefix+name && h.Active(a.now) {
				v.Hold = &h
			}
		}
		if h, ok := upgrade.Held(st, installation, a.now); ok && v.Hold == nil {
			v.Hold = &h
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

// present says whether a waiting merge holds its place against the merges
// behind it.
func (a *app) present(m state.Merge) bool {
	return merge.Present(m, a.now, a.cfg.Merge.QueueTTL.Duration, proc.Alive)
}

// arrived is when a gate call's process started, false when it is gone.
func arrived(pid int) (time.Time, bool) {
	t, err := plat.Machine.Started(pid)
	return t, err == nil
}

// stallText says who a stalled lane's arrived merge waits behind and how to
// free it.
func (a *app) stallText(s merge.Stall) string {
	behind := make([]string, 0, len(s.Behind))
	for _, m := range s.Behind {
		how := "not arrived"
		if m.PID != 0 {
			how = "left the gate at " + clock(a.now, m.Seen)
		}
		behind = append(behind, fmt.Sprintf("%s (%q, %s)", m.Key(), m.By.Name, how))
	}
	return fmt.Sprintf("stalled for %s: %s (%q) waits in the gate behind %s; drop the places that will not arrive (beekeeper lanes drop <owner/repo> <n>|promote)",
		a.now.Sub(s.Since).Round(time.Second), s.Merge.Key(), s.Merge.By.Name, strings.Join(behind, ", "))
}

// settlingText says what a settling merge waits for, and since when it is
// stuck past merge.settleTimeout.
func (a *app) settlingText(m state.Merge) string {
	var text string
	switch {
	case m.Outside:
		text = fmt.Sprintf("settling %s, merged outside the gate at %s, until %s and its HelmReleases are Ready", m.Key(),
			clock(a.now, m.Finished), clock(a.now, m.Finished.Add(a.cfg.Merge.Settle.Duration)))
	case m.Release == "":
		text = fmt.Sprintf("settling %s until an unknown release rolls", m.Key())
	default:
		text = fmt.Sprintf("settling %s until %s rolls", m.Key(), m.Release)
	}
	if stuck := m.Finished.Add(a.cfg.Merge.SettleTimeout.Duration); a.now.After(stuck) {
		text += fmt.Sprintf(", stuck since %s (the watch's LANE STUCK line says why)", clock(a.now, stuck))
	}
	return text
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
			status = fmt.Sprintf("running %s by %s since %s", v.Running.Key(), quotedOwner(v.Running.By), clock(a.now, v.Running.Started))
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
			p("  held by %s until %s%s: %s", quotedOwner(v.Hold.By), untilText(a, *v.Hold), except, v.Hold.Reason)
		}
		if v.Stall != nil {
			p("  %s", a.stallText(*v.Stall))
		}
		present := a.present
		next := max(slices.IndexFunc(v.Waiting, func(m state.Merge) bool {
			_, behind := v.Ahead(m.Repo, m.PR, present)
			return present(m) && !behind
		}), 0)
		for i, m := range v.Waiting {
			label := "      "
			if i == next {
				label = "  next"
			}
			how := "waiting"
			switch {
			case proc.Alive(m.PID):
			case m.Outside:
				how = fmt.Sprintf("settled outside the gate, not merged at %s, heads the lane", clock(a.now, m.Checked))
			case m.Retrying() && present(m):
				how = fmt.Sprintf("retrying after exit %d at %s (holds the lane until %s), queued", m.Exit, clock(a.now, m.Finished),
					clock(a.now, m.Seen.Add(a.cfg.Merge.QueueTTL.Duration)))
			case m.Retrying():
				how = fmt.Sprintf("retrying after exit %d at %s (not back, arrived merges pass it), queued", m.Exit, clock(a.now, m.Finished))
			case m.PID != 0 && present(m):
				how = fmt.Sprintf("left the gate at %s (its place is kept until %s), queued", clock(a.now, m.Seen), clock(a.now, m.Seen.Add(a.cfg.Merge.QueueTTL.Duration)))
			case m.PID != 0:
				how = fmt.Sprintf("left the gate at %s (arrived merges pass it), queued", clock(a.now, m.Seen))
			default:
				how = "not arrived, queued"
			}
			p("%s %d. %s by %s, %s since %s", label, i+1, m.Key(), quotedOwner(m.By), how, clock(a.now, m.Joined))
		}
	}
}

// checkPlaces asks GitHub, at now, about the waiting places of lane (every lane for
// "") whose merge is not in the gate, other than repo#pr, last checked more
// than every ago: a merged pull request settles its lane as a merge outside
// the gate, a closed one leaves it, each with an event naming it. It
// returns GitHub's error for a place settled with lanes settle, which heads
// its lane; other places are asked again at the next check.
func (a *app) checkPlaces(ctx context.Context, by state.Party, now time.Time, lane, repo string, pr int, every time.Duration) error {
	st, err := a.store.Read()
	if err != nil {
		return nil
	}
	pulls := map[string]github.Pull{}
	for _, m := range st.Merges {
		if m.Phase != state.Waiting || m.PR == 0 || proc.Alive(m.PID) || (lane != "" && m.Lane != lane) ||
			(m.Repo == repo && m.PR == pr) || now.Sub(m.Checked) < every {
			continue
		}
		p, err := pullState(ctx, m.Repo, m.PR)
		if err != nil {
			if m.Outside {
				return err
			}
			continue
		}
		pulls[m.Key()] = p
	}
	if len(pulls) == 0 {
		return nil
	}
	return a.store.Update(func(st *state.State) ([]state.Event, error) {
		var ev []state.Event
		st.Merges = slices.DeleteFunc(st.Merges, func(m state.Merge) bool {
			p, ok := pulls[m.Key()]
			if !ok || m.Phase != state.Waiting || proc.Alive(m.PID) || p.State != github.Closed {
				return false
			}
			ev = append(ev, event(by, "merge.dropped", "%s: closed without a merge, its place in lane %s is dropped", m.Key(), m.Lane))
			return true
		})
		for i := range st.Merges {
			m := &st.Merges[i]
			p, ok := pulls[m.Key()]
			if !ok || m.Phase != state.Waiting || proc.Alive(m.PID) {
				continue
			}
			if p.State != github.Merged {
				m.Checked = now.UTC()
				continue
			}
			merge.Merged(m, p.MergedAt)
			ev = append(ev, event(by, verbMerged, "%s outside the gate at %s, release unknown: its place in lane %s settles the lane",
				m.Key(), p.MergedAt.UTC().Format(time.RFC3339), m.Lane))
		}
		return ev, nil
	})
}

// recordGone records the outcome of each running merge whose gate process
// and devctl are gone: from the document and exit code its runner left in
// the state directory, or from GitHub when there is no document (a gate
// killed with its caller while devctl merged on, a hung devctl ended). A
// merge nothing can judge (GitHub unanswered) is lost: it settles, once. It
// returns what it recorded and the lost merges.
func (a *app) recordGone(ctx context.Context) (recorded []string, lost []state.Merge) {
	st, err := a.store.Read()
	if err != nil {
		return nil, nil
	}
	gone := func(m state.Merge) bool { return m.Phase == state.Running && !merge.Runs(m, proc.Alive) }
	runs := map[string]runOutcome{}
	for _, m := range st.Merges {
		if !gone(m) {
			continue
		}
		doc, rc := finishedRun(mergeBase(a.store.Dir(), m.Repo, m.PR))
		r := runOutcome{rc: rc}
		var ok bool
		if r.out, ok = parseOutcome(m.PR, doc); merge.NeedsJudging(ok, rc) {
			if r.out, r.unanswered = judgeRun(ctx, m.Repo, m.PR, 1); r.unanswered != nil {
				continue
			}
		}
		runs[m.Key()] = r
	}
	if len(runs) == 0 && !slices.ContainsFunc(st.Merges, gone) {
		return nil, nil
	}
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		var evs []state.Event
		for key, r := range runs {
			i := slices.IndexFunc(st.Merges, func(m state.Merge) bool { return m.Key() == key && gone(m) })
			if i < 0 {
				continue
			}
			m := st.Merges[i]
			lane, _ := a.cfg.LaneNamed(m.Lane)
			lane.Name = m.Lane
			ev, _ := recordRun(st, i, lane, watchParty, r, a.now.UTC(), fmt.Sprintf(" (for %q, whose gate, pid %d, is gone)", m.By.Name, m.PID))
			evs = append(evs, ev...)
			recorded = append(recorded, ev[len(ev)-1].Detail)
			removeMergeFiles(mergeBase(a.store.Dir(), m.Repo, m.PR))
		}
		lost = merge.Lost(st, a.now, proc.Alive)
		for _, m := range lost {
			evs = append(evs, event(watchParty, "merge.lost", "%s in lane %s: its gate (pid %d) and devctl are gone", m.Key(), m.Lane, m.PID))
		}
		return evs, nil
	})
	if err != nil {
		return nil, nil
	}
	return recorded, lost
}

// dropWait bounds how long lanes drop waits for an ended devctl, and its
// gate, to go.
var dropWait = 30 * time.Second

// dropRunning ends the devctl of repo#pr's running merge when its pull
// request merged or closed, waits for it and its gate to go, and records the
// run if its gate did not.
func (a *app) dropRunning(ctx context.Context, by state.Party, repo string, pr int) error {
	ended := a.endHung(ctx, by, a.now, 0, func(m state.Merge) bool { return m.Repo == repo && m.PR == pr })
	if len(ended) == 0 {
		return refused("%s#%d is running and its pull request is open, or GitHub does not answer: devctl merges it on", repo, pr)
	}
	h := ended[0]
	for deadline := time.Now().Add(dropWait); merge.Runs(h.merge, proc.Alive) && time.Now().Before(deadline); {
		time.Sleep(followPoll)
	}
	if merge.Runs(h.merge, proc.Alive) {
		_, err := fmt.Fprintf(a.out, "ended the devctl of %s (%s): its gate (pid %d) records it once devctl is gone\n", h.merge.Key(), a.pullText(h.pull), h.merge.PID)
		return err
	}
	a.recordGone(ctx)
	_, err := fmt.Fprintf(a.out, "ended the devctl of %s (%s): %s\n", h.merge.Key(), a.pullText(h.pull), a.laneAfter(h.merge))
	return err
}

// laneAfter says where an ended merge left its lane.
func (a *app) laneAfter(m state.Merge) string {
	st, err := a.store.Read()
	if err != nil {
		return "lane " + m.Lane + " is unread"
	}
	q := merge.Queue(st, m.Lane)
	for _, s := range q.AllSettling {
		if s.Key() == m.Key() {
			return a.settlingText(*s)
		}
	}
	return "lane " + m.Lane + " no longer holds it"
}

// endChild ends a merge's devctl: merge-child passes SIGTERM on to it.
var endChild = func(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGTERM)
}

// hungRun is a running merge whose devctl was ended, with its pull request.
type hungRun struct {
	merge state.Merge
	pull  github.Pull
}

// endHung asks GitHub about each running merge that pick takes and whose
// devctl runs, and ends the devctl of those that outlived their pull
// request by after (merge.Hung), with a merge.hung event each. A pid that
// is not the merge's recorded merge-child is never signalled. The merge
// stays running until its gate, or recordGone once the gate is gone,
// records how devctl ended.
func (a *app) endHung(ctx context.Context, by state.Party, now time.Time, after time.Duration, pick func(state.Merge) bool) []hungRun {
	st, err := a.store.Read()
	if err != nil {
		return nil
	}
	var ended []hungRun
	for _, m := range st.Merges {
		if m.Phase != state.Running || m.PR == 0 || !proc.Alive(m.Child) || !pick(m) ||
			readPID(mergeBase(a.store.Dir(), m.Repo, m.PR)) != m.Child {
			continue
		}
		p, err := pullState(ctx, m.Repo, m.PR)
		if err != nil || !merge.Hung(p, now, after) || endChild(m.Child) != nil {
			continue
		}
		ended = append(ended, hungRun{merge: m, pull: p})
	}
	if len(ended) > 0 {
		_ = a.store.Update(func(*state.State) ([]state.Event, error) {
			evs := make([]state.Event, 0, len(ended))
			for _, h := range ended {
				evs = append(evs, event(by, "merge.hung", "%s in lane %s: %s, its devctl (pid %d) ended", h.merge.Key(), h.merge.Lane, a.pullText(h.pull), h.merge.Child))
			}
			return evs, nil
		})
	}
	return ended
}

// pullText says what GitHub reports of a hung merge's pull request.
func (a *app) pullText(p github.Pull) string {
	if p.State == github.Closed {
		return "closed without a merge"
	}
	return "merged at " + clock(a.now, p.MergedAt)
}
