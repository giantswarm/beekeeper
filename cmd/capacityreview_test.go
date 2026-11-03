package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestReviewHeadroomLegacyGuards(t *testing.T) {
	const (
		reviewNoGuard = iota
		reviewMemory
		reviewLoad
		reviewSwap
		reviewLabs
		reviewSlot
	)
	k := config.Capacity{Ceiling: 2, ReviewHeadroom: 2}
	for _, tc := range []struct {
		name   string
		busy   int
		kind   startFor
		builds bool
		guard  int
		room   int
	}{
		{"worker ceiling", 2, forWorker, false, reviewNoGuard, 0},
		{"review ceiling", 2, forReview, false, reviewNoGuard, 2},
		{"review last slot", 3, forReview, false, reviewNoGuard, 1},
		{"review exhausted", 4, forReview, false, reviewNoGuard, 0},
		{"memory availability", 2, forReview, false, reviewMemory, 0},
		{"machine load", 2, forReview, false, reviewLoad, 0},
		{"swap pressure", 2, forReview, false, reviewSwap, 0},
		{"lab count", 2, forReview, false, reviewLabs, 0},
		{"build slot", 2, forReview, true, reviewSlot, 0},
		{"read only review", 2, forReview, false, reviewSlot, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := roomy()
			switch tc.guard {
			case reviewMemory:
				h.AvailableMiB = 0
			case reviewSwap:
				h.Swap.PerHourMiB, h.PSI = 900, 1
			case reviewLoad:
				h.Load1 = h.LoadMax + 1
			case reviewLabs:
				h.Labs = h.MaxLabs + 1
			case reviewSlot:
				h.Slots, h.SlotsFree = 1, 0
			}
			v := newStartCapacity(agentCount{Busy: make([]string, tc.busy)}, k, h, tc.builds, tc.kind)
			if v.Room != tc.room {
				t.Fatalf("room %d, want %d: %s", v.Room, tc.room, v.verdict())
			}
			if tc.busy == 4 && !strings.Contains(v.verdict(), "review headroom exhausted") {
				t.Fatal(v.verdict())
			}
		})
	}
	k.Ceiling = 0
	if v := newStartCapacity(agentCount{}, k, roomy(), false, forReview); v.Room != 0 {
		t.Fatal("opened a stopped pool")
	}
	k.Ceiling, k.ReviewHeadroom = 2, 0
	if v := newStartCapacity(agentCount{Busy: []string{busyOne, busyTwo}}, k, roomy(), false, forReview); v.Room != 0 {
		t.Fatal("zero reserve ignored")
	}
}

func TestReviewHeadroomProviderAdmission(t *testing.T) {
	r := newPickRig(t, subscriptionChoices)
	r.cfg.Capacity.Ceiling = 4
	if err := r.store.Update(func(st *state.State) ([]state.Event, error) {
		for i := range 4 {
			ag := tasked(fmt.Sprintf("worker-%d", i))
			ag.Harness = claudeComm
			if i >= 2 {
				ag.Harness = "codex"
			}
			st.Agents = append(st.Agents, ag)
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	a, _ := r.app(false)
	a.cfgPath = ""
	v, err := a.startCapacityWithOptions(context.Background(), false, nil, forWorker)
	if err != nil || v.Room != 0 {
		t.Fatalf("worker admission: %+v, %v", v, err)
	}
	v, err = a.startCapacityWithOptions(context.Background(), false, nil, forReview)
	if err != nil || v.Room != 2 {
		t.Fatalf("review admission: %+v, %v", v, err)
	}
	if v = v.forTarget(r.cfg.Capacity, claudeComm, pickOpus); v.Room != 2 {
		t.Fatal(v.verdict())
	}

	if err := r.store.Update(func(st *state.State) ([]state.Event, error) {
		for i := range 2 {
			ag := tasked(fmt.Sprintf("review-%d", i))
			ag.Harness = claudeComm
			st.Agents = append(st.Agents, ag)
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	a, _ = r.app(false)
	a.cfgPath = ""
	v, err = a.startCapacityWithOptions(context.Background(), false, nil, forReview)
	if err != nil || v.Room != 0 || !strings.Contains(v.verdict(), "review headroom exhausted") {
		t.Fatalf("exhausted reserve: %+v, %v", v, err)
	}
	if v = v.forTarget(r.cfg.Capacity, "codex", pickGPT); v.Room != 0 || !strings.Contains(v.verdict(), "review headroom exhausted") {
		t.Fatal(v.verdict())
	}
	if err := r.store.Update(func(st *state.State) ([]state.Event, error) { st.Agents = st.Agents[:4]; return nil, nil }); err != nil {
		t.Fatal(err)
	}

	// Provider windows and pace remain binding, including explicit starts.
	r.claude[0], r.codex[0] = 91, 91
	a, _ = r.app(false)
	a.cfgPath = ""
	v, err = a.startCapacityWithOptions(context.Background(), false, nil, forReview)
	if err != nil || v.Room != 0 {
		t.Fatalf("window admission: %+v, %v", v, err)
	}
	if v = v.forTarget(r.cfg.Capacity, claudeComm, pickOpus); v.Room != 0 {
		t.Fatal("explicit start bypassed window stop")
	}
	r.claude[0], r.codex[0] = 10, 10
	r.claude[1], r.codex[1] = 51, 51
	a, _ = r.app(false)
	a.cfgPath = ""
	v, err = a.startCapacityWithOptions(context.Background(), false, nil, forReview)
	if err != nil || v.Room != 0 {
		t.Fatalf("pace admission: %+v, %v", v, err)
	}
}

// A subscription harness on a desk with only a self-hosted provider uses
// the unassigned pool; its refusal must name the exhausted review reserve.
func TestReviewHeadroomUnassignedTarget(t *testing.T) {
	k := config.Capacity{Ceiling: 2, ReviewHeadroom: 2, Providers: map[string]config.Provider{
		loopLocal: {Kind: config.KindSelfHosted, Ceiling: 1},
	}}
	for _, busy := range []int{2, 4} {
		c := agentCount{}
		for i := range busy {
			c.Busy = append(c.Busy, fmt.Sprintf("worker-%d", i))
		}
		v := buildProvidersCapacity(c, nil, k, nil, roomy(), false, nil, forReview, keepNow).forTarget(k, claudeComm, pickOpus)
		if busy == 2 && v.Room != 2 {
			t.Fatalf("no reserve for unassigned target: %s", v.verdict())
		}
		if busy == 4 && (v.Room != 0 || !strings.Contains(v.verdict(), "review headroom exhausted")) {
			t.Fatal(v.verdict())
		}
	}
}

// A pass must start the parked PR's review even with every worker slot busy.
func TestLoopStartsReviewAtWorkerCeilingInOnePass(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Capacity.Ceiling = 2
	r.a.cfg.Capacity.ReviewHeadroom = 2
	r.park(t, loopPR, time.Now())
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, tasked(busyOne), tasked(busyTwo))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.answer = func(args []string) (string, int) {
		if len(args) > 1 && args[0] == agentsName && args[1] == agentStartName {
			st, err := r.a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			kind := forWorker
			if slices.Contains(args, "--review") {
				kind = forReview
			}
			v := newStartCapacity(countAgents(st, nil, r.a.now, r.a.cfg), r.a.cfg.Capacity, roomy(), false, kind)
			if _, err := gateStart(v, false); err != nil {
				return err.Error(), ExitRefused
			}
		}
		return "", 0
	}
	r.pass(t)
	if evs := r.logged(t, "supervisor.review"); len(evs) != 1 {
		t.Fatalf("no review in first pass: %v, %s", r.calls, r.out)
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	v := newStartCapacity(countAgents(st, nil, r.a.now, r.a.cfg), r.a.cfg.Capacity, roomy(), false, forWorker)
	if _, err := gateStart(v, false); err == nil {
		t.Fatal("ordinary worker passed the ceiling")
	}
}
