package superloop

import (
	"fmt"
	"testing"
	"time"
)

func TestOwnDeliveryProblemsStayInTimeline(t *testing.T) {
	const ownPRRed = "CI red on " + overlapPRBURL
	for _, tc := range []struct {
		name, body                                      string
		unknown, closed, done, saved, overlap, wantFile bool
	}{
		{name: "own PR", body: "CI red on https://github.com/o/r/pull/8, reading saved output before fixing"},
		{name: "check and path", body: "pre-commit/goconst failed on https://github.com/o/r/pull/8 (https://github.com/o/r/actions/runs/12/job/34) in internal/superloop/problems.go and cmd/supervisorloop.go"},
		{name: "saved review park", body: "CI red on https://github.com/o/r/pull/8, rerunning", saved: true},
		{name: "claimed issue", body: "Retrying cleanup for " + pr},
		{name: "completion", body: "CI rerun for https://github.com/o/r/pull/8", done: true},
		{name: "unknown owner", body: ownPRRed, unknown: true, wantFile: true},
		{name: "closed PR", body: ownPRRed, closed: true, wantFile: true},
		{name: "overlap waiter", body: ownPRRed, overlap: true, wantFile: true},
		{name: "other PR", body: "CI red on https://github.com/o/r/pull/9", wantFile: true},
		{name: "external defect", body: "https://github.com/o/r/pull/8 fails because other/tool has a gate timeout", wantFile: true},
		{name: "external URL defect", body: "https://github.com/o/r/pull/8 fails in https://github.com/other/tool/actions/runs/9", wantFile: true},
		{name: "external completion", body: "https://github.com/o/r/pull/8 fails because other/tool has a gate timeout", done: true, wantFile: true},
		{name: "unattributed", body: "The gate times out", wantFile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Pull{State: Open, Closes: []string{pr}}
			if tc.closed {
				p.State = Closed
			}
			w := Worker{Name: worker, Serves: pr, Saved: tc.saved}
			if tc.saved {
				w.ParkedOn = overlapPRB
				w.ReviewRequested = true
			}
			if tc.overlap {
				p.Closes = nil
				w.Serves = overlapPRA
				w.ParkedOn = overlapPRB
				w.Waits = "overlap with " + overlapPRB
			}
			in := Input{Workers: []Worker{w}, Pulls: map[string]Pull{overlapPRB: p}, Events: []Event{{At: t0, Verb: VerbProblem, Agent: worker, Detail: worker + ": " + tc.body}}}
			if tc.unknown {
				in.Workers = nil
			}
			if tc.done {
				in.Events = append(in.Events, Event{At: t0, Verb: VerbDone, Agent: worker})
			}
			file := false
			for _, act := range Plan(in) {
				file = file || act.Kind == KindFile
			}
			if file != tc.wantFile {
				t.Fatalf("file = %v, want %v", file, tc.wantFile)
			}
		})
	}
}

func TestWokenWorkerWithoutClosingKeywordRetainsProgress(t *testing.T) {
	in := Input{
		Workers: []Worker{{Name: worker, Serves: pr}},
		Pulls:   map[string]Pull{overlapPRB: {State: Open}},
		Events: []Event{
			{At: t0, Verb: VerbPark, Agent: worker, Detail: worker + ": parked on " + overlapPRB + ": desk review"},
			{At: t1, Verb: VerbProblem, Agent: worker, Detail: worker + ": pre-commit/goconst red on https://github.com/o/r/pull/8"},
		},
	}
	only(t, Plan(in), KindProgress)
}

func TestLoadedDeliveryProgressDoesNotCreateGuideWork(t *testing.T) {
	in := Input{Pulls: map[string]Pull{}}
	for n := 1; n <= 12; n++ {
		name := fmt.Sprintf("worker %d", n)
		ref := fmt.Sprintf("o/r#%d", n)
		in.Workers = append(in.Workers, Worker{Name: name, ParkedOn: ref})
		in.Pulls[ref] = Pull{State: Open}
		for minute := 0; minute < 60; minute++ {
			in.Events = append(in.Events, Event{At: t0.Add(time.Duration(minute) * time.Minute), Verb: VerbProblem, Agent: name, Detail: fmt.Sprintf("%s: CI red on https://github.com/o/r/pull/%d, reading saved output", name, n)})
		}
	}
	in.Events = append(in.Events, Event{At: t0, Verb: VerbProblem, Agent: "worker 1", Detail: "worker 1: other/tool gate times out"})
	var progressActs []Action
	files := 0
	for _, act := range Plan(in) {
		switch act.Kind {
		case KindProgress:
			progressActs = append(progressActs, act)
		case KindFile:
			files++
			if act.Message != "other/tool gate times out" {
				t.Fatalf("progress escaped to guide: %+v", act)
			}
		}
	}
	if files != 1 {
		t.Fatalf("external findings = %d, want 1", files)
	}
	in.Taken = taken(t, t1, progressActs...)
	if acts := Plan(in); len(acts) != 1 || acts[0].Kind != KindFile {
		t.Fatalf("settled progress hid same-timestamp external finding: %+v", acts)
	}
}

func TestDeliveryProgressStaysSettledAfterOwnerLeaves(t *testing.T) {
	in := Input{
		Workers: []Worker{{Name: worker, Serves: pr}},
		Events:  []Event{{At: t0, Verb: VerbProblem, Agent: worker, Detail: worker + ": retrying " + pr}},
	}
	act := only(t, Plan(in), KindProgress)
	in.Taken = taken(t, t1, act)
	in.Workers = nil
	if acts := Plan(in); len(acts) != 0 {
		t.Fatalf("settled progress replayed after owner left: %+v", acts)
	}
}
