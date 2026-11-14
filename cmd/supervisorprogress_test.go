package cmd

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/gate"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// TestValidateProgressTaskIgnoresCase checks that the stalled item's pace
// exception admits the item's own brief whatever case GitHub gives its
// repository: a board ref keeps that case and namedRefs lower-cases.
//
// control: none: validateProgressTask is new in this pull request, so the base cannot build it
func TestValidateProgressTaskIgnoresCase(t *testing.T) {
	a := &app{progressItem: "Example/App#1"}
	if err := a.validateProgressTask("Continue https://github.com/Example/App/issues/1 now."); err != nil {
		t.Fatalf("the item's own brief was refused: %v", err)
	}
	if err := a.validateProgressTask("Continue https://github.com/Example/App/issues/2 now."); err == nil {
		t.Fatal("another item's brief was admitted")
	}
}

func TestValidateProgressTaskAdmitsLinkedPullBrief(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Deliveries = []state.Delivery{{Ref: "Example/App#1", Pulls: []string{"Example/App#7"}, Step: state.DeliveryFixing}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	a := &app{progressItem: "Example/App#1", store: store}
	if err := a.validateProgressTask("Fix https://github.com/Example/App/pull/7 now."); err != nil {
		t.Fatalf("linked pull brief was refused: %v", err)
	}
}

func TestBoardProgressIgnoresMergedDeliverySlicesAndEndedReviewParks(t *testing.T) {
	r := newLoopRig(t)
	at := r.a.timeNow()
	st := &state.State{
		Deliveries: []state.Delivery{
			{Ref: loopClosingIssue, Pulls: []string{loopPR}, Step: state.DeliveryDone},
		},
		ReviewParks: []state.ReviewPark{
			{On: "o/r#8", Serves: loopClosingIssue, At: at.Add(-time.Minute)},
		},
	}
	in := superloop.Input{
		Pulls: map[string]superloop.Pull{},
		Taken: []superloop.Taken{{Kind: superloop.KindEnd, Ref: "o/r#8", At: at}},
	}
	if got := progressPulls(st, in, loopClosingIssue); len(got) != 0 {
		t.Fatalf("ended delivery and review park still tracked: %v", got)
	}

	st.Deliveries = []state.Delivery{
		{Ref: loopClosingIssue, Pulls: []string{loopPR}, Step: state.DeliveryReleasing},
	}
	in.Pulls[loopPR] = superloop.Pull{Ref: loopPR, State: superloop.Merged, Head: loopHead, Green: true}
	sig, known := r.a.boardProgressSignal(st, nil, in, nil, loopClosingIssue)
	if !known || sig != "null" {
		t.Fatalf("merged slice PR kept in progress signal: known=%v sig=%q", known, sig)
	}
}

func TestRequireReviewStartsSecondRoundOnMovedHead(t *testing.T) {
	r := newLoopRig(t)
	r.a.as = loop.LoopParty.Name
	r.a.cfg.Merge.RequireReview = true
	now := r.a.timeNow()
	r.park(t, loopPR, now)
	r.pass(t)
	if start := r.called(agentsName, agentStartName); start == nil || start[2] != loopReviewer {
		t.Fatalf("round 1 reviewer did not start: %v", r.calls)
	}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.DeskReviews = []state.DeskReview{{
			ID:         "round-1",
			Ref:        loopPR,
			Head:       loopHead,
			Round:      1,
			Worker:     state.Party{Name: loopWorker},
			Reviewer:   state.Party{Name: loopReviewer},
			AssignedAt: now.Add(-time.Minute),
			Result:     &state.DeskReviewResult{ID: "R1", Verdict: superloop.Approved, Head: loopHead, By: state.Party{Name: loopReviewer}},
		}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.verdict("R1", loopHead, superloop.Approved, deskReviewApproved)
	r.pull.Head = loopFixed
	r.park(t, loopPR, now.Add(time.Second))
	r.pass(t)
	start := r.called(agentsName, agentStartName)
	if start == nil || start[2] != superloop.ReviewerName(loopPR, 2) || !slices.Contains(start, "--review") {
		t.Fatalf("moved head past approval did not start round 2 reviewer: %v\n%s", r.calls, r.out)
	}
	if merge := r.called(gate.GateCmdName); merge != nil {
		t.Fatalf("unreviewed moved head attempted merge: %v", merge)
	}
}

func TestProgressItemWaivesPositiveCeilingOnly(t *testing.T) {
	orig := plat
	plat = platform.Stub()
	plat.Machine = roomyMachine{Machine: plat.Machine}
	t.Cleanup(func() { plat = orig })

	dir := t.TempDir()
	cfgPath := emptyTestConfig(t, filepath.Join(dir, "config.yaml"))
	t.Setenv("HOME", dir)
	t.Setenv("BEEKEEPER_CONFIG", cfgPath)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("MEMCAP_STATE", filepath.Join(dir, "memcap"))
	t.Setenv("PATH", dir)

	const itemRef = "example/app#1"
	if _, err := config.Set(cfgPath, []config.Setting{{Key: "capacity.ceiling", Value: "1"}}); err != nil {
		t.Fatal(err)
	}
	openCapApp := func(as string) (*app, *bytes.Buffer) {
		c, err := config.Load(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		store, err := state.OpenVersion(c.StateDir, "dev")
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		return &app{cfgPath: cfgPath, cfg: c, store: store, now: keepNow, out: &out, as: as}, &out
	}

	aSetup, _ := openCapApp(agentOne)
	if err := aSetup.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{tasked(busyOne)}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	saved := loop.BoardProgress{
		BoardAt: keepNow,
		Items:   []board.Candidate{{Item: board.Item{Ref: itemRef, Status: "Up Next"}}},
		Entries: map[string]loop.ItemProgress{
			itemRef: {Signal: "null", Changed: keepNow.Add(-31 * time.Minute), Seen: keepNow},
		},
	}
	if err := aSetup.store.WriteFile(loop.BoardProgressFile, saved); err != nil {
		t.Fatal(err)
	}

	aPlain, outPlain := openCapApp(loop.LoopParty.Name)
	if err := aPlain.capacityCmd().Execute(); err != nil || !strings.Contains(outPlain.String(), "no start: 1 busy at the ceiling of 1\n") {
		t.Fatalf("plain capacity at ceiling 1 = %q, %v", outPlain.String(), err)
	}

	aProgress, outProgress := openCapApp(loop.LoopParty.Name)
	progressCmd := aProgress.capacityCmd()
	progressCmd.SetArgs([]string{"--" + loop.BoardProgressFlag, itemRef})
	if err := progressCmd.Execute(); err != nil || !strings.Contains(outProgress.String(), "room for 1 start\n") {
		t.Fatalf("stalled progress capacity at ceiling 1 = %q, %v", outProgress.String(), err)
	}

	if _, err := config.Set(cfgPath, []config.Setting{{Key: "capacity.ceiling", Value: "0"}}); err != nil {
		t.Fatal(err)
	}
	aZero, outZero := openCapApp(loop.LoopParty.Name)
	zeroCmd := aZero.capacityCmd()
	zeroCmd.SetArgs([]string{"--" + loop.BoardProgressFlag, itemRef})
	if err := zeroCmd.Execute(); err != nil || !strings.Contains(outZero.String(), "\n"+noStartCeilingZero+"\n") {
		t.Fatalf("stalled progress capacity at ceiling 0 = %q, %v", outZero.String(), err)
	}
}
