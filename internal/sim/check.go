package sim

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// Verdicts of a checked item.
const (
	VerdictOnTrack = "on track"
	VerdictAhead   = "ahead"
	VerdictBehind  = "behind"
	VerdictStuck   = "stuck as forecast"
	VerdictGone    = "gone"
)

// Outcomes of a checked problem.
const (
	ProblemResolved  = "resolved"
	ProblemStanding  = "standing"
	ProblemUnchecked = "unchecked"
)

// StepBoard is the step of an item still on the board.
const StepBoard = "board"

// BehindGrace is how late an item may run before the check calls it
// behind: the loop's own pass between two polls.
const BehindGrace = 10 * time.Minute

// CheckedItem is one forecast item against the desk now.
type CheckedItem struct {
	Ref string `json:"ref"`
	// Expected is the step the forecast had the item in at the check,
	// Actual the step it is in: done once released, gone when the loop
	// no longer records it.
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
	Verdict  string `json:"verdict"`
	// Behind is how long ago the forecast had the item enter the first
	// step it has not reached, with the Behind verdict.
	Behind     time.Duration `json:"behind,omitempty"`
	Agent      string        `json:"agent,omitempty"`
	AgentState string        `json:"agentState,omitempty"`
	Waits      string        `json:"waits,omitempty"`
}

// CheckedProblem is a forecast problem against the items it named.
type CheckedProblem struct {
	Problem
	Outcome string `json:"outcome"`
	// Moved names the refs that left the step the forecast saw them in,
	// Still those that did not.
	Moved []string `json:"moved,omitempty"`
	Still []string `json:"still,omitempty"`
}

// Tally counts the starts and releases up to the check.
type Tally struct {
	Started  int `json:"started"`
	Released int `json:"released"`
}

// Checked is a forecast against the desk at a later time.
type Checked struct {
	ForecastAt time.Time        `json:"forecastAt"`
	At         time.Time        `json:"at"`
	Items      []CheckedItem    `json:"items"`
	Forecast   Tally            `json:"forecast"`
	Actual     Tally            `json:"actual"`
	Problems   []CheckedProblem `json:"problems,omitempty"`
	// Counts the items by verdict (on track, ahead, behind, ...).
	Counts map[string]int `json:"counts"`
}

// Check compares f with the desk at now: items are the active deliveries,
// released the refs whose delivery ended done since the forecast, started
// the refs whose delivery started since it.
func Check(f Forecast, now time.Time, items []Item, released, started []string) Checked {
	c := Checked{ForecastAt: f.At, At: now, Counts: map[string]int{}}
	byRef := map[string]Item{}
	for _, it := range items {
		byRef[strings.ToLower(it.Ref)] = it
	}
	done := refSet(released)
	begun := refSet(started)
	actualOf := func(ref string) string {
		key := strings.ToLower(ref)
		if it, ok := byRef[key]; ok {
			return it.Step
		}
		if done[key] {
			return state.DeliveryDone
		}
		return ""
	}
	for _, fi := range f.Items {
		ci := CheckedItem{Ref: fi.Ref, Expected: expectedStep(fi, now)}
		actual := actualOf(fi.Ref)
		if it, ok := byRef[strings.ToLower(fi.Ref)]; ok {
			ci.Agent, ci.AgentState, ci.Waits = it.Agent, it.AgentState, it.Waits
		}
		switch {
		case actual != "":
			ci.Actual = actual
		case fi.Step == StepBoard && !begun[strings.ToLower(fi.Ref)]:
			ci.Actual = StepBoard
		default:
			ci.Actual = VerdictGone
		}
		ci.Verdict, ci.Behind = verdict(fi, ci.Expected, ci.Actual, now)
		c.Counts[ci.Verdict]++
		c.Items = append(c.Items, ci)
	}
	slices.SortStableFunc(c.Items, func(a, b CheckedItem) int {
		if ra, rb := verdictRank(a.Verdict), verdictRank(b.Verdict); ra != rb {
			return ra - rb
		}
		if a.Behind != b.Behind {
			return int(b.Behind - a.Behind)
		}
		return strings.Compare(a.Ref, b.Ref)
	})
	for _, st := range f.Starts {
		if !st.At.After(now) {
			c.Forecast.Started++
		}
	}
	for _, fi := range f.Items {
		if !fi.ReleaseAt.IsZero() && !fi.ReleaseAt.After(now) {
			c.Forecast.Released++
		}
	}
	c.Actual = Tally{Started: len(started), Released: len(released)}
	stepThen := map[string]string{}
	for _, fi := range f.Items {
		stepThen[strings.ToLower(fi.Ref)] = fi.Step
	}
	for _, p := range f.Problems {
		cp := CheckedProblem{Problem: p, Outcome: ProblemUnchecked}
		for _, ref := range p.Refs {
			key := strings.ToLower(ref)
			then, known := stepThen[key]
			if !known {
				then = StepBoard
			}
			if at := actualOf(ref); at != "" && at != then || then == StepBoard && begun[key] {
				cp.Moved = append(cp.Moved, ref)
			} else {
				cp.Still = append(cp.Still, ref)
			}
		}
		switch {
		case len(p.Refs) == 0:
		case len(cp.Still) == 0:
			cp.Outcome = ProblemResolved
		default:
			cp.Outcome = ProblemStanding
		}
		c.Problems = append(c.Problems, cp)
	}
	return c
}

func refSet(refs []string) map[string]bool {
	out := make(map[string]bool, len(refs))
	for _, r := range refs {
		out[strings.ToLower(r)] = true
	}
	return out
}

// expectedStep is the step the forecast had the item in at now: the last
// transition at or before now, else the step it was in at the forecast.
func expectedStep(fi ItemForecast, now time.Time) string {
	step := fi.Step
	for _, t := range fi.Timeline {
		if t.At.After(now) {
			break
		}
		step = t.Step
	}
	return step
}

// verdict judges actual against expected: the same step or further along
// is on track or ahead; before it, behind by the time since the forecast
// had the item enter the first step it has not reached, once that is past
// BehindGrace; an item the forecast called stuck that did not move is
// stuck as forecast.
func verdict(fi ItemForecast, expected, actual string, now time.Time) (string, time.Duration) {
	switch {
	case actual == VerdictGone || actual == state.DeliveryDropped:
		return VerdictGone, 0
	case fi.Stuck != "" && actual == fi.Step:
		return VerdictStuck, 0
	case actual == expected:
		return VerdictOnTrack, 0
	case fi.Stuck != "" || order(actual) > order(expected):
		return VerdictAhead, 0
	}
	// The first transition the item has not reached: the one after its
	// furthest occurrence of the actual step, else the first further step.
	next := slices.IndexFunc(fi.Timeline, func(t Transition) bool { return order(t.Step) > order(actual) })
	if k := lastIndex(fi.Timeline, actual); k >= 0 {
		next = k + 1
	}
	if next < 0 || next >= len(fi.Timeline) || fi.Timeline[next].At.After(now.Add(-BehindGrace)) {
		return VerdictOnTrack, 0
	}
	return VerdictBehind, now.Sub(fi.Timeline[next].At).Truncate(time.Minute)
}

func lastIndex(ts []Transition, step string) int {
	for k := len(ts) - 1; k >= 0; k-- {
		if ts[k].Step == step {
			return k
		}
	}
	return -1
}

// order places a step in the flow: the board before every stage, done
// after the last.
func order(step string) int {
	switch step {
	case StepBoard:
		return -1
	case state.DeliveryDone:
		return len(Stages)
	}
	if i := slices.Index(Stages, step); i >= 0 {
		return i
	}
	return -2
}

func verdictRank(v string) int {
	switch v {
	case VerdictBehind:
		return 0
	case VerdictStuck:
		return 1
	case VerdictGone:
		return 2
	case VerdictAhead:
		return 3
	}
	return 4
}

// Summary is the one line of the check: how many items are where the
// forecast said, and the rest by verdict.
func (c Checked) Summary() string {
	n := len(c.Items)
	var parts []string
	for _, v := range []string{VerdictAhead, VerdictBehind, VerdictStuck, VerdictGone} {
		if k := c.Counts[v]; k > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", k, v))
		}
	}
	s := fmt.Sprintf("%d of %d items where the forecast said", c.Counts[VerdictOnTrack], n)
	if len(parts) > 0 {
		s += " (" + strings.Join(parts, ", ") + ")"
	}
	return s
}
