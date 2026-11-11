package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const deliveryFixtureRepo = "o/r"
const deliveryCompareBehind = "behind"
const deliveryFailedTag = "v8.1.1"
const deliveryAvailableTag = "v2.0.0-rc.1"

func TestDeliveryReleaseVerdictsSurviveLane(t *testing.T) {
	for _, tc := range []struct {
		name                string
		out                 merge.Outcome
		hand, step, verdict string
	}{
		{"available", merge.Outcome{Merged: true, Release: "v1.2.3-rc.1"}, "", state.DeliveryDone, verdictAvailable},
		{"no-release verdict", merge.Outcome{Merged: true, NoRelease: true}, "", state.DeliveryDone, state.DeliveryNoRelease},
		{"hand cut", merge.Outcome{Merged: true}, "maintenance", state.DeliveryDone, state.DeliveryNoRelease},
		{"release failure", merge.Outcome{Merged: true, ReleaseFailed: true, Release: "v9.0.1"}, "", state.DeliveryReleasing, state.DeliveryReleaseFailed},
		{"unconfirmed", merge.Outcome{Merged: true, Unconfirmed: true}, "", state.DeliveryReleasing, verdictUnconfirmed},
		{"missing verdict", merge.Outcome{Merged: true}, "", state.DeliveryReleasing, verdictUnconfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := supervisorApp(t)
			now := a.timeNow()
			ref := blockerRef
			seed(t, a, func(st *state.State) {
				st.Deliveries = []state.Delivery{{Ref: epicTen, Pulls: []string{ref}, Step: state.DeliveryMerging, Since: now.Add(-time.Hour), Started: now.Add(-2 * time.Hour)}}
				st.Merges = []state.Merge{{Repo: deliveryFixtureRepo, PR: 20, HandCut: tc.hand}}
			})
			err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				return recordRun(st, 0, config.Lane{}, loopParty, runOutcome{out: tc.out}, now, ""), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			// Remove the lane and reopen the store: the verdict belongs to the delivery.
			seed(t, a, func(st *state.State) { st.Merges = nil })
			store, err := state.Open(a.store.Dir())
			if err != nil {
				t.Fatal(err)
			}
			st, err := store.Read()
			if err != nil {
				t.Fatal(err)
			}
			d := st.Deliveries[0]
			if d.Step != tc.step || d.Verdict != tc.verdict || d.Release != tc.out.Release {
				t.Fatalf("%+v", d)
			}
			events, err := store.Events(0, func(e state.Event) bool { return e.Verb == state.DeliveryStepVerb })
			if err != nil || len(events) != 1 {
				t.Fatalf("events: %+v %v", events, err)
			}
		})
	}
}

func TestDeliveryExternalMergeRestartAndReadBound(t *testing.T) {
	a, _ := supervisorApp(t)
	now := a.timeNow()
	seed(t, a, func(st *state.State) {
		for i := 1; i <= 4; i++ {
			st.Deliveries = append(st.Deliveries, state.Delivery{Ref: fmt.Sprintf("o/r#%d", i), Pulls: []string{fmt.Sprintf("o/r#%d", i+10)}, Step: state.DeliveryReleasing, Started: now.Add(-time.Hour), Since: now.Add(-time.Minute)})
		}
	})
	in := superloop.Input{Now: now, Pulls: map[string]superloop.Pull{}}
	for i := 1; i <= 4; i++ {
		ref := fmt.Sprintf("o/r#%d", i+10)
		in.Pulls[ref] = superloop.Pull{Ref: ref, State: superloop.Merged, MergeCommit: loopHead}
	}
	old := readDeliveryRelease
	t.Cleanup(func() { readDeliveryRelease = old })
	calls := 0
	readDeliveryRelease = func(context.Context, superloop.Pull) (string, string, error) {
		calls++
		return "", "", errors.New("offline")
	}
	l := &supervisorLoop{a: a, said: map[string]bool{}}
	if err := l.observeDeliveries(t.Context(), in, nil); err != nil {
		t.Fatal(err)
	}
	if calls != deliveryReleaseReads {
		t.Fatalf("calls %d", calls)
	}
	// Restart processes; failed attempts still back off, while other PRs get a turn.
	l = &supervisorLoop{a: a, said: map[string]bool{}}
	if err := l.observeDeliveries(t.Context(), in, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatalf("calls after restart %d", calls)
	}
	if err := l.observeDeliveries(t.Context(), in, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatalf("unbounded %d", calls)
	}
	st, _ := a.store.Read()
	for _, d := range st.Deliveries {
		if d.Step != state.DeliveryReleasing {
			t.Fatalf("failed read ended %+v", d)
		}
	}
	in.Now = now.Add(deliveryReleaseEvery)
	readDeliveryRelease = func(context.Context, superloop.Pull) (string, string, error) {
		return verdictAvailable, deliveryAvailableTag, nil
	}
	if err := l.observeDeliveries(t.Context(), in, nil); err != nil {
		t.Fatal(err)
	}
	st, _ = a.store.Read()
	done := 0
	for _, d := range st.Deliveries {
		if d.Step == state.DeliveryDone {
			done++
		}
	}
	if done != deliveryReleaseReads {
		t.Fatalf("done %d", done)
	}
}

func TestDeliveryStartsAtomicAndRestartIdempotent(t *testing.T) {
	r := newLoopRig(t)
	now := r.a.timeNow()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 1; i <= 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				prev := append([]state.Delivery(nil), st.Deliveries...)
				s := state.Start{Party: state.Party{Name: fmt.Sprintf("delivery worker %d", i), Session: fmt.Sprint(i)}, At: now}
				_, err := recordStart(st, s, "research", fmt.Sprintf("o/r#%d", i), func(state.Party) bool { return false })
				return state.DeliveryEvents(prev, st.Deliveries, loopParty), err
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	st, _ := r.a.store.Read()
	if len(st.Deliveries) != 2 {
		t.Fatal(st.Deliveries)
	}
	in := superloop.Input{Now: now, Workers: []superloop.Worker{{Name: "delivery worker 1", Serves: refOne}, {Name: "delivery worker 2", Serves: idleRef}}}
	for range 2 {
		l := &supervisorLoop{a: r.a, said: map[string]bool{}}
		if err := l.observeDeliveries(t.Context(), in, nil); err != nil {
			t.Fatal(err)
		}
	}
	events, err := r.a.store.Events(0, func(e state.Event) bool { return e.Verb == state.DeliveryStepVerb })
	if err != nil || len(events) != 2 {
		t.Fatalf("duplicate events %+v %v", events, err)
	}
}

func TestDeliveryCommandGoldenAndNoNetwork(t *testing.T) {
	a, out := supervisorApp(t)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a.now = now
	// A shell tool is never needed to read deliveries or their statistics.
	t.Setenv("PATH", t.TempDir())
	oldGH := github.RunGH
	t.Cleanup(func() { github.RunGH = oldGH })
	github.RunGH = func(context.Context, ...string) ([]byte, error) {
		t.Fatal("delivery invoked GitHub")
		return nil, errors.New("unexpected network")
	}
	seed(t, a, func(st *state.State) {
		st.Deliveries = []state.Delivery{{Ref: epicTen, Pulls: []string{blockerRef}, Step: state.DeliveryCI, Since: now.Add(-time.Minute), Started: now.Add(-time.Hour), Agent: "worker", Waits: "checks"}}
		st.DeliveriesAt = now
	})
	c := a.deliveryCmd()
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	want := "https://github.com/o/r/issues/10  https://github.com/o/r/pull/20  ci  step 1m0s  age 1h0m0s  worker  checks\n1 flowing of limit 0 (off); 0 waiting on the person; 0 blocked\n"
	if out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
	out.Reset()
	a.json = true
	if err := a.deliveryCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	var got deliveryView
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	expected := readDelivery(st, 0, now)
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("json: %+v != %+v", got, expected)
	}
	out.Reset()
	c = a.deliveryCmd()
	c.SetArgs([]string{"--stats"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"released": 0`) {
		t.Fatal(out.String())
	}
	seed(t, a, func(st *state.State) { st.Deliveries = nil })
	out.Reset()
	a.json = false
	if err := a.deliveryCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "0 flowing of limit 0 (off); 0 waiting on the person; 0 blocked\n" {
		t.Fatal(out.String())
	}
}

func TestDeliveryStatsWindowAndDuplicates(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	since := now.Add(-24 * time.Hour)
	var events []state.Event
	add := func(ref, step string, at time.Time) {
		d := state.Delivery{Ref: ref, Step: step, Since: at, Started: since}
		raw, _ := json.Marshal(d)
		events = append(events, state.Event{Verb: state.DeliveryStepVerb, At: at, Detail: string(raw)})
	}
	add(refOne, state.DeliveryImplementing, since.Add(-time.Hour))
	add(refOne, state.DeliveryCI, since.Add(time.Hour))
	add(refOne, state.DeliveryDone, since.Add(3*time.Hour))
	add(refOne, state.DeliveryDone, since.Add(4*time.Hour))
	add(idleRef, state.DeliveryImplementing, since)
	add(idleRef, state.DeliveryDone, since.Add(3*time.Hour))
	add("o/r#3", state.DeliveryCI, now.Add(time.Hour))
	got := deliveryStatistics(events, since, now)
	want := deliveryStats{Outcomes: map[string]int{state.OutcomeMerged: 2}, Since: since, Until: now, Released: 2, ReleasedPerDay: 2, MeanCycleTime: 3 * time.Hour, CycleSamples: 2, Steps: []deliveryStepStats{{Step: state.DeliveryCI, Median: 2 * time.Hour, Worst: 2 * time.Hour, Samples: 1}, {Step: state.DeliveryImplementing, Median: 2 * time.Hour, Worst: 3 * time.Hour, Samples: 2}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v != %+v", got, want)
	}
}

func TestDeliverySharedStatusAndReportReading(t *testing.T) {
	a, out := supervisorApp(t)
	seed(t, a, func(st *state.State) {
		st.Deliveries = []state.Delivery{{Ref: refOne, Step: state.DeliveryUnknown, Since: a.timeNow()}, {Ref: idleRef, Step: state.DeliveryDone}}
	})
	a.cfg.Delivery.WIPLimit = 5
	if err := a.supervisorStatus(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "1 flowing of limit 5") {
		t.Fatal(out.String())
	}
	st, _ := a.store.Read()
	v := readDelivery(st, 5, a.timeNow())
	// report's Queue renders this same view, not an independently computed count.
	f := &reportFacts{Delivery: &v, Zone: time.UTC}
	if got := renderReport(f); !strings.Contains(got, "1 flowing of limit 5") {
		t.Fatal(got)
	}
}

func TestDeliveryLimitLeavesCapacityJSONUnchanged(t *testing.T) {
	original := plat
	plat = platform.Stub()
	plat.Machine = roomyMachine{Machine: plat.Machine}
	t.Cleanup(func() { plat = original })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv("MEMCAP_STATE", t.TempDir())
	a, out := supervisorApp(t)
	a.json = true
	a.cfg.Capacity.Ceiling = 10
	if err := a.capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	before := out.String()
	out.Reset()
	a.cfg.Delivery.WIPLimit = 1
	seed(t, a, func(st *state.State) {
		for i := 1; i <= 3; i++ {
			st.Deliveries = append(st.Deliveries, state.Delivery{Ref: fmt.Sprintf("o/r#%d", i), Step: state.DeliveryUnknown})
		}
	})
	if err := a.capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != before {
		t.Fatalf("capacity changed with a delivery limit: %s != %s", out.String(), before)
	}
}

func TestDeliveryRESTTagMustContainActualMerge(t *testing.T) {
	old := github.RunGH
	t.Cleanup(func() { github.RunGH = old })
	var calls []string
	status := deliveryCompareBehind
	github.RunGH = func(_ context.Context, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		switch {
		case strings.Contains(call, github.AutoReleaseWorkflow):
			return []byte("on:\n  push:\n    branches: [main]\n"), nil
		case strings.Contains(call, "/tags?"):
			return []byte("v3.1.0-rc.2\n"), nil
		case strings.Contains(call, "/compare/"+loopFixed+"...v3.1.0-rc.2"):
			return []byte(status), nil
		default:
			t.Fatalf("unexpected REST request %s", call)
			return nil, nil
		}
	}
	p := superloop.Pull{Ref: "example/delivery#2", Base: mainBranch, MergeCommit: loopFixed, State: superloop.Merged}
	verdict, _, err := readDeliveryRelease(t.Context(), p)
	if err != nil || verdict != "" {
		t.Fatalf("tag does not contain merge: %s %v", verdict, err)
	}
	status = "ahead"
	verdict, tag, err := readDeliveryRelease(t.Context(), p)
	if err != nil || verdict != verdictAvailable || tag != "v3.1.0-rc.2" {
		t.Fatalf("tag contains merge: %s %s %v", verdict, tag, err)
	}
	if len(calls) != 6 {
		t.Fatalf("calls: %v", calls)
	}
	for _, call := range calls {
		if !strings.HasPrefix(call, "api ") || strings.Contains(call, "graphql") {
			t.Fatal(call)
		}
	}
}

func TestDeliveryCompleteSeedAndPartialReadTimestamp(t *testing.T) {
	a, _ := supervisorApp(t)
	now := a.timeNow()
	old := now.Add(-time.Hour)
	seed(t, a, func(st *state.State) { st.DeliveriesAt = old })
	l := &supervisorLoop{a: a, said: map[string]bool{}, openPulls: []string{refOne, idleRef}}
	in := superloop.Input{Now: now, Pulls: map[string]superloop.Pull{refOne: {Ref: refOne, State: superloop.Open}}}
	if err := l.observeDeliveries(t.Context(), in, map[string]bool{idleRef: true}); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	if len(st.Deliveries) != 2 || !st.DeliveriesAt.Equal(old) {
		t.Fatalf("partial seed: %+v at %s", st.Deliveries, st.DeliveriesAt)
	}
	in.Pulls[idleRef] = superloop.Pull{Ref: idleRef, State: superloop.Open}
	if err := l.observeDeliveries(t.Context(), in, nil); err != nil {
		t.Fatal(err)
	}
	st, _ = a.store.Read()
	if !st.DeliveriesAt.Equal(now) {
		t.Fatal(st.DeliveriesAt)
	}
	events, _ := a.store.Events(0, func(e state.Event) bool { return e.Verb == state.DeliveryStepVerb })
	l = &supervisorLoop{a: a, said: map[string]bool{}, openPulls: []string{refOne, idleRef}}
	if err := l.observeDeliveries(t.Context(), in, nil); err != nil {
		t.Fatal(err)
	}
	after, _ := a.store.Events(0, func(e state.Event) bool { return e.Verb == state.DeliveryStepVerb })
	if len(events) != len(after) {
		t.Fatal("restart duplicated steps")
	}
}

func TestDeliveryClaimRecordsSameAtomicUpdate(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		me := state.Party{Session: fmt.Sprintf("delivery-claim-%d", i), Name: fmt.Sprintf("claim worker %d", i)}
		result, err := claimNext(store, boardCandidates(2), pickScope{me: me, alive: func(state.Party) bool { return true }, listed: time.Now()}, "implement", func(string) bool { return false })
		if err != nil || !result.Claimed {
			t.Fatalf("%+v %v", result, err)
		}
	}
	st, _ := store.Read()
	if len(st.Records) != 2 || len(st.Deliveries) != 2 {
		t.Fatalf("records %d deliveries %d", len(st.Records), len(st.Deliveries))
	}
	events, _ := store.Events(0, func(e state.Event) bool { return e.Verb == state.DeliveryStepVerb })
	if len(events) != 2 {
		t.Fatal(events)
	}
}

func TestDeliveryOverlappedReleasePersistsWhenLaneForgets(t *testing.T) {
	a, _ := supervisorApp(t)
	now := a.timeNow()
	seed(t, a, func(st *state.State) {
		st.Merges = []state.Merge{{Repo: deliveryFixtureRepo, PR: 20, Phase: state.Settling, Releasing: true}}
		st.Deliveries = []state.Delivery{{Ref: epicTen, Pulls: []string{blockerRef}, Step: state.DeliveryReleasing, Started: now, Since: now}}
	})
	old := releaseWait
	t.Cleanup(func() { releaseWait = old })
	releaseWait = func(context.Context, string, ...string) []byte {
		return []byte(`{"verdict":"available","tag":"v2.3.4-rc.1"}`)
	}
	g := &gateRun{ctx: t.Context(), app: a, repo: deliveryFixtureRepo, pr: 20, lane: config.Lane{Name: "delivery-lane"}}
	out := merge.Outcome{Merged: true}
	_, code := g.confirmRelease(&out, 0)
	if code != 0 {
		t.Fatal(code)
	}
	st, _ := a.store.Read()
	if len(st.Merges) != 0 || st.Deliveries[0].Step != state.DeliveryDone || st.Deliveries[0].Release != "v2.3.4-rc.1" {
		t.Fatalf("lane %v delivery %+v", st.Merges, st.Deliveries)
	}
}

func TestDeliveryFailedTagDoesNotBecomeReleaseProof(t *testing.T) {
	a, _ := supervisorApp(t)
	now := a.timeNow()
	seed(t, a, func(st *state.State) {
		st.Deliveries = []state.Delivery{{Ref: refOne, Pulls: []string{idleRef}, Step: state.DeliveryReleasing, Verdict: state.DeliveryReleaseFailed, Release: deliveryFailedTag, Started: now, Since: now}}
	})
	old := readDeliveryRelease
	t.Cleanup(func() { readDeliveryRelease = old })
	tag := deliveryFailedTag
	readDeliveryRelease = func(context.Context, superloop.Pull) (string, string, error) { return verdictAvailable, tag, nil }
	l := &supervisorLoop{a: a, said: map[string]bool{}}
	in := superloop.Input{Now: now, Pulls: map[string]superloop.Pull{idleRef: {Ref: idleRef, State: superloop.Merged}}}
	if err := l.observeDeliveries(t.Context(), in, nil); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	if st.Deliveries[0].Step != state.DeliveryReleasing || st.Deliveries[0].Verdict != state.DeliveryReleaseFailed {
		t.Fatal(st.Deliveries)
	}
	in.Now = now.Add(deliveryReleaseEvery)
	tag = "v2.0.2"
	if err := l.observeDeliveries(t.Context(), in, nil); err != nil {
		t.Fatal(err)
	}
	st, _ = a.store.Read()
	if st.Deliveries[0].Step != state.DeliveryDone {
		t.Fatal(st.Deliveries)
	}
}

func TestDeliveryUnreadableRepositoryIsNotNoRelease(t *testing.T) {
	old := github.RunGH
	t.Cleanup(func() { github.RunGH = old })
	readable := false
	github.RunGH = func(_ context.Context, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		if strings.Contains(call, "/branches/main") && readable {
			return []byte(mainBranch), nil
		}
		return nil, errors.New("HTTP 404")
	}
	p := superloop.Pull{Ref: "example/hidden#3", Base: mainBranch, MergeCommit: loopHead, State: superloop.Merged}
	verdict, _, err := readDeliveryRelease(t.Context(), p)
	if err == nil || verdict != "" {
		t.Fatalf("unreadable repo ended: %q %v", verdict, err)
	}
	readable = true
	verdict, _, err = readDeliveryRelease(t.Context(), p)
	if err != nil || verdict != state.DeliveryNoRelease {
		t.Fatalf("readable branch: %q %v", verdict, err)
	}
}

func TestDeliveryLoopWritesBeforeActionsAndRestart(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pull.Closes = []string{epicTen}
	answer := r.answer
	r.answer = func(args []string) (string, int) {
		if len(args) > 1 && args[0] == agentsName && args[1] == "start" {
			st, err := r.a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Deliveries) != 1 || st.Deliveries[0].Step != state.DeliveryReviewQueued {
				t.Fatalf("action ran before observation: %+v", st.Deliveries)
			}
		}
		return answer(args)
	}
	r.pass(t)
	st, _ := r.a.store.Read()
	if len(st.Deliveries) != 1 || st.Deliveries[0].Ref != epicTen {
		t.Fatal(st.Deliveries)
	}
	r.answer = answer
	r.pass(t)
	events, _ := r.a.store.Events(0, func(e state.Event) bool { return e.Verb == state.DeliveryStepVerb })
	r.pass(t)
	after, _ := r.a.store.Events(0, func(e state.Event) bool { return e.Verb == state.DeliveryStepVerb })
	if len(events) != len(after) {
		t.Fatalf("restart repeated transitions: %d -> %d", len(events), len(after))
	}
}

func TestDeliveryGeneralTagCheckKeepsSelfUpdateWakesScoped(t *testing.T) {
	old := github.RunGH
	t.Cleanup(func() { github.RunGH = old })
	calls := 0
	github.RunGH = func(context.Context, ...string) ([]byte, error) { calls++; return []byte("ahead"), nil }
	ref := "example/delivery#2"
	contains, err := releaseTagContains(t.Context(), ref, loopFixed, "v3.0.0")
	if err != nil || !contains || calls != 1 {
		t.Fatalf("general check: %v %v calls %d", contains, err, calls)
	}
	contains, err = loopReleaseContains(t.Context(), ref, loopFixed, "v3.0.0")
	if err != nil || contains || calls != 1 {
		t.Fatalf("self-update crossed repositories: %v %v calls %d", contains, err, calls)
	}
}

func TestDeliveryMeanCycleTimeUsesFullCompletedAges(t *testing.T) {
	now := time.Date(2026, 1, 8, 0, 0, 0, 0, time.UTC)
	since := now.Add(-24 * time.Hour)
	event := func(ref string, started, released time.Time) state.Event {
		raw, err := json.Marshal(state.Delivery{Ref: ref, Step: state.DeliveryDone, Started: started, Since: released})
		if err != nil {
			t.Fatal(err)
		}
		return state.Event{Verb: state.DeliveryStepVerb, At: released, Detail: string(raw)}
	}
	first := event("o/r#1", since.Add(-48*time.Hour), since.Add(time.Hour))
	got := deliveryStatistics([]state.Event{
		first, first,
		event("o/r#2", now.Add(-time.Hour), now),
		event("o/r#3", time.Time{}, now),
		event("o/r#4", now.Add(time.Hour), now),
		event("o/r#5", since.Add(-3*time.Hour), since.Add(-time.Hour)),
		event("o/r#6", now, now.Add(time.Hour)),
	}, since, now)
	if got.Released != 4 || got.CycleSamples != 2 || got.MeanCycleTime != 25*time.Hour {
		t.Fatalf("full completed cycle statistics: %+v", got)
	}
}

// A pass can take longer than the per-release retry interval. Ledger-order
// probing then retries the same failures forever, leaving published releases
// at the tail counted in WIP even though they could finish immediately.
func TestDeliveryReleaseBacklogDoesNotStarveBehindFailures(t *testing.T) {
	a, _ := supervisorApp(t)
	now := a.timeNow()
	in := superloop.Input{Now: now, Pulls: map[string]superloop.Pull{}}
	seed(t, a, func(st *state.State) {
		for i := 1; i <= deliveryReleaseReads+1; i++ {
			ref := fmt.Sprintf("o/r#%d", i+10)
			st.Deliveries = append(st.Deliveries, state.Delivery{Ref: ref, Pulls: []string{ref}, Step: state.DeliveryReleasing, Started: now, Since: now})
			in.Pulls[ref] = superloop.Pull{Ref: ref, State: superloop.Merged, MergeCommit: loopHead}
		}
	})
	old := readDeliveryRelease
	t.Cleanup(func() { readDeliveryRelease = old })
	var probed []string
	tail := fmt.Sprintf("o/r#%d", deliveryReleaseReads+11)
	readDeliveryRelease = func(_ context.Context, p superloop.Pull) (string, string, error) {
		probed = append(probed, p.Ref)
		if p.Ref == tail {
			return verdictAvailable, deliveryAvailableTag, nil
		}
		return "", "", errors.New("release observation unavailable")
	}
	for pass := range 2 {
		// Restart too: fairness follows persisted attempt times, not process state.
		in.Now = now.Add(time.Duration(pass) * (deliveryReleaseEvery + time.Minute))
		l := &supervisorLoop{a: a, said: map[string]bool{}}
		before := len(probed)
		if err := l.observeDeliveries(t.Context(), in, nil); err != nil {
			t.Fatal(err)
		}
		if len(probed)-before > deliveryReleaseReads {
			t.Fatal("exceeded release probe allowance")
		}
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range st.Deliveries {
		if d.Ref == tail {
			if d.Step != state.DeliveryDone {
				t.Fatalf("published tail starved behind repeated failures: %+v; probed %v", d, probed)
			}
			if deliveryInFlight(st, d, in.Now, a.deliveryPolicy()) {
				t.Fatal("completed release still occupies WIP")
			}
			return
		}
	}
	t.Fatal("tail delivery disappeared")
}
