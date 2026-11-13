package superloop

import (
	"slices"
	"testing"
)

func TestCurrentFindingsSettleBeforeDoneEvenOnOwnedIssue(t *testing.T) {
	const report = "https://github.com/o/r/issues/1: Queue stalls\n\nThe queue stalls when a lease expires."
	problem := Event{At: t0, Verb: VerbProblem, Agent: worker, Detail: worker + ": " + report}
	done := Event{At: t0, Verb: VerbDone, Agent: worker}
	in := Input{Workers: []Worker{{Name: worker, Serves: "o/r#1"}}, Events: []Event{problem, done}}
	acts := Plan(in)
	if slices.ContainsFunc(acts, func(a Action) bool { return a.Kind == KindDone || a.Kind == KindProgress }) {
		t.Fatalf("retired before placement: %+v", acts)
	}
	file := only(t, acts, KindFile)
	if file.Message != report {
		t.Fatalf("lost description: %+v", file)
	}
	in.Taken = []Taken{{Key: file.Key, Kind: KindFile}}
	if acts = Plan(in); slices.ContainsFunc(acts, func(a Action) bool { return a.Kind == KindFile }) {
		t.Fatalf("replayed: %+v", acts)
	}
	only(t, acts, KindDone)
}

func TestFinalReportPlacesEveryFindingAtTheSameTimestamp(t *testing.T) {
	in := Input{Events: []Event{
		{At: t0, Verb: VerbProblem, Agent: worker, Detail: worker + ": example/app: Queue stalls\n\nQueue evidence"},
		{At: t0, Verb: VerbProblem, Agent: worker, Detail: worker + ": example/app: Lost heartbeat\n\nHeartbeat evidence"},
		{At: t0, Verb: VerbDone, Agent: worker},
	}}
	acts := Plan(in)
	files := []Action{}
	for _, act := range acts {
		if act.Kind == KindFile {
			files = append(files, act)
		}
	}
	if len(files) != 2 || files[0].Key == files[1].Key {
		t.Fatalf("findings lost: %+v", acts)
	}
	for _, act := range files {
		in.Taken = append(in.Taken, Taken{Key: act.Key, Kind: KindFile})
	}
	only(t, Plan(in), KindDone)
}
