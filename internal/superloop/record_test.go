package superloop

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

func replayRecording(t *testing.T, in Input) Input {
	t.Helper()
	data, err := json.Marshal(Recording(in))
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{pr, head1, worker} {
		if strings.Contains(string(data), private) {
			t.Fatalf("recording retained %q", private)
		}
	}
	var out Input
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRecordingPreservesReviewAndMergeLedger(t *testing.T) {
	in := Input{Workers: parked(t0), Pulls: pull(head1, true), Rounds: 1}
	start := only(t, Plan(in), KindReview)
	in.Taken = taken(t, t0, start)
	if got := Plan(replayRecording(t, in)); len(got) != 0 {
		t.Fatalf("recording restarted an in-flight review: %+v", got)
	}
	p := in.Pulls[pr]
	p.Reviews = []Review{{ID: "R1", Head: head1, Verdict: Approved, At: t1}}
	in.Pulls[pr] = p
	only(t, Plan(replayRecording(t, in)), KindGo)
	goAction := only(t, Plan(in), KindGo)
	in.Taken = append(in.Taken, taken(t, t1.Add(time.Minute), goAction)...)
	if got := Plan(replayRecording(t, in)); len(got) != 0 {
		t.Fatalf("recording repeated a completed merge go: %+v", got)
	}
}

func TestRecordingPreservesSweepAndEpisodeKeys(t *testing.T) {
	in := Input{Pulls: pull(head1, true), Taken: []Taken{{
		Key:  sweepKey(KindReview, pr, head1, t0) + episodeTag + t1.Format(time.RFC3339Nano),
		Kind: KindReview, Ref: pr, Agent: worker, At: t1,
	}}}
	out := replayRecording(t, in)
	tk := out.Taken[0]
	if !IsSweep(tk.Key) || KeyHead(tk.Key) != out.Pulls[tk.Ref].Head {
		t.Fatalf("lost sweep or head: %+v", tk)
	}
	if _, episode := splitEpisode(tk.Key); episode != t1.Format(time.RFC3339Nano) {
		t.Fatalf("lost tracking episode: %+v", tk)
	}
}

func TestRecordingPreservesDeliveryStates(t *testing.T) {
	for _, step := range []string{state.DeliveryUnknown, state.DeliveryImplementing, state.DeliveryCI, state.DeliveryReviewQueued, state.DeliveryReviewing, state.DeliveryFixing, state.DeliveryMerging, state.DeliveryReleasing, state.DeliveryDone, state.DeliveryDropped} {
		in := state.Delivery{Ref: pr, Origin: state.DeliveryOriginObserved, Step: step}
		data, err := json.Marshal(Recording(in))
		if err != nil {
			t.Fatal(err)
		}
		var out state.Delivery
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatal(err)
		}
		if out.Origin != in.Origin || out.Step != in.Step || out.Active() != in.Active() {
			t.Fatalf("recording changed delivery state: %+v -> %+v", in, out)
		}
	}
}
