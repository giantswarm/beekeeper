package fakejev

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/jev"
	"github.com/giantswarm/beekeeper/internal/settle"
)

// Fixtures: decision points and their answers, people and items.
const (
	pointReady     = "ready?"
	pointNext      = "next?"
	answerReady    = "ready"
	answerNotReady = "not-ready"
	answerCritical = "critical"

	alice         = "alice"
	refRoadmap101 = "giantswarm/roadmap#101"
	refRoadmap102 = "giantswarm/roadmap#102"
	refOR1        = "o/r#1"

	fieldCandidates = "candidates"
)

// Keys of the seed and of the tools' arguments.
const (
	keyDefault    = "default"
	keyPoint      = "point"
	keyItem       = "item"
	keyAnswer     = "answer"
	keyConfidence = "confidence"
	keyError      = "error"
	keySinceStep  = "since_step"
)

func ready(ref, rule string) jev.Request {
	return jev.Request{
		Point: pointReady, Subject: ref, Answers: []string{answerReady, answerNotReady}, Rule: rule,
		Fields: map[string]any{fieldBody: "### Acceptance criteria\n- [ ] x", fieldTitle: "Retry transient errors"},
	}
}

func next(login, rule string) jev.Request {
	return jev.Request{
		Point: pointNext, Subject: login, Answers: []string{refRoadmap101, refRoadmap102}, Rule: rule,
		Fields: map[string]any{fieldCandidates: []string{"…"}},
	}
}

func ask(t *testing.T, f *Fake, r jev.Request) (jev.Answer, error) {
	t.Helper()
	return f.Ask(context.Background(), r)
}

func TestAnswers(t *testing.T) {
	t.Parallel()
	f := New(Options{})
	// The merged pre_configuration: the base fixture's default and a scenario's answers.
	err := f.Seed(map[string]any{
		keyDefault: "agree",
		"answers": []any{
			map[string]any{keyPoint: pointReady, keyItem: refRoadmap101, keyAnswer: answerNotReady, keyConfidence: 0.4},
			map[string]any{keyPoint: pointNext, "person": alice, keyAnswer: refRoadmap102},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		name string
		set  map[string]any
		req  jev.Request
		want jev.Answer
		kind string
	}{
		{name: "seeded low confidence", req: ready(refRoadmap101, answerReady), want: jev.Answer{Answer: answerNotReady, Confidence: 0.4}},
		{name: "seeded without confidence", req: next(alice, refRoadmap101), want: jev.Answer{Answer: refRoadmap102, Confidence: 0.9}},
		{name: "agrees by default", req: ready(refRoadmap102, answerReady), want: jev.Answer{Answer: answerReady, Confidence: 0.9}},
		{name: "agrees for another person", req: next("bob", refRoadmap101), want: jev.Answer{Answer: refRoadmap101, Confidence: 0.9}},
		{
			name: "set before the item exists",
			set:  map[string]any{keyPoint: "bug-severity?", keyItem: "giantswarm/roadmap#130", keyAnswer: answerCritical, keyConfidence: 0.9},
			req: jev.Request{Point: "bug-severity?", Subject: "giantswarm/roadmap#130", Rule: "medium",
				Answers: []string{answerCritical, "high", "medium", "low"}},
			want: jev.Answer{Answer: answerCritical, Confidence: 0.9},
		},
		{
			name: "timeout fails at once",
			set:  map[string]any{keyPoint: pointReady, keyItem: refRoadmap101, keyError: ErrorTimeout},
			req:  ready(refRoadmap101, answerReady),
			kind: jev.KindTimeout,
		},
		{
			name: "server error",
			set:  map[string]any{keyPoint: pointReady, keyItem: refRoadmap102, keyError: ErrorServer},
			req:  ready(refRoadmap102, answerReady),
			kind: jev.KindServer,
		},
		{
			name: "a point-wide answer applies to subjects without their own",
			set:  map[string]any{keyPoint: pointNext, keyAnswer: refRoadmap101, keyConfidence: 1},
			req:  next("bob", refRoadmap102),
			want: jev.Answer{Answer: refRoadmap101, Confidence: 1},
		},
		{name: "the subject's own answer wins", req: next(alice, refRoadmap101), want: jev.Answer{Answer: refRoadmap102, Confidence: 0.9}},
	}
	for _, s := range steps {
		if s.set != nil {
			if _, err := f.Do(context.Background(), ToolSet, s.set); err != nil {
				t.Fatalf("%s: jev_set: %v", s.name, err)
			}
		}
		start := time.Now()
		got, err := ask(t, f, s.req)
		if time.Since(start) > time.Second {
			t.Errorf("%s: the answer took %s", s.name, time.Since(start))
		}
		if s.kind != "" {
			if jev.ErrorKind(err) != s.kind {
				t.Errorf("%s: error %v, want kind %s", s.name, err, s.kind)
			}
			continue
		}
		if err != nil || got != s.want {
			t.Errorf("%s: Ask = %+v, %v; want %+v", s.name, got, err, s.want)
		}
	}
	if n := len(f.Records()); n != len(steps) {
		t.Errorf("recorded %d requests, want %d", n, len(steps))
	}
}

func TestCalls(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC))
	f := New(Options{Clock: clk})
	_, _ = ask(t, f, ready(refRoadmap101, answerReady))
	_, _ = ask(t, f, ready(refRoadmap102, answerReady))
	_, _ = ask(t, f, next(alice, refRoadmap101))
	f.MarkStep("body-edited")
	clk.Advance(time.Minute)
	_, _ = ask(t, f, ready(refRoadmap101, answerReady))

	calls := func(args map[string]any) string {
		t.Helper()
		res, err := f.Do(context.Background(), ToolCalls, args)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	got := calls(map[string]any{keyPoint: pointReady})
	want := `{"count":3,"requests":[` +
		`{"point":"ready?","item":"giantswarm/roadmap#101","fields":["title","body"]},` +
		`{"point":"ready?","item":"giantswarm/roadmap#102","fields":["title","body"]},` +
		`{"point":"ready?","item":"giantswarm/roadmap#101","fields":["title","body"]}]}`
	if got != want {
		t.Errorf("jev_calls ready? =\n%s\nwant\n%s", got, want)
	}
	for _, s := range []string{alice, "bob", "assignees"} {
		if strings.Contains(strings.ToLower(got), s) {
			t.Errorf("jev_calls ready? contains %q", s)
		}
	}
	if got, want := calls(map[string]any{keyPoint: pointNext}),
		`{"count":1,"requests":[{"point":"next?","person":"alice","fields":["candidates"]}]}`; got != want {
		t.Errorf("jev_calls next? = %s, want %s", got, want)
	}
	if got, want := calls(map[string]any{keyPoint: pointReady, keySinceStep: "body-edited"}),
		`{"count":1,"requests":[{"point":"ready?","item":"giantswarm/roadmap#101","fields":["title","body"]}]}`; got != want {
		t.Errorf("jev_calls since_step = %s, want %s", got, want)
	}
	if got, want := calls(map[string]any{keySinceStep: "body-edited", keyPoint: pointNext}), `{"count":0,"requests":[]}`; got != want {
		t.Errorf("jev_calls none = %s, want %s", got, want)
	}
	if got := calls(nil); !strings.HasPrefix(got, `{"count":4,`) {
		t.Errorf("jev_calls without filters = %s, want all 4", got)
	}
	recs := f.Records()
	if !recs[0].At.Equal(time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)) || !recs[3].At.Equal(time.Date(2026, 11, 16, 9, 1, 0, 0, time.UTC)) {
		t.Errorf("records not stamped with the clock: %v, %v", recs[0].At, recs[3].At)
	}
}

func TestStrict(t *testing.T) {
	t.Parallel()
	f := New(Options{})
	bad := []struct {
		name string
		do   func() error
	}{
		{"unknown jev_set key", func() error {
			_, err := f.Do(context.Background(), ToolSet, map[string]any{keyPoint: pointReady, keyItem: refOR1, keyAnswer: answerReady, "subject": "x"})
			return err
		}},
		{"no point", func() error {
			_, err := f.Do(context.Background(), ToolSet, map[string]any{keyItem: refOR1, keyAnswer: answerReady})
			return err
		}},
		{"item and person", func() error {
			_, err := f.Do(context.Background(), ToolSet, map[string]any{keyPoint: pointReady, keyItem: refOR1, "person": alice, keyAnswer: answerReady})
			return err
		}},
		{"answer and error", func() error {
			_, err := f.Do(context.Background(), ToolSet, map[string]any{keyPoint: pointReady, keyAnswer: answerReady, keyError: ErrorTimeout})
			return err
		}},
		{"neither answer nor error", func() error {
			_, err := f.Do(context.Background(), ToolSet, map[string]any{keyPoint: pointReady})
			return err
		}},
		{"unknown error", func() error {
			_, err := f.Do(context.Background(), ToolSet, map[string]any{keyPoint: pointReady, keyError: "slow"})
			return err
		}},
		{"confidence out of range", func() error {
			_, err := f.Do(context.Background(), ToolSet, map[string]any{keyPoint: pointReady, keyAnswer: answerReady, keyConfidence: 1.5})
			return err
		}},
		{"unknown jev_calls key", func() error {
			_, err := f.Do(context.Background(), ToolCalls, map[string]any{keyItem: refOR1})
			return err
		}},
		{"unknown step", func() error {
			_, err := f.Do(context.Background(), ToolCalls, map[string]any{keySinceStep: "nope"})
			return err
		}},
		{"unknown tool", func() error {
			_, err := f.Do(context.Background(), "jev_reset", nil)
			return err
		}},
		{"unknown seed key", func() error { return f.Seed(map[string]any{keyDefault: "agree", "latency": "1s"}) }},
		{"unknown default", func() error { return f.Seed(map[string]any{keyDefault: "disagree"}) }},
		{"bad seeded answer", func() error {
			return f.Seed(map[string]any{"answers": []any{map[string]any{keyPoint: pointReady, keyItem: refOR1}}})
		}},
	}
	for _, b := range bad {
		if err := b.do(); err == nil {
			t.Errorf("%s: no error", b.name)
		}
	}
	// Nothing bad was applied: the fake still agrees.
	if a, err := ask(t, f, ready(refOR1, answerReady)); err != nil || a.Answer != answerReady {
		t.Errorf("after rejected configuration: %+v, %v", a, err)
	}
}

func TestTrackedAndCanceled(t *testing.T) {
	t.Parallel()
	tr := settle.New()
	f := New(Options{Tracker: tr})
	if _, err := ask(t, f, ready(refOR1, answerReady)); err != nil {
		t.Fatal(err)
	}
	if idle, pending := tr.Idle(); !idle {
		t.Errorf("tracker busy after the call: %v", pending)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Ask(ctx, ready(refOR1, answerReady)); jev.ErrorKind(err) != jev.KindCanceled {
		t.Errorf("canceled call: %v", err)
	}
}

func TestFieldNames(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in   map[string]any
		want []string
	}{
		{nil, []string{}},
		{map[string]any{fieldBody: 1, fieldTitle: 1}, []string{fieldTitle, fieldBody}},
		{map[string]any{"footprints": 1, fieldBody: 1, fieldCandidates: 1}, []string{fieldBody, fieldCandidates, "footprints"}},
	} {
		if got := FieldNames(tt.in); !slices.Equal(got, tt.want) {
			t.Errorf("FieldNames(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
