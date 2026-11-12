package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/state"
)

// refusedStarts reports whether err is the "starts" hold's refusal.
func refusedStarts(err error) bool {
	var e *exitError
	return errors.As(err, &e) && e.code == ExitRefused && strings.Contains(e.msg, "starts are held by")
}

// A "starts" hold refuses every new worker and reviewer, a forced one too,
// and a done agent's wake, while it ends no session, holds no merge or
// GitHub call and lets an agent on its task be woken; status names it with
// who set it, and its lift lets starts through again.
func TestStartsHoldRefusesNewWorkAndKeepsRunningWork(t *testing.T) {
	a, l, ended := stopDesk(t)
	ctx := context.Background()
	hold := func(args ...string) error {
		c := a.holdCmd()
		c.SetArgs(args)
		c.SetOut(a.out)
		c.SetErr(a.out)
		c.SilenceErrors, c.SilenceUsage = true, true
		return c.Execute()
	}
	if err := hold("set", holdStarts, "-r", "sorting priorities", "--except", refOne); Code(err) != ExitUsage {
		t.Errorf("a starts hold with an exception: %v", err)
	}
	if err := hold("set", holdStarts, "-r", "the person sorts priorities"); err != nil {
		t.Fatal(err)
	}
	if len(*ended) != 0 || len(l.stopped) != 0 {
		t.Errorf("the starts hold ended sessions %v, stopped units %v", *ended, l.stopped)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Stop != nil {
		t.Errorf("the starts hold recorded a stop: %+v", st.Stop)
	}
	for _, target := range stopHolds {
		if h, ok := activeHold(st, a, target); ok {
			t.Errorf("the starts hold holds %s: %+v", target, h)
		}
	}

	for _, kind := range []StartFor{ForWorker, ForReview, ForReviewOfSelfHosted} {
		v, err := a.startCapacityWithOptions(ctx, false, nil, kind)
		if err != nil || v.Room != 0 || !strings.Contains(v.Verdict(), "no start: starts are held by "+agentOne) || !strings.Contains(v.Verdict(), "the person sorts priorities") {
			t.Errorf("capacity of a start (%d) while starts are held: %v %+v", kind, err, v)
		}
		if _, err := gateStart(v, true); !refusedStarts(err) || strings.Contains(err.Error(), "--force") {
			t.Errorf("a forced start's gate (%d) while starts are held: %v", kind, err)
		}
		if p := NewPick(v); p.Pick != PickNone || !strings.Contains(p.Reason, "starts are held") {
			t.Errorf("capacity pick (%d) while starts are held: %+v", kind, p)
		}
	}

	// The worker on its task is woken as before; done, its wake would hand
	// it new work.
	if err := a.wakeAgent(ctx, state.Party{Name: agentOne}, agentSeven, "go on", ""); refusedStarts(err) {
		t.Errorf("a working agent's wake while starts are held: %v", err)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		for i := range st.Agents {
			if st.Agents[i].Name == agentSeven {
				st.Agents[i].Done, st.Agents[i].Task = true, ""
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.wakeAgent(ctx, state.Party{Name: agentOne}, agentSeven, "take this", ""); !refusedStarts(err) || !strings.Contains(err.Error(), agentSeven+" refused: it is done") {
		t.Errorf("a done agent's wake while starts are held: %v", err)
	}

	s, err := a.status()
	if err != nil || !strings.HasPrefix(s.line(), "STARTS HELD: starts are held by "+agentOne) || !strings.Contains(s.line(), "the person sorts priorities") || s.Stopped != "" {
		t.Errorf("status while starts are held: %v %q", err, s.line())
	}

	if err := hold("lift", holdStarts); err != nil {
		t.Fatal(err)
	}
	v, err := a.startCapacity(ctx, false)
	if err != nil || v.Stopped != "" || strings.Contains(v.Verdict(), "starts are held") {
		t.Errorf("capacity after the lift: %v %+v", err, v)
	}
	if s, err := a.status(); err != nil || s.StartsHeld != "" || strings.Contains(s.line(), "STARTS HELD") {
		t.Errorf("status after the lift: %v %q", err, s.line())
	}
}

// A standing stop is what a start's refusal names while both stand.
func TestTheStopIsNamedOverTheStartsHold(t *testing.T) {
	a, _, _ := stopDesk(t)
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Holds = append(st.Holds, state.Hold{Target: holdStarts, Reason: "sorting", By: state.Party{Name: agentOne}, At: a.now})
		st.Stop = &state.Stop{Reason: "the person said stop", By: state.Party{Name: agentOne}, At: a.now}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	v, err := a.startCapacity(context.Background(), false)
	if err != nil || !strings.Contains(v.Stopped, "beekeeper is stopped") {
		t.Errorf("capacity under a stop and a starts hold: %v %+v", err, v)
	}
}
