package cmd

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/state"
)

type deliveryRow struct {
	state.Delivery
	Flow    deliveryFlow  `json:"flow"`
	StepAge time.Duration `json:"stepAge"`
	Age     time.Duration `json:"age"`
}

type deliveryView struct {
	Deliveries      []deliveryRow `json:"deliveries"`
	WaitingOnPerson int           `json:"waitingOnPerson"`
	Blocked         int           `json:"blocked"`
	InDelivery      int           `json:"inDelivery"`
	Limit           int           `json:"limit"`
	CompleteAt      time.Time     `json:"completeAt"`
}

// readDelivery is the component's one state-only reading, shared by status and
// report. Capacity deliberately has no dependency on this observation.
func readDelivery(st *state.State, limit int, now time.Time, policies ...deliveryPolicy) deliveryView {
	p := deliveryPolicy{}
	if len(policies) > 0 {
		p = policies[0]
	}
	v := deliveryView{Deliveries: []deliveryRow{}, Limit: limit, CompleteAt: st.DeliveriesAt}
	for _, d := range st.Deliveries {
		flow := classifyDelivery(st, d, now, p)
		v.Deliveries = append(v.Deliveries, deliveryRow{Delivery: d, Flow: flow, StepAge: max(0, now.Sub(d.Since)), Age: max(0, now.Sub(d.Started))})
		if deliveryOwned(st, d, now, p) {
			switch flow.Class {
			case deliveryPerson:
				v.WaitingOnPerson++
			case deliveryBlocked:
				v.Blocked++
			default:
				v.InDelivery++
			}
		}
	}
	slices.SortFunc(v.Deliveries, func(a, b deliveryRow) int { return strings.Compare(a.Ref, b.Ref) })
	return v
}

func (v deliveryView) line() string {
	line := fmt.Sprintf("%d flowing of limit %d", v.InDelivery, v.Limit)
	if v.Limit == 0 {
		line += " (off)"
	}
	return fmt.Sprintf("%s; %d waiting on the person; %d blocked", line, v.WaitingOnPerson, v.Blocked)
}

func (a *app) deliveryCmd() *cobra.Command {
	var stats bool
	var since time.Duration
	c := &cobra.Command{Use: "delivery", Short: "Show issue deliveries from the local state", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if since <= 0 {
			return usageErr("--since must be a positive duration")
		}
		if stats {
			events, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == state.DeliveryStepVerb })
			if err != nil {
				return err
			}
			v := deliveryStatistics(events, a.timeNow().Add(-since), a.timeNow())
			if a.json {
				return a.printJSON(v)
			}
			for _, s := range v.Steps {
				if _, err := fmt.Fprintf(a.out, "%s: median %s, worst %s (%d observations)\n", s.Step, s.Median, s.Worst, s.Samples); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintf(a.out, "%d released, %.2f released per day since %s; mean start to released %s (%d complete cycles)\n", v.Released, v.ReleasedPerDay, v.Since.Format(time.RFC3339), v.MeanCycleTime, v.CycleSamples)
			return err
		}
		st, err := a.store.Read()
		if err != nil {
			return err
		}
		v := readDelivery(st, a.cfg.Delivery.WIPLimit, a.timeNow(), a.deliveryPolicy())
		if a.json {
			return a.printJSON(v)
		}
		for _, d := range v.Deliveries {
			ref := deliveryURL(d.Ref, slices.Contains(d.Pulls, d.Ref))
			var pulls []string
			for _, p := range d.Pulls {
				pulls = append(pulls, deliveryURL(p, true))
			}
			waits := d.Waits
			if d.Verdict != "" {
				waits = strings.TrimSpace(waits + " " + d.Verdict + " " + d.Release)
			}
			if _, err := fmt.Fprintf(a.out, "%s  %s  %s  step %s  age %s  %s  %s\n", ref, strings.Join(pulls, ","), d.Step, d.StepAge.Round(time.Second), d.Age.Round(time.Second), d.Agent, waits); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintln(a.out, v.line())
		return err
	}}
	c.AddCommand(a.deliveryDropCmd())
	c.Flags().BoolVar(&stats, "stats", false, "Show step durations and releases per day from delivery.step events")
	c.Flags().DurationVar(&since, "since", 7*24*time.Hour, "Statistics window")
	return c
}

func deliveryURL(ref string, pull bool) string {
	r, ok := parseRef(ref)
	if !ok {
		return ref
	}
	kind := "issues"
	if pull {
		kind = loopPullStep
	}
	return fmt.Sprintf("https://github.com/%s/%s/%d", r.Repo, kind, r.N)
}

type deliveryStepStats struct {
	Step    string        `json:"step"`
	Median  time.Duration `json:"median"`
	Worst   time.Duration `json:"worst"`
	Samples int           `json:"samples"`
}
type deliveryStats struct {
	Since          time.Time           `json:"since"`
	Until          time.Time           `json:"until"`
	Steps          []deliveryStepStats `json:"steps"`
	Released       int                 `json:"released"`
	ReleasedPerDay float64             `json:"releasedPerDay"`
	MeanCycleTime  time.Duration       `json:"meanCycleTime"`
	CycleSamples   int                 `json:"cycleSamples"`
}

// Intervals crossing the window boundary are clipped. End states have no
// residence duration; duplicate events and events in the future add nothing.
func deliveryStatistics(events []state.Event, since, until time.Time) deliveryStats {
	v := deliveryStats{Since: since, Until: until, Steps: []deliveryStepStats{}}
	last := map[string]state.Delivery{}
	durations := map[string][]time.Duration{}
	slices.SortStableFunc(events, func(a, b state.Event) int { return a.At.Compare(b.At) })
	for _, e := range events {
		if e.At.After(until) {
			continue
		}
		var d state.Delivery
		if e.Verb != state.DeliveryStepVerb || json.Unmarshal([]byte(e.Detail), &d) != nil || d.Ref == "" {
			continue
		}
		old, ok := last[d.Ref]
		if ok && old.Step == d.Step && old.Started.Equal(d.Started) {
			continue
		}
		if ok && old.Active() && e.At.After(since) {
			start := old.Since
			if start.Before(since) {
				start = since
			}
			if e.At.After(start) {
				durations[old.Step] = append(durations[old.Step], e.At.Sub(start))
			}
		}
		if d.Step == state.DeliveryDone && !e.At.Before(since) {
			v.Released++
			// Completed cycles keep their full age, even if their start precedes
			// the statistics window. Unknown start times cannot size a limit.
			if !d.Started.IsZero() && !d.Started.After(e.At) {
				cycle := e.At.Sub(d.Started)
				v.CycleSamples++
				v.MeanCycleTime += (cycle - v.MeanCycleTime) / time.Duration(v.CycleSamples)
			}
		}
		last[d.Ref] = d
	}
	for _, d := range last {
		if d.Active() {
			start := d.Since
			if start.Before(since) {
				start = since
			}
			if until.After(start) {
				durations[d.Step] = append(durations[d.Step], until.Sub(start))
			}
		}
	}
	for step, ds := range durations {
		slices.Sort(ds)
		median := ds[len(ds)/2]
		if len(ds)%2 == 0 {
			median = ds[len(ds)/2-1] + (median-ds[len(ds)/2-1])/2
		}
		v.Steps = append(v.Steps, deliveryStepStats{Step: step, Median: median, Worst: ds[len(ds)-1], Samples: len(ds)})
	}
	slices.SortFunc(v.Steps, func(a, b deliveryStepStats) int { return strings.Compare(a.Step, b.Step) })
	if days := until.Sub(since).Hours() / 24; days > 0 {
		v.ReleasedPerDay = float64(v.Released) / days
	}
	return v
}

func (a *app) deliveryDropCmd() *cobra.Command {
	var reason string
	c := &cobra.Command{Use: "drop <ref> --reason <why>", Short: "End only a delivery, keeping its pull requests and workers", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		reason = strings.TrimSpace(reason)
		if reason == "" {
			return usageErr("delivery drop requires a nonempty --reason")
		}
		refs, err := skipRefs(args)
		if err != nil {
			return usageErr("%v", err)
		}
		by, err := a.caller()
		if err != nil {
			return err
		}
		snapshot, err := a.store.Read()
		if err != nil {
			return err
		}
		idx := slices.IndexFunc(snapshot.Deliveries, func(d state.Delivery) bool { return strings.EqualFold(d.Ref, refs[0]) })
		if idx < 0 {
			return refused("delivery not found: %s", refs[0])
		}
		observed := snapshot.Deliveries[idx]
		if observed.DropReason != "" && observed.Step == state.DeliveryDropped {
			return nil
		}
		heads := map[string]string{}
		// Bind to the actual GitHub head, which may have moved since the last
		// supervisor observation. A failed read cannot produce an unbound drop.
		for _, ref := range observed.Pulls {
			p, err := readLoopPull(c.Context(), ref)
			if err != nil {
				return err
			}
			if p.Head == "" {
				return refused("delivery head not observed for %s; cannot bind drop", ref)
			}
			heads[ref] = p.Head
		}

		return a.store.Update(func(st *state.State) ([]state.Event, error) {
			i := slices.IndexFunc(st.Deliveries, func(d state.Delivery) bool { return strings.EqualFold(d.Ref, refs[0]) })
			if i < 0 {
				return nil, refused("delivery not found: %s", refs[0])
			}
			d := &st.Deliveries[i]
			if d.DropReason != "" && d.Step == state.DeliveryDropped {
				return nil, nil
			}
			if !d.Active() {
				return nil, refused("delivery already ended: %s", refs[0])
			}
			if !d.Started.Equal(observed.Started) || !slices.Equal(d.Pulls, observed.Pulls) || !maps.Equal(d.Heads, observed.Heads) {
				return nil, refused("delivery changed while reading its head; retry drop")
			}

			prev := slices.Clone(st.Deliveries)
			d.Heads = heads
			d.DroppedHeads = maps.Clone(heads)
			// The CLI clock was captured before the network reads. Commit time
			// prevents a cached read from during those reads undoing this drop.
			at := time.Now().UTC()
			if logical := a.timeNow(); logical.After(at) {
				at = logical
			}
			d.Step, d.Since, d.DropAt, d.DropBy, d.DropReason = state.DeliveryDropped, at, at, by, reason
			events := state.DeliveryEvents(prev, st.Deliveries, by)
			return append(events, about(event(by, "delivery.drop", "%s: %s", deliveryURL(d.Ref, slices.Contains(d.Pulls, d.Ref)), reason), d.Ref)), nil
		})
	}}
	c.Flags().StringVar(&reason, "reason", "", "Why the delivery is ended (required); does not close a PR or stop a worker")
	return c
}
