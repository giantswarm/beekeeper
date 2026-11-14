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

func TestPartOfContinuationAndContextProblemsStayInTimeline(t *testing.T) {
	const (
		contAgent  = "r 7 next-0123abcd"
		mergedRef  = "o/r#6"
		otherOpen  = "o/r#9"
		servedURL  = "https://github.com/o/r/issues/7"
		sshAgent   = "r 8 next-deadbeef"
		reviewName = "Review r 8 r1"
	)
	pulls := map[string]Pull{
		mergedRef:  {State: Merged},
		overlapPRB: {State: Open, Body: "Part of " + servedURL},
		otherOpen:  {State: Open},
	}
	for _, tc := range []struct {
		name, agent, body string
		workers           []Worker
		done, wantFile    bool
	}{
		{
			name:    "part-of PR with merged context and non-repo slash tokens",
			agent:   contAgent,
			workers: []Worker{{Name: contAgent, Issues: []string{servedURL}}},
			body:    "CI red on https://github.com/o/r/pull/8 after https://github.com/o/r/pull/6 merged, in src/index.rs, cmd/app-test, recovery/broken-candidate and x/crypto (i/o timeout, yaml/v4, os/exec)",
		},
		{
			name:    "SSH checks wait and continuation worker name",
			agent:   sshAgent,
			workers: []Worker{{Name: sshAgent, ParkedOn: overlapPRB, Waits: "CI"}},
			body:    "re-reading CI failure on " + overlapPRBURL,
		},
		{
			name:     "unowned open PR alongside owned PR still files",
			agent:    contAgent,
			workers:  []Worker{{Name: contAgent, Issues: []string{servedURL}}},
			body:     "CI red on https://github.com/o/r/pull/8 and https://github.com/o/r/pull/9",
			wantFile: true,
		},
		{
			name:     "final done report naming merged PR still files",
			agent:    contAgent,
			workers:  []Worker{{Name: contAgent, Issues: []string{servedURL}}},
			body:     "https://github.com/o/r/pull/8 overlapped https://github.com/o/r/pull/6",
			done:     true,
			wantFile: true,
		},
		{
			name:     "reviewer non-forwarded problem on reviewed PR still files",
			agent:    reviewName,
			workers:  []Worker{{Name: reviewName, Serves: overlapPRB}},
			body:     "gate slot queue stalled while checking " + overlapPRBURL,
			wantFile: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := Input{
				Workers: tc.workers,
				Pulls:   pulls,
				Events:  []Event{{At: t0, Verb: VerbProblem, Agent: tc.agent, Detail: tc.agent + ": " + tc.body}},
			}
			if tc.done {
				in.Events = append(in.Events, Event{At: t0, Verb: VerbDone, Agent: tc.agent})
			}
			file := false
			for _, act := range Plan(in) {
				file = file || act.Kind == KindFile
			}
			if file != tc.wantFile {
				t.Fatalf("file = %v, want %v (actions = %+v)", file, tc.wantFile, Plan(in))
			}
		})
	}
}

func TestUnreadCandidatePullWaitsForObservationWithoutDoubleSettling(t *testing.T) {
	ev := Event{At: t0, Verb: VerbProblem, Agent: worker, Detail: worker + ": CI red on " + overlapPRBURL}
	in := Input{
		Now:        t0,
		PullReadAt: map[string]time.Time{},
		Workers:    []Worker{{Name: worker, Serves: pr}},
		Pulls:      map[string]Pull{},
		Events:     []Event{ev},
	}
	if acts := Plan(in); len(acts) != 0 {
		t.Fatalf("settled before unread candidate pull was observed: %+v", acts)
	}

	// Once observed as the worker's Part-of PR, it settles as progress.
	ownedIn := in
	ownedIn.Pulls = map[string]Pull{overlapPRB: {State: Open, Body: "Part of https://github.com/o/r/issues/7"}}
	ownedIn.PullReadAt = map[string]time.Time{overlapPRB: t1}
	only(t, Plan(ownedIn), KindProgress)

	// An already-filed single problem never emits a second progress action later.
	filedAlready := ownedIn
	filedAlready.Taken = taken(t, t1, Action{Key: eventKey(KindFile, ev), Kind: KindFile, Agent: worker})
	if acts := Plan(filedAlready); len(acts) != 0 {
		t.Fatalf("already-filed problem double-settled as progress: %+v", acts)
	}

	// Once observed as an unowned open PR, or past the observation bound, it files.
	unownedIn := in
	unownedIn.Pulls = map[string]Pull{overlapPRB: {State: Open}}
	unownedIn.PullReadAt = map[string]time.Time{overlapPRB: t1}
	only(t, Plan(unownedIn), KindFile)

	timedOut := in
	timedOut.Now = t0.Add(6 * time.Minute)
	only(t, Plan(timedOut), KindFile)
}
