package superloop

import (
	"fmt"
	"testing"
	"time"
)

func TestOwnDeliveryProblemsStayInTimeline(t *testing.T) {
	const issue = "o/r#7"
	const pullRef = "o/r#8"
	for _, tc := range []struct {
		name, body                      string
		unknown, closed, done, wantFile bool
	}{
		{name: "own PR", body: "CI red on https://github.com/o/r/pull/8, reading saved output before fixing"},
		{name: "claimed issue", body: "Retrying cleanup for o/r#7"},
		{name: "completion", body: "CI rerun for https://github.com/o/r/pull/8", done: true},
		{name: "unknown owner", body: "CI red on https://github.com/o/r/pull/8", unknown: true, wantFile: true},
		{name: "closed PR", body: "CI red on https://github.com/o/r/pull/8", closed: true, wantFile: true},
		{name: "other PR", body: "CI red on https://github.com/o/r/pull/9", wantFile: true},
		{name: "external defect", body: "https://github.com/o/r/pull/8 fails because other/tool has a gate timeout", wantFile: true},
		{name: "external completion", body: "https://github.com/o/r/pull/8 fails because other/tool has a gate timeout", done: true, wantFile: true},
		{name: "unattributed", body: "The gate times out", wantFile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Pull{State: Open, Closes: []string{issue}}
			if tc.closed {
				p.State = Closed
			}
			in := Input{Workers: []Worker{{Name: worker, Serves: issue}}, Pulls: map[string]Pull{pullRef: p}, Events: []Event{{At: t0, Verb: VerbProblem, Agent: worker, Detail: worker + ": " + tc.body}}}
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
	files := 0
	for _, act := range Plan(in) {
		if act.Kind == KindFile {
			files++
			if act.Message != "other/tool gate times out" {
				t.Fatalf("progress escaped to guide: %+v", act)
			}
		}
	}
	if files != 1 {
		t.Fatalf("external findings = %d, want 1", files)
	}
}

func TestDeliveryProgressStaysSettledAfterOwnerLeaves(t *testing.T) {
	in := Input{
		Workers: []Worker{{Name: worker, Serves: "o/r#7"}},
		Events:  []Event{{At: t0, Verb: VerbProblem, Agent: worker, Detail: worker + ": retrying o/r#7"}},
	}
	act := only(t, Plan(in), KindProgress)
	in.Taken = taken(t, t1, act)
	in.Workers = nil
	if acts := Plan(in); len(acts) != 0 {
		t.Fatalf("settled progress replayed after owner left: %+v", acts)
	}
}
