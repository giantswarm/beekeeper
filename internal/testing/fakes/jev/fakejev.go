// Package fakejev is the scenarios' JEV (contract §3.1): an in-process jev.Client with typed answers per
// decision point and subject (by default the rule's answer at confidence 0.9), errors, timeouts that
// fail at once, and low confidence. It records every request and the names of the fields it carried.
package fakejev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/jev"
	"github.com/giantswarm/beekeeper/internal/settle"
)

// Tools the fake answers (contract §4.2).
const (
	ToolSet   = "jev_set"
	ToolCalls = "jev_calls"
)

// DefaultConfidence is the confidence of an agreeing answer, and of an answer set without one.
const DefaultConfidence = 0.9

// Error kinds jev_set and pre_configuration take.
const (
	ErrorTimeout = "timeout"
	ErrorServer  = "server"
)

// Options configure the fake.
type Options struct {
	// Clock stamps the recorded requests; nil leaves them unstamped.
	Clock clock.Clock
	// Tracker sees each call while it runs.
	Tracker *settle.Tracker
}

// Fake is the in-process JEV. Pass it as the engine's jev.Client.
type Fake struct {
	clock   clock.Clock
	tracker *settle.Tracker

	mu       sync.Mutex
	agree    bool
	answers  map[key]entry
	requests []Record
	marks    map[string]int
}

var _ jev.Client = (*Fake)(nil)

// key selects an answer: a point and a subject (an item's ref or a person's login; empty for any).
type key struct{ point, subject string }

type entry struct {
	answer     string
	confidence float64
	err        string
}

// Record is one request the fake was asked.
type Record struct {
	At      time.Time
	Point   string
	Subject string
	// Fields are the names of the fields the request carried (see FieldNames).
	Fields []string
}

// New returns a fake that agrees with the rule until seeded otherwise.
func New(o Options) *Fake {
	return &Fake{clock: o.Clock, tracker: o.Tracker, agree: true, answers: map[key]entry{}, marks: map[string]int{}}
}

// Ask answers a request: the answer set for its point and subject, else the one set for its point,
// else the rule's answer at confidence 0.9. A timeout or server error fails at once.
func (f *Fake) Ask(ctx context.Context, r jev.Request) (jev.Answer, error) {
	done := f.tracker.Begin("fakejev: call")
	defer done()

	rec := Record{Point: r.Point, Subject: r.Subject, Fields: FieldNames(r.Fields)}
	if f.clock != nil {
		rec.At = f.clock.Now()
	}
	f.mu.Lock()
	f.requests = append(f.requests, rec)
	e, ok := f.answers[key{r.Point, r.Subject}]
	if !ok {
		e, ok = f.answers[key{r.Point, ""}]
	}
	agree := f.agree
	f.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return jev.Answer{}, &jev.Error{Kind: jev.ErrorKind(err), Err: err}
	}
	switch {
	case ok && e.err == ErrorTimeout:
		return jev.Answer{}, &jev.Error{Kind: jev.KindTimeout, Err: context.DeadlineExceeded}
	case ok && e.err == ErrorServer:
		return jev.Answer{}, &jev.Error{Kind: jev.KindServer, Status: 500, Err: errors.New("internal server error")}
	case ok:
		return jev.Answer{Answer: e.answer, Confidence: e.confidence}, nil
	case agree:
		return jev.Answer{Answer: r.Rule, Confidence: DefaultConfidence}, nil
	}
	return jev.Answer{}, &jev.Error{Kind: jev.KindServer, Status: 500, Err: fmt.Errorf("no answer set for %s %s", r.Point, r.Subject)}
}

// Fields of ready? and bug-severity?.
const (
	fieldTitle = "title"
	fieldBody  = "body"
)

// fieldOrder puts the fields of ready? and bug-severity? first; the others follow sorted.
var fieldOrder = []string{fieldTitle, fieldBody}

// FieldNames lists a request's field names: title and body first, then the rest sorted.
func FieldNames(fields map[string]any) []string {
	names := make([]string, 0, len(fields))
	for _, n := range fieldOrder {
		if _, ok := fields[n]; ok {
			names = append(names, n)
		}
	}
	var rest []string
	for n := range fields {
		if !slices.Contains(fieldOrder, n) {
			rest = append(rest, n)
		}
	}
	sort.Strings(rest)
	return append(names, rest...)
}

// SetArgs are jev_set's arguments, and an entry of pre_configuration.jev.answers.
type SetArgs struct {
	Point  string `json:"point"`
	Item   string `json:"item,omitempty"`
	Person string `json:"person,omitempty"`
	Answer string `json:"answer,omitempty"`
	// Confidence defaults to 0.9.
	Confidence *float64 `json:"confidence,omitempty"`
	// Error is timeout or server.
	Error string `json:"error,omitempty"`
}

// SeedConfig is pre_configuration.jev.
type SeedConfig struct {
	// Default is agree (the default): answer as the rule does, with confidence 0.9.
	Default string    `json:"default,omitempty"`
	Answers []SetArgs `json:"answers,omitempty"`
}

// CallsArgs are jev_calls' arguments.
type CallsArgs struct {
	Point     string `json:"point,omitempty"`
	SinceStep string `json:"since_step,omitempty"`
}

// Calls is jev_calls' result.
type Calls struct {
	Count    int    `json:"count"`
	Requests []Call `json:"requests"`
}

// Call is one recorded request in jev_calls' result. Item or Person is the subject.
type Call struct {
	Point  string   `json:"point"`
	Item   string   `json:"item,omitempty"`
	Person string   `json:"person,omitempty"`
	Fields []string `json:"fields"`
}

// Seed applies the merged pre_configuration.jev, decoded strictly. It replaces earlier answers.
func (f *Fake) Seed(cfg map[string]any) error {
	var c SeedConfig
	if err := decodeStrict(cfg, &c); err != nil {
		return fmt.Errorf("jev pre_configuration: %w", err)
	}
	switch c.Default {
	case "", "agree":
	default:
		return fmt.Errorf("jev pre_configuration: default %q: only agree is supported", c.Default)
	}
	answers := map[key]entry{}
	for i, a := range c.Answers {
		k, e, err := a.entry()
		if err != nil {
			return fmt.Errorf("jev pre_configuration: answers[%d]: %w", i, err)
		}
		answers[k] = e
	}
	f.mu.Lock()
	f.agree = true
	f.answers = answers
	f.mu.Unlock()
	return nil
}

// Set sets the answer (or error) for a point and subject; without a subject it applies to every
// subject of the point that has no answer of its own.
func (f *Fake) Set(a SetArgs) error {
	k, e, err := a.entry()
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.answers[k] = e
	f.mu.Unlock()
	return nil
}

func (a SetArgs) entry() (key, entry, error) {
	switch {
	case a.Point == "":
		return key{}, entry{}, errors.New("point is required")
	case a.Item != "" && a.Person != "":
		return key{}, entry{}, errors.New("item and person are exclusive")
	case (a.Answer == "") == (a.Error == ""):
		return key{}, entry{}, errors.New("give either answer or error")
	case a.Error != "" && a.Error != ErrorTimeout && a.Error != ErrorServer:
		return key{}, entry{}, fmt.Errorf("error %q: want timeout or server", a.Error)
	case a.Error != "" && a.Confidence != nil:
		return key{}, entry{}, errors.New("confidence goes with an answer, not an error")
	}
	e := entry{answer: a.Answer, confidence: DefaultConfidence, err: a.Error}
	if a.Confidence != nil {
		if *a.Confidence < 0 || *a.Confidence > 1 {
			return key{}, entry{}, fmt.Errorf("confidence %v: want 0 to 1", *a.Confidence)
		}
		e.confidence = *a.Confidence
	}
	return key{a.Point, a.Item + a.Person}, e, nil
}

// MarkStep records where a step starts, for since_step.
func (f *Fake) MarkStep(id string) {
	f.mu.Lock()
	f.marks[id] = len(f.requests)
	f.mu.Unlock()
}

// Calls returns the recorded requests of a point (all points when empty), since a step's start (all
// when empty), oldest first.
func (f *Fake) Calls(point, sinceStep string) (Calls, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	from := 0
	if sinceStep != "" {
		i, ok := f.marks[sinceStep]
		if !ok {
			return Calls{}, fmt.Errorf("since_step %q: no such step", sinceStep)
		}
		from = i
	}
	out := Calls{Requests: []Call{}}
	for _, r := range f.requests[from:] {
		if point != "" && r.Point != point {
			continue
		}
		c := Call{Point: r.Point, Fields: slices.Clone(r.Fields)}
		if strings.Contains(r.Subject, "#") {
			c.Item = r.Subject
		} else {
			c.Person = r.Subject
		}
		out.Requests = append(out.Requests, c)
	}
	out.Count = len(out.Requests)
	return out, nil
}

// Records returns every request so far, oldest first.
func (f *Fake) Records() []Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// Do runs a step tool: jev_set or jev_calls.
func (f *Fake) Do(_ context.Context, tool string, args map[string]any) (any, error) {
	switch tool {
	case ToolSet:
		var a SetArgs
		if err := decodeStrict(args, &a); err != nil {
			return nil, fmt.Errorf("%s: %w", tool, err)
		}
		if err := f.Set(a); err != nil {
			return nil, fmt.Errorf("%s: %w", tool, err)
		}
		return map[string]any{}, nil
	case ToolCalls:
		var a CallsArgs
		if err := decodeStrict(args, &a); err != nil {
			return nil, fmt.Errorf("%s: %w", tool, err)
		}
		return f.Calls(a.Point, a.SinceStep)
	}
	return nil, fmt.Errorf("fakejev: unknown tool %q", tool)
}

// decodeStrict decodes a YAML-born map into v, failing on unknown keys.
func decodeStrict(in map[string]any, v any) error {
	if in == nil {
		return nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
