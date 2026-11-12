package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func TestRecordedMergeGoRequiresExactLoopAction(t *testing.T) {
	worker := state.Party{Name: workerRole, Session: "worker-session"}
	ref, head := "person/project#7", strings.Repeat("a", 40)
	goEvent := event(loop.LoopParty, "supervisor.go", "[go %s@%s] worker: merge go", ref, head)
	if !recordedMergeGo([]state.Event{goEvent}, worker, ref, head) {
		t.Fatal("exact go denied")
	}
	for name, events := range map[string][]state.Event{
		"missing go":         nil,
		"worker copied go":   {event(worker, goEvent.Verb, "%s", goEvent.Detail)},
		"claim only":         {event(loop.LoopParty, loop.LoopClaimVerb, "%s", goEvent.Detail)},
		"changed head":       {event(loop.LoopParty, goEvent.Verb, "[go %s@%s] worker: merge go", ref, strings.Repeat("b", 40))},
		"other worker":       {event(loop.LoopParty, goEvent.Verb, "[go %s@%s] other: merge go", ref, head)},
		"new review":         {goEvent, event(loop.LoopParty, "supervisor.review", "[review %s@%s round 2] worker: review", ref, head)},
		"findings forwarded": {goEvent, event(loop.LoopParty, "supervisor.forward", "[forward %s review-id] worker: changes", ref)},
	} {
		if recordedMergeGo(events, worker, ref, head) {
			t.Errorf("%s admitted", name)
		}
	}
}

func TestHookMergeGoChecksLiveHeadCIAndIdentity(t *testing.T) {
	a, r := executionTestApp(t)
	head := strings.Repeat("a", 40)
	ref := r.Repository + "#7"
	if err := a.store.Record(event(loop.LoopParty, "supervisor.go", "[go %s@%s] %s: merge go", ref, head, r.Name)); err != nil {
		t.Fatal(err)
	}
	old := loop.ReadLoopPull
	t.Cleanup(func() { loop.ReadLoopPull = old })
	pull := superloop.Pull{State: superloop.Open, Head: head, Green: true}
	loop.ReadLoopPull = func(context.Context, string) (superloop.Pull, error) { return pull, nil }
	if why := a.hookMergeGo(r.ConversationID, r.Repository, 7, head); why != "" {
		t.Fatal(why)
	}
	if why := a.hookMergeGo("other-session", r.Repository, 7, head); why == "" {
		t.Fatal("other worker admitted")
	}
	pull.Head = strings.Repeat("b", 40)
	if why := a.hookMergeGo(r.ConversationID, r.Repository, 7, head); why == "" {
		t.Fatal("moved head admitted")
	}
	pull.Head = head
	pull.Green = false
	if why := a.hookMergeGo(r.ConversationID, r.Repository, 7, head); why == "" {
		t.Fatal("red CI admitted")
	}
	pull.Green = true
	pull.State = superloop.Merged
	if why := a.hookMergeGo(r.ConversationID, r.Repository, 7, head); why == "" {
		t.Fatal("already merged admitted")
	}
}

func TestHookMergeGoDoesNotInferAuthorityFromGreenCI(t *testing.T) {
	a, r := executionTestApp(t)
	old := loop.ReadLoopPull
	t.Cleanup(func() { loop.ReadLoopPull = old })
	loop.ReadLoopPull = func(context.Context, string) (superloop.Pull, error) {
		t.Fatal("read GitHub before checking durable go")
		return superloop.Pull{}, nil
	}
	if why := a.hookMergeGo(r.ConversationID, r.Repository, 7, strings.Repeat("a", 40)); !strings.Contains(why, "no go from the supervisor") {
		t.Fatalf("missing-go refusal: %s", why)
	}
}

func TestHookMergeGoUsesHarnessRosterIdentity(t *testing.T) {
	for _, harness := range []struct{ variable, prefix string }{{codex.EnvAgent, codex.HostPrefix}, {agy.EnvAgent, agy.HostPrefix}} {
		t.Run(harness.prefix, func(t *testing.T) {
			a, r := executionTestApp(t)
			t.Setenv(codex.EnvAgent, "")
			t.Setenv(agy.EnvAgent, "")
			t.Setenv(harness.variable, "fixture-agent")
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents[0].Party = state.Party{Name: r.Name, HostSession: harness.prefix + "fixture-agent"}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			head := strings.Repeat("a", 40)
			if err := a.store.Record(event(loop.LoopParty, "supervisor.go", "[go %s#7@%s] %s: merge go", r.Repository, head, r.Name)); err != nil {
				t.Fatal(err)
			}
			old := loop.ReadLoopPull
			t.Cleanup(func() { loop.ReadLoopPull = old })
			loop.ReadLoopPull = func(context.Context, string) (superloop.Pull, error) {
				return superloop.Pull{State: superloop.Open, Head: head, Green: true}, nil
			}
			if why := a.hookMergeGo("thread-id", r.Repository, 7, head); why != "" {
				t.Fatal(why)
			}
		})
	}
}

func TestReviewMergeGoRevocations(t *testing.T) {
	worker := state.Party{Name: workerRole, Session: "worker-session"}
	ref, head := "person/project#7", strings.Repeat("a", 40)
	grant := event(loop.LoopParty, "supervisor.go", "[go %s@%s] worker: merge go", ref, head)
	for _, revoke := range []state.Event{
		event(loop.LoopParty, "supervisor.forward", "[forward %s review-id] worker: changes requested", ref),
		event(loop.LoopParty, "supervisor.done", "[done %s worker] worker: retired", ref),
		event(loop.LoopParty, "supervisor.done", "[done agents.done@2026-11-01T00:00:00Z worker] worker: retired"),
		event(loop.LoopParty, "supervisor.end", "[end %s] closed", ref),
	} {
		if recordedMergeGo([]state.Event{grant, revoke}, worker, ref, head) {
			t.Errorf("go survived %s %s", revoke.Verb, revoke.Detail)
		}
	}

	for _, neutral := range []state.Event{
		event(loop.LoopParty, "supervisor.forward", "[forward person/project#70 review-id] worker: changes requested"),
		event(loop.LoopParty, "supervisor.done", "[done %s reviewer] reviewer: retired", ref),
		event(loop.LoopParty, "supervisor.done", "[done agents.done@2026-11-01T00:00:00Z reviewer] reviewer: retired"),
		event(loop.LoopParty, "supervisor.end", "[end person/project#70] closed"),
	} {
		if !recordedMergeGo([]state.Event{grant, neutral}, worker, ref, head) {
			t.Errorf("unrelated event revoked go: %s", neutral.Detail)
		}
	}
}

func TestHookMergeGoRefusesWhenLatestReviewRequestsChanges(t *testing.T) {
	a, r := executionTestApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Party = state.Party{Name: r.Name, Session: "thread-id"}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	if err := a.store.Record(event(loop.LoopParty, "supervisor.go", "[go %s#7@%s] %s: merge go", r.Repository, head, r.Name)); err != nil {
		t.Fatal(err)
	}
	old := loop.ReadLoopPull
	t.Cleanup(func() { loop.ReadLoopPull = old })
	loop.ReadLoopPull = func(context.Context, string) (superloop.Pull, error) {
		return superloop.Pull{
			State: superloop.Open,
			Head:  head,
			Green: true,
			Reviews: []superloop.Review{
				{ID: "r1", Head: head, Verdict: superloop.Approved, At: time.Unix(100, 0)},
				{ID: "r2", Head: head, Verdict: superloop.ChangesRequested, At: time.Unix(200, 0)},
			},
		}, nil
	}
	if why := a.hookMergeGo("thread-id", r.Repository, 7, head); !strings.Contains(why, "review requested changes") {
		t.Fatalf("hookMergeGo allowed merge over changes requested: %q", why)
	}
}
