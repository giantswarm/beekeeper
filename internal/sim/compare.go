package sim

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Row compares one stage: the model's residence against the log's.
type Row struct {
	Stage    string    `json:"stage"`
	Expected Quantiles `json:"expected"`
	Observed Quantiles `json:"observed"`
	// Gap is the observed median over the expected one.
	Gap time.Duration `json:"gap"`
	// Causes are what the log names around this stage, most frequent first.
	Causes []string `json:"causes,omitempty"`
}

// WhatIf is the model with the observed values and one change undone.
type WhatIf struct {
	Change         string        `json:"change"`
	ReleasedPerDay float64       `json:"releasedPerDay"`
	Gain           float64       `json:"gain"`
	Cycle          time.Duration `json:"cycle"`
}

// Comparison is the model against the log: per stage, the loop, the
// capacity, the throughput and the changes ranked by what they gain.
type Comparison struct {
	Model    Model    `json:"model"`
	Expected Result   `json:"expected"`
	Observed Observed `json:"observed"`
	// Fitted is the model with the observed stage times, loop uptime,
	// drop share and start rate; Baseline its run, which should come
	// close to the observed throughput when the model holds.
	Fitted   Model  `json:"fitted"`
	Baseline Result `json:"baseline"`
	Rows     []Row  `json:"rows"`
	// WhatIfs are the single changes to the fitted model, by gain.
	WhatIfs []WhatIf `json:"whatIfs"`
	// Findings are the sentences a reader acts on, ranked.
	Findings []string `json:"findings"`
}

// FitStages returns m with the log's stage medians alone, where the log
// has at least three residences of a stage; the capacities, chances and
// the board stay as they were.
func FitStages(m Model, o Observed) Model {
	for _, s := range o.Stages {
		if s.Residence.Count >= 3 && s.Residence.Median > 0 {
			m = m.With(s.Stage, s.Residence.Median)
		}
	}
	return m
}

// Fit returns m with the observed values: the stages' medians, the loop's
// uptime, the share of dropped work and the rate at which issues were
// started, so that a run of it replays the week as the log shows it.
func Fit(m Model, o Observed) Model {
	m = FitStages(m, o)
	if o.LoopUptime > 0 && o.LoopUptime < 1 {
		m.LoopUptime = o.LoopUptime
		var sum time.Duration
		for _, g := range o.Outages {
			sum += g.To.Sub(g.From)
		}
		if len(o.Outages) > 0 {
			m.LoopOutage = sum / time.Duration(len(o.Outages))
		}
	}
	m.Dropped = min(0.95, o.DropShare)
	if o.Lanes > 0 {
		m.Lanes = o.Lanes
	}
	m.Backlog = 0
	m.ArrivalsPerDay = o.StartedPerDay
	if o.Days > 0 {
		m.Horizon = time.Duration(o.Days * float64(24*time.Hour))
	}
	return m
}

// Seeds is how many seeds Compare averages the fitted runs over, so that
// the chance of one run never ranks a change below another.
const Seeds = 5

// runAverage runs m with Seeds seeds and averages the rates and medians.
func runAverage(m Model) Result {
	var sum Result
	var cycles []time.Duration
	for i := range Seeds {
		m.Seed = int64(i + 1)
		r := Run(m)
		sum.Started += r.Started
		sum.Released += r.Released
		sum.Dropped += r.Dropped
		sum.Open += r.Open
		sum.StartedPerDay += r.StartedPerDay
		sum.ReleasedPerDay += r.ReleasedPerDay
		sum.WorkerUtilization += r.WorkerUtilization
		sum.LaneUtilization += r.LaneUtilization
		sum.LoopDown += r.LoopDown
		cycles = append(cycles, r.Cycle.Median)
		if i == 0 {
			sum.Horizon, sum.Stages, sum.BacklogWait = r.Horizon, r.Stages, r.BacklogWait
		}
	}
	n := float64(Seeds)
	sum.Started, sum.Released, sum.Dropped, sum.Open = sum.Started/Seeds, sum.Released/Seeds, sum.Dropped/Seeds, sum.Open/Seeds
	sum.StartedPerDay, sum.ReleasedPerDay = sum.StartedPerDay/n, sum.ReleasedPerDay/n
	sum.WorkerUtilization, sum.LaneUtilization = sum.WorkerUtilization/n, sum.LaneUtilization/n
	sum.LoopDown /= Seeds
	sum.Cycle = quantiles(cycles)
	return sum
}

// Compare runs the model, fits it to the observation, runs the fit and
// every single change back towards the model, and ranks the changes.
func Compare(m Model, o Observed) Comparison {
	c := Comparison{Model: m, Expected: Run(m), Observed: o}
	c.Fitted = Fit(m, o)
	c.Baseline = runAverage(c.Fitted)
	expected := map[string]Quantiles{}
	for _, s := range c.Expected.Stages {
		expected[s.Stage] = s.Residence
	}
	observed := map[string]Quantiles{}
	for _, s := range o.Stages {
		observed[s.Stage] = s.Residence
	}
	for _, name := range Stages {
		r := Row{Stage: name, Expected: expected[name], Observed: observed[name], Causes: causes(name, o)}
		if r.Observed.Count > 0 {
			r.Gap = r.Observed.Median - r.Expected.Median
		}
		c.Rows = append(c.Rows, r)
	}
	c.WhatIfs = whatIfs(c.Fitted, c.Baseline, m)
	c.Findings = findings(c)
	return c
}

func whatIfs(fitted Model, baseline Result, model Model) []WhatIf {
	try := func(change string, w Model) WhatIf {
		r := runAverage(w)
		return WhatIf{Change: change, ReleasedPerDay: r.ReleasedPerDay, Gain: r.ReleasedPerDay - baseline.ReleasedPerDay, Cycle: r.Cycle.Median}
	}
	var out []WhatIf
	if fitted.LoopUptime < 1 {
		w := fitted
		w.LoopUptime = 1
		out = append(out, try(fmt.Sprintf("the loop always up (it was up %.0f%% of the time)", 100*fitted.LoopUptime), w))
	}
	if fitted.Dropped > 0 {
		w := fitted
		w.Dropped = 0
		out = append(out, try(fmt.Sprintf("no started issue dropped without a pull request (%.0f%% were)", 100*fitted.Dropped), w))
	}
	for _, name := range Stages {
		f, m := fitted.Stages[name], model.Stages[name]
		if f.Median <= m.Median+m.Median/10 {
			continue
		}
		out = append(out, try(fmt.Sprintf("%s at %s instead of %s", name, short(m.Median), short(f.Median)), fitted.With(name, m.Median)))
	}
	{
		w := fitted
		w.Backlog = -1
		w.ArrivalsPerDay = 0
		out = append(out, try(fmt.Sprintf("every free slot filled from the board (%.1f starts a day were made, %d slots exist)", fitted.ArrivalsPerDay, fitted.Workers), w))
	}
	slices.SortStableFunc(out, func(a, b WhatIf) int {
		switch {
		case a.Gain > b.Gain:
			return -1
		case a.Gain < b.Gain:
			return 1
		}
		return 0
	})
	return out
}

// causes names what the log counted around a stage.
func causes(stage string, o Observed) []string {
	var out []string
	add := func(n int, format string, a ...any) {
		if n > 0 {
			out = append(out, fmt.Sprintf("%d× ", n)+fmt.Sprintf(format, a...))
		}
	}
	failures := func(steps ...string) {
		for _, f := range o.Failures {
			for _, s := range steps {
				if strings.HasPrefix(f.Name, s+": ") {
					add(f.N, "%s", f.Name)
				}
			}
		}
	}
	parks := func(reason, what string) {
		for _, p := range o.Parks {
			if p.Name == reason {
				add(p.N, "%s", what)
			}
		}
	}
	switch stage {
	case StageImplementing:
		add(o.Dropped, "a delivery ended without a pull request (%.0f%% of the %d that ended)", 100*o.DropShare, o.Released+o.Dropped)
		parks("timer", "a worker parked on a timer")
		parks("person", "a worker parked on its person's answer")
		parks("other", "a worker parked on something else")
	case StageCI:
		parks("ci", "a worker parked on CI")
	case StageReviewQueued:
		failures("review")
		parks("desk review", "a worker parked on a desk review")
		if o.WorkerStarts > 0 {
			out = append(out, fmt.Sprintf("%d reviewer starts for %d worker starts", o.ReviewStarts, o.WorkerStarts))
		}
	case StageFixing:
		for _, f := range o.MergeFailures {
			if strings.HasPrefix(f.Name, "exit 1:") {
				add(f.N, "a merge %s", f.Name)
			}
		}
		failures("red", "rework", "forward")
	case StageMerging:
		for _, f := range o.MergeFailures {
			if !strings.HasPrefix(f.Name, "exit 1:") {
				add(f.N, "a merge %s", f.Name)
			}
		}
		failures("go", "merge")
		parks("merge", "a worker parked on a merge")
		add(o.RateLimited, "a GitHub API limit was exhausted")
	case StageReleasing:
		add(o.Rollbacks, "a release rolled back")
		failures("release", "install")
	}
	return out
}

// findings ranks what a reader should act on.
func findings(c Comparison) []string {
	var out []string
	o := c.Observed
	if o.MeanCeiling > 0 {
		out = append(out, fmt.Sprintf("%.1f agents were busy of a ceiling of %.0f (%.0f%%): the capacity, not the flow, sets the throughput.", o.MeanBusy, o.MeanCeiling, 100*o.Utilization))
	}
	if o.LoopUptime < 0.98 {
		out = append(out, fmt.Sprintf("The loop was up %.0f%% of the window, down %s in all; while it is down nothing starts, is reviewed or merges.", 100*o.LoopUptime, short(o.LoopDown)))
	}
	for _, w := range c.WhatIfs {
		if w.Gain >= 1 {
			out = append(out, fmt.Sprintf("+%.0f released a day with %s (%.0f instead of %.0f).", w.Gain, w.Change, w.ReleasedPerDay, c.Baseline.ReleasedPerDay))
		}
	}
	var worst *Row
	for i := range c.Rows {
		r := &c.Rows[i]
		if r.Observed.Count >= 3 && (worst == nil || r.Gap > worst.Gap) {
			worst = r
		}
	}
	if worst != nil && worst.Gap > 0 {
		s := fmt.Sprintf("%s takes %s where the model takes %s", worst.Stage, short(worst.Observed.Median), short(worst.Expected.Median))
		if len(worst.Causes) > 0 {
			s += "; the log names " + worst.Causes[0]
		}
		out = append(out, s+".")
	}
	if c.Baseline.ReleasedPerDay > 0 && o.ReleasedPerDay > 0 {
		ratio := c.Baseline.ReleasedPerDay / o.ReleasedPerDay
		if ratio < 0.7 || ratio > 1.4 {
			out = append(out, fmt.Sprintf("The fitted model releases %.0f a day where the log shows %.0f: the model misses something; check the stage times and the loop uptime.", c.Baseline.ReleasedPerDay, o.ReleasedPerDay))
		}
	}
	return out
}

// short renders a duration: 4s, 12m, 3h05m, 2d.
func short(d time.Duration) string {
	switch {
	case d < 0:
		return "-" + short(-d)
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours())/24)
}
