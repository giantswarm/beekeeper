package superloop

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

const deliveryDropReason = "manual end"

const deliveryFirstHead = "aaa"
const deliveryDeskAuthor = "desk"
const deliveryCombinedPR = "o/r#30"

const deliveryIssue = "o/r#10"
const deliveryPR = "o/r#20"

func deliveryInput() Input {
	return Input{Now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), Pulls: map[string]Pull{deliveryPR: {Ref: deliveryPR, State: Open, Head: deliveryFirstHead, Author: deliveryDeskAuthor, Closes: []string{deliveryIssue}}}, Desk: Desk{Authors: []string{deliveryDeskAuthor}}, Workers: []Worker{{Name: "implementer", Serves: deliveryIssue, ParkedOn: deliveryPR, ParkedHead: deliveryFirstHead}}}
}

// A never-owned repository named github must not exclude ordinary GitHub
// workflows in a different, owned repository. Start with an unread PR seed,
// then resolve it to its issue and plan the review from the same observation.
func TestWorkflowDeliveryResolvesAndRequestsReview(t *testing.T) {
	for _, title := range []string{"feat: add palette grades", "chore(deps): update GitHub artifact actions"} {
		t.Run(title, func(t *testing.T) {
			in := deliveryInput()
			in.Desk.Never = []string{"upstream/github"}
			in.OpenPulls = []string{deliveryPR}
			p := in.Pulls[deliveryPR]
			in.Pulls = nil
			seed := Deliveries(in, nil)
			if len(seed) != 1 || seed[0].Step != state.DeliveryUnknown {
				t.Fatalf("unread seed: %+v", seed)
			}
			p.Title, p.Branch = title, "renovate/major-github-artifact-actions"
			p.Files, p.ChangedFiles = []string{".github/workflows/proof.yml"}, 1
			p.Green = true
			in.Pulls = map[string]Pull{deliveryPR: p}
			in.Workers[0].ReviewRequested = true
			in.Workers[0].ParkedAt = in.Now
			for range 2 {
				in.Now = in.Now.Add(time.Minute)
				got := Deliveries(in, seed)
				if len(got) != 1 || got[0].Ref != deliveryIssue || got[0].OutsideDesk || got[0].Step != state.DeliveryReviewQueued {
					t.Fatalf("owned workflow lost its delivery: %+v", got)
				}
				a := only(t, Plan(in), KindReview)
				if a.Ref != deliveryPR || a.Head != p.Head {
					t.Fatalf("wrong review: %+v", a)
				}
				seed = got
			}
		})
	}
}
func TestDeliveryLifecycle(t *testing.T) {
	in := deliveryInput()
	var prev []Delivery
	cases := []struct {
		name, step string
		change     func()
	}{
		{"start", state.DeliveryImplementing, func() {}},
		{"request", state.DeliveryCI, func() { in.Workers[0].ReviewRequested = true }},
		{"green", state.DeliveryReviewQueued, func() { p := in.Pulls[deliveryPR]; p.Green = true; in.Pulls[deliveryPR] = p }},
		{"review", state.DeliveryReviewing, func() {
			in.Taken = append(in.Taken, Taken{Kind: KindReview, Ref: deliveryPR, Key: reviewKey(deliveryPR, deliveryFirstHead), At: in.Now})
		}},
		{"findings", state.DeliveryFixing, func() {
			p := in.Pulls[deliveryPR]
			p.Reviews = []Review{{Head: deliveryFirstHead, Verdict: ChangesRequested, At: in.Now}}
			in.Pulls[deliveryPR] = p
		}},
		{"fix pushed", state.DeliveryCI, func() {
			p := in.Pulls[deliveryPR]
			p.Head = "bbb"
			p.Green = false
			in.Pulls[deliveryPR] = p
			in.Workers[0].ParkedHead = "bbb"
		}},
		{"fix green", state.DeliveryReviewQueued, func() { p := in.Pulls[deliveryPR]; p.Green = true; in.Pulls[deliveryPR] = p }},
		{"second round", state.DeliveryReviewing, func() {
			in.Taken = append(in.Taken, Taken{Kind: KindReview, Ref: deliveryPR, Key: reviewKey(deliveryPR, "bbb"), At: in.Now})
		}},
		{"approved lane", state.DeliveryMerging, func() {
			in.Taken = append(in.Taken, Taken{Kind: KindGo, Ref: deliveryPR, Key: goKey(deliveryPR, "bbb"), At: in.Now})
		}},
		{"merged", state.DeliveryReleasing, func() { p := in.Pulls[deliveryPR]; p.State = Merged; in.Pulls[deliveryPR] = p }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in.Now = in.Now.Add(time.Minute)
			tc.change()
			got := Deliveries(in, prev)
			if len(got) != 1 || got[0].Step != tc.step {
				t.Fatalf("got %+v, want %s", got, tc.step)
			}
			if len(prev) > 0 && !got[0].Started.Equal(prev[0].Started) {
				t.Fatal("start clock changed")
			}
			prev = got
		})
	}
}
func TestDeliveryBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Input)
		step   string
		count  int
	}{
		{"red", func(in *Input) { p := in.Pulls[deliveryPR]; p.Red = []Check{{Name: "test"}}; in.Pulls[deliveryPR] = p }, state.DeliveryFixing, 1},
		{"conflict", func(in *Input) {
			p := in.Pulls[deliveryPR]
			p.Mergeable = mergeableConflicting
			in.Pulls[deliveryPR] = p
		}, state.DeliveryFixing, 1},
		{"two issues", func(in *Input) {
			p := in.Pulls[deliveryPR]
			p.Closes = append(p.Closes, "o/r#11")
			in.Pulls[deliveryPR] = p
		}, state.DeliveryImplementing, 2},
		{"foreign", func(in *Input) { p := in.Pulls[deliveryPR]; p.Author = "someone-else"; in.Pulls[deliveryPR] = p }, "", 0},
		{"closed continuing", func(in *Input) { p := in.Pulls[deliveryPR]; p.State = Closed; in.Pulls[deliveryPR] = p }, state.DeliveryImplementing, 1},
		{"closed abandoned", func(in *Input) {
			p := in.Pulls[deliveryPR]
			p.State = Closed
			in.Pulls[deliveryPR] = p
			in.Workers = nil
		}, state.DeliveryDropped, 1},
		{"closed done worker", func(in *Input) {
			p := in.Pulls[deliveryPR]
			p.State = Closed
			in.Pulls[deliveryPR] = p
			in.Workers[0].Done = true
		}, state.DeliveryDropped, 1},
		{"issue-less", func(in *Input) { p := in.Pulls[deliveryPR]; p.Closes = nil; in.Pulls[deliveryPR] = p; in.Workers = nil }, state.DeliveryImplementing, 1},
		{"saved request", func(in *Input) { in.Workers[0].Saved = true; in.Workers[0].ReviewRequested = true }, state.DeliveryCI, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := deliveryInput()
			tc.change(&in)
			got := Deliveries(in, nil)
			if len(got) != tc.count {
				t.Fatalf("got %+v", got)
			}
			if len(got) > 0 && got[0].Step != tc.step {
				t.Fatalf("got %+v, want %s", got, tc.step)
			}
		})
	}
}
func TestDeliveryFailedReadAndSeed(t *testing.T) {
	in := deliveryInput()
	in.OpenPulls = []string{deliveryPR, "o/r#21"}
	first := Deliveries(in, nil)
	if len(first) != 2 {
		t.Fatalf("seed: %+v", first)
	}
	in.Pulls = nil
	in.Workers = nil
	in.Now = in.Now.Add(time.Hour)
	got := Deliveries(in, first)
	if !reflect.DeepEqual(got, first) {
		t.Fatalf("failed read changed observations: %+v -> %+v", first, got)
	}
	if got[1].Step != state.DeliveryUnknown || !got[1].Active() {
		t.Fatalf("unread seed not counted: %+v", got)
	}
}
func TestDeliveryAbsorbAndReopen(t *testing.T) {
	in := deliveryInput()
	first := Deliveries(in, nil)
	in.Workers = nil
	p := in.Pulls[deliveryPR]
	p.State = Closed
	in.Pulls[deliveryPR] = p
	dropped := Deliveries(in, first)
	if dropped[0].Step != state.DeliveryDropped {
		t.Fatal(dropped)
	}
	in.Now = in.Now.Add(time.Hour)
	p.State = Open
	in.Pulls[deliveryPR] = p
	reopened := Deliveries(in, dropped)
	if reopened[0].Step != state.DeliveryImplementing || !reopened[0].Started.Equal(in.Now) {
		t.Fatal(reopened)
	}
	p.State = Closed
	in.Pulls[deliveryPR] = p
	in.Pulls[deliveryCombinedPR] = Pull{Ref: deliveryCombinedPR, State: Open, Head: "ccc", Author: deliveryDeskAuthor, Absorbs: []string{deliveryPR}}
	in.OpenPulls = []string{deliveryCombinedPR}
	absorbed := Deliveries(in, reopened)
	if len(absorbed) != 1 || len(absorbed[0].Pulls) != 2 || absorbed[0].Step == state.DeliveryDropped {
		t.Fatalf("absorption: %+v", absorbed)
	}
}
func TestDeliveryResearchAndClosedIssue(t *testing.T) {
	in := deliveryInput()
	in.Pulls = nil
	in.Workers = nil
	prev := []Delivery{{Ref: deliveryIssue, Step: state.DeliveryImplementing, Started: in.Now.Add(-time.Hour), Since: in.Now.Add(-time.Hour), Agent: "researcher"}}
	for _, closed := range []bool{false, true} {
		if closed {
			in.Events = nil
			in.ClosedIssues = map[string]bool{deliveryIssue: true}
		} else {
			in.Events = []Event{{Verb: VerbDone, Agent: "researcher", At: in.Now}}
		}
		got := Deliveries(in, prev)
		if got[0].Step != state.DeliveryDropped {
			t.Fatal(got)
		}
	}
}
func TestDeliveryDoesNotMutatePreviousOrPlan(t *testing.T) {
	in := deliveryInput()
	prev := Deliveries(in, nil)
	before := Deliveries(in, nil)
	plan := Plan(in)
	in.Pulls[deliveryCombinedPR] = Pull{Ref: deliveryCombinedPR, Author: deliveryDeskAuthor, State: Open, Absorbs: []string{deliveryPR}}
	_ = Deliveries(in, prev)
	if !reflect.DeepEqual(prev, before) {
		t.Fatalf("mutated prev: %+v", prev)
	}
	delete(in.Pulls, deliveryCombinedPR)
	if !reflect.DeepEqual(plan, Plan(in)) {
		t.Fatal("derivation changed planner")
	}
}

func TestDeliveryClosedIssueDropsEvenWithContinuingWorker(t *testing.T) {
	in := deliveryInput()
	in.Pulls = nil
	in.ClosedIssues = map[string]bool{deliveryIssue: true}
	prev := []Delivery{{Ref: deliveryIssue, Step: state.DeliveryImplementing, Since: in.Now, Started: in.Now, Agent: in.Workers[0].Name}}
	got := Deliveries(in, prev)
	if got[0].Step != state.DeliveryDropped {
		t.Fatal(got)
	}
}
func TestDeliveryDoneRosterDropsResearchWithoutEventHistory(t *testing.T) {
	in := deliveryInput()
	in.Pulls = nil
	in.Workers[0].Done = true
	prev := []Delivery{{Ref: deliveryIssue, Step: state.DeliveryImplementing, Since: in.Now, Started: in.Now, Agent: in.Workers[0].Name}}
	got := Deliveries(in, prev)
	if got[0].Step != state.DeliveryDropped {
		t.Fatal(got)
	}
}

func TestDeliveryManualDropSurvivesSameHeadRestartAndResumesNewHead(t *testing.T) {
	in := deliveryInput()
	prev := Deliveries(in, nil)
	prev[0].Step = state.DeliveryDropped
	prev[0].DropReason = deliveryDropReason
	prev[0].DroppedHeads = map[string]string{deliveryPR: deliveryFirstHead}
	for range 2 {
		prev = Deliveries(in, prev)
		if prev[0].Active() {
			t.Fatalf("same head reopened: %+v", prev)
		}
	}
	saved := in.Pulls
	in.Pulls = nil
	prev = Deliveries(in, prev)
	if prev[0].Active() {
		t.Fatal("unread reopened")
	}
	in.Pulls = saved
	p := in.Pulls[deliveryPR]
	p.Head = "new"
	in.Pulls[deliveryPR] = p
	in.Now = in.Now.Add(time.Hour)
	got := Deliveries(in, prev)
	if !got[0].Active() || got[0].DropReason != "" || !got[0].Started.Equal(in.Now) || got[0].Heads[deliveryPR] != "new" {
		t.Fatalf("new head did not reopen: %+v", got)
	}
	if prev[0].Heads[deliveryPR] != deliveryFirstHead {
		t.Fatal("derivation mutated previous snapshot")
	}
}

func TestDeliveryDropIgnoresPreDropObservations(t *testing.T) {
	in := deliveryInput()
	prev := Deliveries(in, nil)
	prev[0].Step = state.DeliveryDropped
	prev[0].DropReason = deliveryDropReason
	prev[0].DropAt = in.Now.Add(time.Minute)
	prev[0].Since = prev[0].DropAt
	prev[0].DroppedHeads = map[string]string{deliveryPR: "new-actual-head"}
	// An old pass finishing after the command may not resurrect its old head.
	got := Deliveries(in, prev)
	if got[0].Active() {
		t.Fatal("old pass reopened drop")
	}
	in.PullReadAt = map[string]time.Time{deliveryPR: in.Now}
	in.Now = in.Now.Add(2 * time.Minute)
	got = Deliveries(in, prev)
	if got[0].Active() {
		t.Fatal("old cached read reopened drop")
	}
	in.PullReadAt[deliveryPR] = in.Now
	got = Deliveries(in, prev)
	if !got[0].Active() {
		t.Fatal("new observation did not resume changed head")
	}
}

func TestDeliveryDroppedPRSeedPreservesTombstoneWhenLinked(t *testing.T) {
	in := deliveryInput()
	prev := []Delivery{{Ref: deliveryPR, Pulls: []string{deliveryPR}, Step: state.DeliveryDropped, Since: in.Now, Started: in.Now, DropReason: deliveryDropReason, DroppedHeads: map[string]string{deliveryPR: deliveryFirstHead}}}
	got := Deliveries(in, prev)
	if len(got) != 1 || got[0].Ref != deliveryIssue || got[0].Active() {
		t.Fatalf("link resurrected dropped seed: %+v", got)
	}
}

func TestReviewerPublishedFixesNeedRepair(t *testing.T) {
	in := deliveryInput()
	in.Workers[0].ReviewRequested = true
	in.Taken = []Taken{{Kind: KindReview, Ref: deliveryPR, Key: reviewKey(deliveryPR, deliveryFirstHead), At: in.Now}}
	p := in.Pulls[deliveryPR]
	p.Head = "reviewer-fix"
	p.Reviews = []Review{{Head: p.Head, Verdict: Approved, At: in.Now.Add(time.Minute)}}
	in.Pulls[deliveryPR] = p
	got := Deliveries(in, nil)
	if len(got) != 1 || got[0].Step != state.DeliveryFixing {
		t.Fatalf("reviewer fixes with pending CI: %+v", got)
	}
}

func TestLinkedDeliveryOutsideOwnershipDoesNotCount(t *testing.T) {
	in := deliveryInput()
	prev := Deliveries(in, nil)
	in.Desk.Never = []string{"o/r"}
	next := Deliveries(in, prev)
	if len(next) != 1 || !next[0].OutsideDesk {
		t.Fatalf("linked delivery did not retain ownership exclusion: %+v", next)
	}
	in.Desk.Never = nil
	next = Deliveries(in, next)
	if len(next) != 1 || next[0].OutsideDesk {
		t.Fatalf("ownership recovery not observed: %+v", next)
	}
}

func TestDeliveriesLargeScaleAvoidsQuadraticOwnershipScan(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	never := make([]string, 0, 50)
	for i := 1; i <= 50; i++ {
		never = append(never, fmt.Sprintf("o/never-repo-%d", i))
	}
	files := make([]string, 0, 50)
	for i := 1; i <= 50; i++ {
		files = append(files, fmt.Sprintf("pkg/component%d/service_handler.go", i))
	}
	pulls := make(map[string]Pull, 150)
	for i := 1; i <= 150; i++ {
		ref := fmt.Sprintf("o/r#%d", 1000+i)
		pulls[ref] = Pull{
			Ref:          ref,
			State:        Open,
			Head:         fmt.Sprintf("head-%d", i),
			Author:       deliveryDeskAuthor,
			Title:        fmt.Sprintf("feat: update component %d behavior", i),
			Branch:       fmt.Sprintf("feat/component-%d", i),
			Files:        files,
			ChangedFiles: len(files),
			Closes:       []string{fmt.Sprintf("o/r#%d", i)},
		}
	}
	prev := make([]Delivery, 0, 500)
	for i := 1; i <= 500; i++ {
		issueRef := fmt.Sprintf("o/r#%d", i)
		prRef := fmt.Sprintf("o/r#%d", 1000+((i-1)%150)+1)
		prev = append(prev, Delivery{
			Ref:     issueRef,
			Pulls:   []string{prRef},
			Step:    state.DeliveryImplementing,
			Since:   now.Add(-time.Hour),
			Started: now.Add(-2 * time.Hour),
		})
	}
	in := Input{
		Now:   now,
		Pulls: pulls,
		Desk:  Desk{Authors: []string{deliveryDeskAuthor}, Never: never},
	}
	start := time.Now()
	got := Deliveries(in, prev)
	elapsed := time.Since(start)
	if len(got) != 500 {
		t.Fatalf("got %d deliveries, want 500", len(got))
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Deliveries took %s (> 2s); quadratic ownership or uncached regex regression", elapsed)
	}
}

func TestAbandonedDeliverySurvivesMergedPullObservation(t *testing.T) {
	in := deliveryInput()
	p := in.Pulls[deliveryPR]
	p.State = Merged
	in.Pulls[deliveryPR] = p
	in.Workers = nil
	prev := []Delivery{{Ref: deliveryIssue, Pulls: []string{deliveryPR}, Agent: "implementer", Step: state.DeliveryDropped, Abandoned: true, Started: in.Now.Add(-time.Hour), Since: in.Now}}
	got := Deliveries(in, prev)
	if len(got) != 1 || got[0].Active() || !got[0].Abandoned {
		t.Fatalf("reopened abandoned release: %+v", got)
	}
	in.Now = in.Now.Add(time.Hour)
	in.Workers = []Worker{{Name: "replacement", Serves: deliveryIssue}}
	got = Deliveries(in, got)
	if len(got) != 1 || !got[0].Active() || got[0].Abandoned || !got[0].Started.Equal(in.Now) {
		t.Fatalf("new assignment lost: %+v", got)
	}
}

// Ownership alone must not turn an untouched external PR into desk WIP.
func TestDeliveryPassivePullAdoption(t *testing.T) {
	in := deliveryInput()
	in.Workers = nil
	p := in.Pulls[deliveryPR]
	p.Closes = nil
	in.Pulls[deliveryPR] = p
	in.OpenPulls = []string{deliveryPR}
	got := Deliveries(in, nil)
	if len(got) != 1 || got[0].Origin != state.DeliveryOriginObserved {
		t.Fatalf("external PR adopted: %+v", got)
	}
	// Legacy records and restarted observations get the same classification.
	got[0].Origin = ""
	got = Deliveries(in, got)
	got = Deliveries(in, got)
	if got[0].Origin != state.DeliveryOriginObserved {
		t.Fatalf("passive PR changed on restart: %+v", got)
	}
	// Failed reads preserve the evidence rather than claiming work began.
	saved := in.Pulls
	in.Pulls = nil
	got = Deliveries(in, got)
	if got[0].Origin != state.DeliveryOriginObserved {
		t.Fatal(got)
	}
	in.Pulls = saved
	p.Head = "new-work"
	in.Pulls[deliveryPR] = p
	got = Deliveries(in, got)
	got = Deliveries(in, got)
	if got[0].Origin != state.DeliveryOriginStarted {
		t.Fatalf("new commit not durably adopted: %+v", got)
	}
	in.Workers = deliveryInput().Workers
	in.Workers[0].Serves = deliveryPR
	got = Deliveries(in, nil)
	if got[0].Origin != state.DeliveryOriginStarted {
		t.Fatalf("worker PR not adopted: %+v", got)
	}
}
