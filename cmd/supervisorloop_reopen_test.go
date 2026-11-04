package cmd

import (
	"testing"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func TestReopenEpisodeActionReaders(t *testing.T) {
	key := "go " + loopPR + "@" + loopHead + " episode reopen cycle2"
	worker := state.Party{Name: loopWorker}
	goEvent := event(loopParty, loopGoVerb, "[%s] %s: merge go", key, worker.Name)
	if !recordedMergeGo([]state.Event{goEvent}, worker, loopPR, loopHead) {
		t.Fatal("episode go denied")
	}
	for _, endKey := range []string{"end " + loopPR + " episode reopen cycle2", "end " + loopPR + " merged episode reopen cycle2"} {
		end := event(loopParty, loopEndVerb, "[%s] tracking ends", endKey)
		if recordedMergeGo([]state.Event{goEvent, end}, worker, loopPR, loopHead) {
			t.Fatal("ended episode retained merge authority")
		}
	}
	if got := redChecks("red " + loopPR + "@" + loopHead + " park: lint episode reopen cycle2"); got != "lint" {
		t.Fatalf("red checks = %q", got)
	}
	r := newLoopRig(t)
	if _, err := (&supervisorLoop{a: r.a}).act(t.Context(), &state.State{}, superloop.Action{Kind: superloop.KindReopen, Ref: loopPR, Message: "open"}); err != nil {
		t.Fatal(err)
	}
}
