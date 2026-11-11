package sim

import (
	"cmp"
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// Count is a named tally.
type Count struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

// Outage is a period without any supervisor loop event.
type Outage struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// ProviderUse is a provider's mean busy agents against its mean ceiling
// over the window, from the watch's USAGE lines.
type ProviderUse struct {
	Provider    string  `json:"provider"`
	MeanBusy    float64 `json:"meanBusy"`
	MeanCeiling float64 `json:"meanCeiling"`
}

// ObservedStage is a delivery step's residence time over the window.
type ObservedStage struct {
	Stage     string    `json:"stage"`
	Residence Quantiles `json:"residence"`
}

// Observed is the real flow read from the event log.
type Observed struct {
	Outcomes map[string]int `json:"outcomes"`
	Since    time.Time      `json:"since"`
	Until    time.Time      `json:"until"`
	Days     float64        `json:"days"`
	// Stages are the delivery steps' residence times (delivery.step).
	Stages []ObservedStage `json:"stages"`
	// Deliveries counts the issues observed, Started those whose worker
	// started in the window, Released those that reached done in it and
	// Dropped those that ended in it without a merge; DropShare is
	// Dropped over both.
	Deliveries     int     `json:"deliveries"`
	Started        int     `json:"started"`
	Released       int     `json:"released"`
	Dropped        int     `json:"dropped"`
	DropShare      float64 `json:"dropShare"`
	StartedPerDay  float64 `json:"startedPerDay"`
	ReleasedPerDay float64 `json:"releasedPerDay"`
	// LoopUptime is the share of the window with a supervisor loop event
	// at least every LoopGap; Outages are the longest gaps.
	LoopUptime float64       `json:"loopUptime"`
	LoopDown   time.Duration `json:"loopDown"`
	Outages    []Outage      `json:"outages,omitempty"`
	// MeanBusy and MeanCeiling are the busy agents and the ceiling across
	// the subscription providers, time-weighted from the USAGE lines.
	MeanBusy    float64       `json:"meanBusy"`
	MeanCeiling float64       `json:"meanCeiling"`
	Utilization float64       `json:"utilization"`
	Providers   []ProviderUse `json:"providers,omitempty"`
	// Starts count agents.start events by kind.
	WorkerStarts int `json:"workerStarts"`
	ReviewStarts int `json:"reviewStarts"`
	// Failures are the supervisor.failure events by step and reason,
	// MergeFailures the merge.failed events by exit code, Parks the
	// agents.park events by what they wait on, Rollbacks the release
	// rollbacks.
	Failures      []Count `json:"failures,omitempty"`
	MergeFailures []Count `json:"mergeFailures,omitempty"`
	Parks         []Count `json:"parks,omitempty"`
	Rollbacks     int     `json:"rollbacks"`
	// RateLimited counts events that name an exhausted GitHub API limit.
	RateLimited int `json:"rateLimited"`
	// Lanes counts the merge lanes that saw a merge.
	Lanes int `json:"lanes"`
}

// LoopGap is the longest silence of the supervisor loop that still counts
// as up: the loop logs a claim or a stage on nearly every 30s pass.
const LoopGap = 15 * time.Minute

// Verbs are the event verbs Observe reads, for the caller's filter.
func Verbs(verb string) bool {
	switch {
	case verb == state.DeliveryStepVerb, verb == "watch.usage", verb == "agents.start", verb == "agents.park", verb == "merge.failed", verb == "merging":
		return true
	case strings.HasPrefix(verb, "supervisor."), strings.HasPrefix(verb, "self-update."):
		return true
	}
	return strings.Contains(verb, "rate") || strings.Contains(verb, "budget")
}

var (
	usageRe   = regexp.MustCompile(`(\w+): (\d+) busy of ceiling (\d+)`)
	failureRe = regexp.MustCompile(`^\[(\w+)`)
	exitRe    = regexp.MustCompile(`exit (\d+)`)
	laneRe    = regexp.MustCompile(`in lane (\S+)`)
	numberRe  = regexp.MustCompile(`[0-9a-f]{12,}|\d+`)
	kindRe    = regexp.MustCompile(`kind (\w+)`)
	ciRe      = regexp.MustCompile(`\bci\b|checks|workflow|green|red`)
)

// Observe measures the window since..until from the events (any order,
// any verbs: it keeps what it needs).
func Observe(events []state.Event, since, until time.Time) Observed {
	o := Observed{Since: since, Until: until, Days: until.Sub(since).Hours() / 24}
	events = slices.Clone(events)
	slices.SortStableFunc(events, func(a, b state.Event) int { return a.At.Compare(b.At) })
	var loopAt []time.Time
	var usage []state.Event
	failures := map[string]int{}
	mergeFails := map[string]int{}
	parks := map[string]int{}
	lanes := map[string]bool{}
	var steps []state.Event
	for _, e := range events {
		if e.At.Before(since) || e.At.After(until) {
			if e.Verb == state.DeliveryStepVerb && e.At.Before(since) {
				steps = append(steps, e)
			}
			continue
		}
		d := e.Detail
		switch {
		case e.Verb == state.DeliveryStepVerb:
			steps = append(steps, e)
		case e.Verb == "watch.usage":
			usage = append(usage, e)
		case e.Verb == "agents.start":
			if m := kindRe.FindStringSubmatch(d); m != nil {
				if m[1] == "review" {
					o.ReviewStarts++
				} else {
					o.WorkerStarts++
				}
			}
		case e.Verb == "agents.park":
			parks[parkReason(d)]++
		case e.Verb == "merging":
			if m := laneRe.FindStringSubmatch(d); m != nil {
				lanes[m[1]] = true
			}
		case e.Verb == "merge.failed":
			code := "?"
			if m := exitRe.FindStringSubmatch(d); m != nil {
				code = m[1]
			}
			mergeFails[code]++
		case e.Verb == "supervisor.failure":
			failures[failureClass(d)]++
		case e.Verb == "supervisor.file" && strings.Contains(d, "RELEASE ROLLED BACK"), strings.HasPrefix(e.Verb, "self-update.") && strings.Contains(strings.ToLower(d), "roll"):
			o.Rollbacks++
		}
		if strings.HasPrefix(e.Verb, "supervisor.") && e.Verb != "supervisor.throughput-health" {
			loopAt = append(loopAt, e.At)
		}
		if strings.Contains(d, "rate limit") || strings.Contains(d, "API rate") {
			o.RateLimited++
		}
	}
	o.observeSteps(steps)
	o.observeLoop(loopAt)
	o.observeUsage(usage)
	o.Failures = counts(failures, 12)
	o.MergeFailures = counts(mergeFails, 0)
	for i := range o.MergeFailures {
		o.MergeFailures[i].Name = mergeExit(o.MergeFailures[i].Name)
	}
	o.Parks = counts(parks, 0)
	o.Lanes = len(lanes)
	return o
}

// observeSteps reads the delivery steps: the residence in each step, the
// deliveries started, released and dropped in the window.
func (o *Observed) observeSteps(steps []state.Event) {
	o.Outcomes = state.DeliveryOutcomes(steps, o.Since, o.Until)
	last := map[string]state.Delivery{}
	first := map[string]time.Time{}
	released := map[string]bool{}
	dropped := map[string]bool{}
	residence := map[string][]time.Duration{}
	for _, e := range steps {
		var d state.Delivery
		if json.Unmarshal([]byte(e.Detail), &d) != nil || d.Ref == "" {
			continue
		}
		if _, ok := first[d.Ref]; !ok {
			first[d.Ref] = e.At
		}
		old, ok := last[d.Ref]
		if ok && old.Step == d.Step && old.Started.Equal(d.Started) {
			continue
		}
		if ok && old.Active() && old.Step != state.DeliveryUnknown && !e.At.Before(o.Since) {
			start := old.Since
			if start.Before(o.Since) {
				start = o.Since
			}
			if e.At.After(start) {
				residence[old.Step] = append(residence[old.Step], e.At.Sub(start))
			}
		}
		if !e.At.Before(o.Since) {
			if d.Step == state.DeliveryDone && d.Outcome != state.OutcomeNoChange {
				released[d.Ref] = true
			}
			if d.Step == state.DeliveryDropped {
				dropped[d.Ref] = true
			}
		}
		last[d.Ref] = d
	}
	o.Released = len(released)
	for ref, d := range last {
		o.Deliveries++
		// Started is the worker's start the loop recorded, which may lie
		// before the loop first observed the delivery.
		started := d.Started
		if started.IsZero() {
			started = first[ref]
		}
		if !started.Before(o.Since) && !started.After(o.Until) {
			o.Started++
		}
		// Dropped is the delivery's end: one started again is not dropped.
		if d.Step == state.DeliveryDropped && dropped[ref] && !released[ref] {
			o.Dropped++
		}
		if d.Active() && d.Step != state.DeliveryUnknown {
			start := d.Since
			if start.Before(o.Since) {
				start = o.Since
			}
			if o.Until.After(start) {
				residence[d.Step] = append(residence[d.Step], o.Until.Sub(start))
			}
		}
	}
	for _, s := range Stages {
		if ds := residence[s]; len(ds) > 0 {
			o.Stages = append(o.Stages, ObservedStage{Stage: s, Residence: quantiles(ds)})
		}
	}
	if ended := o.Released + o.Dropped; ended > 0 {
		o.DropShare = float64(o.Dropped) / float64(ended)
	}
	if o.Days > 0 {
		o.StartedPerDay = float64(o.Started) / o.Days
		o.ReleasedPerDay = float64(o.Released) / o.Days
	}
}

// observeLoop finds the silences of the loop longer than LoopGap.
func (o *Observed) observeLoop(at []time.Time) {
	prev := o.Since
	var outages []Outage
	for _, t := range append(at, o.Until) {
		if t.Sub(prev) > LoopGap {
			outages = append(outages, Outage{From: prev, To: t})
			o.LoopDown += t.Sub(prev)
		}
		prev = t
	}
	if len(at) == 0 {
		o.LoopDown = o.Until.Sub(o.Since)
		outages = []Outage{{From: o.Since, To: o.Until}}
	}
	if w := o.Until.Sub(o.Since); w > 0 {
		o.LoopUptime = 1 - float64(o.LoopDown)/float64(w)
	}
	slices.SortFunc(outages, func(a, b Outage) int { return cmp.Compare(b.To.Sub(b.From), a.To.Sub(a.From)) })
	o.Outages = outages[:min(3, len(outages))]
}

// observeUsage integrates the USAGE lines: each reading holds until the
// next one, at most 30 minutes.
func (o *Observed) observeUsage(usage []state.Event) {
	type acc struct{ busy, ceiling float64 }
	per := map[string]*acc{}
	var total time.Duration
	for i, e := range usage {
		hold := 30 * time.Minute
		if i+1 < len(usage) {
			hold = min(hold, usage[i+1].At.Sub(e.At))
		} else {
			hold = min(hold, o.Until.Sub(e.At))
		}
		if hold <= 0 {
			continue
		}
		total += hold
		h := float64(hold)
		for _, m := range usageRe.FindAllStringSubmatch(e.Detail, -1) {
			a := per[m[1]]
			if a == nil {
				a = &acc{}
				per[m[1]] = a
			}
			b, _ := strconv.Atoi(m[2])
			c, _ := strconv.Atoi(m[3])
			a.busy += float64(b) * h
			a.ceiling += float64(c) * h
		}
	}
	if total == 0 {
		return
	}
	names := make([]string, 0, len(per))
	for n := range per {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		a := per[n]
		p := ProviderUse{Provider: n, MeanBusy: a.busy / float64(total), MeanCeiling: a.ceiling / float64(total)}
		o.Providers = append(o.Providers, p)
		o.MeanBusy += p.MeanBusy
		o.MeanCeiling += p.MeanCeiling
	}
	if o.MeanCeiling > 0 {
		o.Utilization = o.MeanBusy / o.MeanCeiling
	}
}

// parkReason classes what a park waits on.
func parkReason(detail string) string {
	d := strings.ToLower(detail)
	switch {
	case strings.Contains(d, "desk review"):
		return "desk review"
	case ciRe.MatchString(d):
		return "ci"
	case strings.Contains(d, "timer"):
		return "timer"
	case strings.Contains(d, "merge"):
		return "merge"
	case strings.Contains(d, "note "), strings.Contains(d, "answer"), strings.Contains(d, "person"), strings.Contains(d, "home"):
		return "person"
	case strings.Contains(d, "host"), strings.Contains(d, "ssh"):
		return "host"
	case strings.Contains(d, "build slot"), strings.Contains(d, "capacity"):
		return "capacity"
	}
	return "other"
}

// failureClass is a supervisor.failure's step and its reason without the
// numbers: "intake: board move: gh api graphql: Field 'login' doesn't…".
func failureClass(detail string) string {
	step := "loop"
	if m := failureRe.FindStringSubmatch(detail); m != nil {
		step = m[1]
	}
	msg := detail
	if i := strings.Index(detail, "{"); i >= 0 {
		var f struct {
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(detail[i:]), &f) == nil && f.Message != "" {
			msg = f.Message
		}
	}
	msg = strings.ReplaceAll(msg, "beekeeper: ", "")
	msg = numberRe.ReplaceAllString(msg, "N")
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > 120 {
		msg = msg[:120] + "…"
	}
	return step + ": " + msg
}

// mergeExit names a merge's exit code as the gate defines it.
func mergeExit(code string) string {
	switch code {
	case "1":
		return "exit 1: red (the merge-result check or CI failed)"
	case "2":
		return "exit 2: timeout"
	case "3":
		return "exit 3: not applicable (draft, behind, conflict or a duplicate run)"
	case "5":
		return "exit 5: refused (review or policy)"
	case "7":
		return "exit 7: merge validation unavailable"
	case "74":
		return "exit 74: budget unread"
	case "75":
		return "exit 75: busy machine"
	case "77":
		return "exit 77: hold, budget unknown or reviews unreadable"
	}
	return "exit " + code
}

func counts(m map[string]int, top int) []Count {
	var out []Count
	for k, v := range m {
		out = append(out, Count{Name: k, N: v})
	}
	slices.SortFunc(out, func(a, b Count) int {
		if a.N != b.N {
			return b.N - a.N
		}
		return strings.Compare(a.Name, b.Name)
	})
	if top > 0 && len(out) > top {
		out = out[:top]
	}
	return out
}
