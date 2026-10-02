package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/state"
)

// keptBy says what keeps ag on the roster past agents.staleAfter at now: its
// keep marker while it holds, or an open timer that wakes it by name until
// the timer is done. Empty when nothing keeps it.
func keptBy(st *state.State, ag state.Agent, now time.Time) string {
	if k := ag.Keep; k.Holds(now) {
		s := "kept"
		if !k.Until.IsZero() {
			s += " until " + clock(now, k.Until)
		}
		if k.Reason != "" {
			s += ": " + k.Reason
		}
		return s
	}
	for _, t := range st.Timers {
		if t.Wake != "" && strings.EqualFold(t.Wake, ag.Name) {
			return fmt.Sprintf("timer #%d wakes it", t.ID)
		}
	}
	return ""
}

func (a *app) agentKeepCmd() *cobra.Command {
	var until, reason string
	var lift bool
	c := &cobra.Command{
		Use:   "keep <agent>",
		Short: "Keep an idle agent on the roster: the doctor never removes it for staleness",
		Long: `Marks an agent kept on purpose: a judge or reviewer session a person returns
to, a spare, a worker parked on a long external wait. The doctor never takes
a kept entry off the roster for staleness (agents.staleAfter) nor archives
its desktop session, the watch says no AGENTS STOPPED line for it, and its
sessions serve record covers its board item while it is parked. --until ends
the marker at a time or after a duration, --reason says why; agents shows
both. --no-keep lifts the marker: the entry is back under agents.staleAfter.

An open timer that wakes the agent by name (timer add --wake) keeps it the
same way until the timer is done, without a marker.`,
		Example: `  beekeeper agents keep "Judge" --reason "a person returns to it"
  beekeeper agents keep "BK 229" --until 6h --reason "waits on the batch job"
  beekeeper agents keep "Judge" --no-keep`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			end, err := untilTime(a.now, until)
			if err != nil {
				return err
			}
			if until != "" && !end.After(a.now) {
				return usageErr("--until %s is not in the future", until)
			}
			reason = strings.Join(strings.Fields(reason), " ")
			var line string
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				i, err := findAgent(st, args[0])
				if err != nil {
					return nil, err
				}
				ag := &st.Agents[i]
				if lift {
					if ag.Keep == nil {
						return nil, refused("%q carries no keep marker", ag.Name)
					}
					ag.Keep = nil
					line = fmt.Sprintf("%s: keep lifted, back under agents.staleAfter", ag.Name)
					if why := keptBy(st, *ag, a.now); why != "" {
						line = fmt.Sprintf("%s: keep lifted; the doctor still leaves it while %s", ag.Name, why)
					}
					return []state.Event{event(me, "agents.unkeep", "%s", ag.Name)}, nil
				}
				ag.Keep = &state.Keep{By: me, At: a.now.UTC(), Until: end.UTC(), Reason: reason}
				line = fmt.Sprintf("%s: %s", ag.Name, keptBy(st, *ag, a.now))
				return []state.Event{event(me, "agents.keep", "%s", line)}, nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, line)
			return err
		},
	}
	c.Flags().StringVar(&until, "until", "", "end the marker at a time (18:00) or after a duration (6h)")
	c.Flags().StringVar(&reason, "reason", "", "why the entry is kept, shown by agents")
	c.Flags().BoolVar(&lift, "no-keep", false, "lift the marker: the entry is back under agents.staleAfter")
	c.MarkFlagsMutuallyExclusive("no-keep", "until")
	c.MarkFlagsMutuallyExclusive("no-keep", "reason")
	return c
}
