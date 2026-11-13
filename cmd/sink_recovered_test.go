package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/gate"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

const mergedSinkYAML = "sink: {repo: o/site, workflow: refresh.yaml, verbs: [merged]}\n"

const sinkReplacement = "replacement"

func recoveredMergeOwed() state.Owed {
	return state.Owed{
		Ask: true, Repo: scratchRepo, PR: 7,
		Command: "devctl pr merge o/r 7", Stop: "SIGTERM (exit 143)",
		Since: time.Now().UTC(),
	}
}

func stubRecoveredPull(t *testing.T) {
	t.Helper()
	prev := gate.PullState
	t.Cleanup(func() { gate.PullState = prev })
	merged := time.Now().UTC()
	gate.PullState = func(_ context.Context, repo string, pr int) (github.Pull, error) {
		if repo != scratchRepo || pr != 7 {
			t.Errorf("looked up %s#%d, want %s", repo, pr, decisionRef)
		}
		return github.Pull{State: github.Merged, MergedAt: merged}, nil
	}
}

// A stopped gate that cannot confirm its merge records merge.unknown.
// Confirmation while handing over the owed outcome must still produce a
// merged event with its subject, so the configured sink sees the merge.
func TestTheSinkDispatchesARecoveredMerge(t *testing.T) {
	fail := false
	calls := fakeSinkGH(t, &fail)
	a, _ := sinkApp(t, mergedSinkYAML)
	w := a.newWatcher(false, true)
	sinkPoll(w)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = append(st.Merges, state.Merge{Repo: scratchRepo, PR: 7, Lane: scratchRepo, Phase: state.Running})
		return recordRun(st, len(st.Merges)-1, config.Lane{Name: scratchRepo}, sinkParty,
			gate.RunOutcome{Rc: 143, Unanswered: errors.New("GitHub unavailable")}, time.Now().UTC(), ""), nil
	}); err != nil {
		t.Fatal(err)
	}

	stubRecoveredPull(t)
	owed := recoveredMergeOwed()
	if _, ok := a.owedLine(context.Background(), owed); !ok {
		t.Fatal("the confirmed merge's outcome was not ready")
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == verbMerged })
	if err != nil || len(evs) != 1 {
		t.Fatalf("recovered merge events: %+v, %v", evs, err)
	}
	if evs[0].Subject != decisionRef {
		t.Fatalf("recovered merge subject %q, want %q", evs[0].Subject, decisionRef)
	}

	sinkPoll(w)
	if len(*calls) != 1 || !strings.Contains((*calls)[0], "-f verb=merged -f subject="+decisionRef+" ") {
		t.Fatalf("the recovered merge did not dispatch its workflow: %q", *calls)
	}
	// A failed owner wake keeps the outcome owed and confirms it again.
	// Fixing the missing subject must not turn that retry into a second
	// workflow run for the same recovered merge.
	if a.store, err = state.Open(a.store.Dir()); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.owedLine(context.Background(), owed); !ok {
		t.Fatal("the repeated confirmation's outcome was not ready")
	}
	sinkPoll(w)
	if len(*calls) != 1 {
		t.Fatalf("the recovered merge was dispatched more than once: %q", *calls)
	}
}

// The gate and its replacement may confirm a merge before or after owed
// recovery. Both paths retain their log details, but dispatch only once.
func TestTheSinkDispatchesAMergeOnceAcrossRecoveryAndGate(t *testing.T) {
	for _, producer := range []string{gate.GateCmdName, sinkReplacement} {
		for _, recoveryFirst := range []bool{false, true} {
			name := producer + "/gate-first"
			if recoveryFirst {
				name = producer + "/recovery-first"
			}
			t.Run(name, func(t *testing.T) {
				fail := false
				calls := fakeSinkGH(t, &fail)
				a, _ := sinkApp(t, mergedSinkYAML)
				w := a.newWatcher(false, true)
				sinkPoll(w)
				stubRecoveredPull(t)
				if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
					st.Merges = []state.Merge{{Repo: scratchRepo, PR: 7, Lane: scratchRepo, Phase: state.Running}}
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
				recover := func() {
					if _, ok := a.owedLine(t.Context(), recoveredMergeOwed()); !ok {
						t.Fatal("the recovered outcome was not ready")
					}
				}
				record := func() {
					base := gate.MergeBase(a.store.Dir(), scratchRepo, 7)
					doc := []byte(`{"mergeCommitSha":"abc","release":{"verdict":"available","tag":"v1.2.3"}}`)
					if producer == gate.GateCmdName {
						g := &gate.GateRun{Host: a, Ctx: t.Context(), Repo: scratchRepo, Pr: 7, Me: sinkParty,
							Lane: config.Lane{Name: scratchRepo, Installation: hungLane}}
						if err := g.Record(base, gate.ChildRun{Doc: doc}); err != nil {
							t.Fatal(err)
						}
						return
					}
					if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(base+".json", doc, 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(base+".rc", []byte("0"), 0o600); err != nil {
						t.Fatal(err)
					}
					recorded, _, lost := a.recordGone(t.Context())
					if len(recorded) != 1 || len(lost) != 0 {
						t.Fatalf("recorded %q, lost %+v", recorded, lost)
					}
				}
				first, second := record, recover
				if recoveryFirst {
					first, second = recover, record
				}
				first()
				sinkPoll(w)
				second()
				sinkPoll(w)
				evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == verbMerged })
				if err != nil || len(evs) != 2 || evs[0].Subject != decisionRef || evs[1].Subject != "" ||
					!strings.Contains(evs[0].Detail+evs[1].Detail, "release v1.2.3") {
					t.Fatalf("merge confirmation details or subjects: %+v, %v", evs, err)
				}
				if len(*calls) != 1 {
					t.Fatalf("dispatched one PR's confirmations %d times: %q", len(*calls), *calls)
				}
			})
		}
	}
}

func TestTheSinkDeduplicatesConcurrentRecoveredMerges(t *testing.T) {
	fail := false
	calls := fakeSinkGH(t, &fail)
	a, _ := sinkApp(t, mergedSinkYAML)
	w := a.newWatcher(false, true)
	sinkPoll(w)
	stubRecoveredPull(t)
	other := *a
	var err error
	other.store, err = state.Open(a.store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var done sync.WaitGroup
	var ready [2]bool
	for i, current := range []*app{a, &other} {
		done.Go(func() {
			<-start
			_, ready[i] = current.owedLine(t.Context(), recoveredMergeOwed())
		})
	}
	close(start)
	done.Wait()
	if !ready[0] || !ready[1] {
		t.Fatalf("confirmed outcomes not ready: %v", ready)
	}
	sinkPoll(w)
	if len(*calls) != 1 {
		t.Fatalf("concurrent confirmations dispatched %d times: %q", len(*calls), *calls)
	}
}

type unreadMergeHistory struct{ state.Store }

func (unreadMergeHistory) Events(int, func(state.Event) bool) ([]state.Event, error) {
	return nil, errors.New("merge history unavailable")
}

func TestARecoveredMergeWaitsUntilItsEventCanBeRecorded(t *testing.T) {
	fail := false
	calls := fakeSinkGH(t, &fail)
	a, _ := sinkApp(t, mergedSinkYAML)
	w := a.newWatcher(false, true)
	sinkPoll(w)
	stubRecoveredPull(t)
	owed := recoveredMergeOwed()
	if err := a.owe(owed); err != nil {
		t.Fatal(err)
	}
	store := a.store
	a.store = unreadMergeHistory{store}
	if _, ok := a.owedLine(t.Context(), owed); ok {
		t.Fatal("a confirmed merge was handed over without recording its event")
	}
	a.store = store
	st, err := a.store.Read()
	if err != nil || len(st.Owed) != 1 || !strings.Contains(st.Owed[0].Why, "merge history unavailable") {
		t.Fatalf("outcome not kept owed with its failure: %+v, %v", st, err)
	}
	if _, ok := a.owedLine(t.Context(), owed); !ok {
		t.Fatal("a recordable confirmed merge was not ready")
	}
	sinkPoll(w)
	if len(*calls) != 1 {
		t.Fatalf("the retried confirmation dispatched %d times: %q", len(*calls), *calls)
	}
}

// A history read failure must leave the runner's evidence for a later poll.
func TestMergeRecordersKeepResultsWhenHistoryFails(t *testing.T) {
	for _, producer := range []string{gate.GateCmdName, sinkReplacement} {
		t.Run(producer, func(t *testing.T) {
			a, _ := sinkApp(t, mergedSinkYAML)
			base := gate.MergeBase(a.store.Dir(), scratchRepo, 7)
			doc := []byte(`{"mergeCommitSha":"abc","release":{"verdict":"available","tag":"v1.2.3"}}`)
			if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
				t.Fatal(err)
			}
			for suffix, data := range map[string][]byte{".json": doc, ".rc": []byte("0")} {
				if err := os.WriteFile(base+suffix, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Merges = []state.Merge{{Repo: scratchRepo, PR: 7, Lane: scratchRepo, Phase: state.Running}}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			store := a.store
			a.store = unreadMergeHistory{store}
			if producer == gate.GateCmdName {
				g := &gate.GateRun{Host: a, Ctx: t.Context(), Repo: scratchRepo, Pr: 7, Me: sinkParty,
					Lane: config.Lane{Name: scratchRepo, Installation: hungLane}}
				if err := g.Record(base, gate.ChildRun{Doc: doc}); err == nil {
					t.Fatal("recording failure was hidden")
				}
			} else {
				recorded, _, _ := a.recordGone(t.Context())
				if len(recorded) != 0 {
					t.Fatalf("reported unrecorded results: %v", recorded)
				}
			}
			for _, suffix := range []string{".json", ".rc"} {
				if _, err := os.Stat(base + suffix); err != nil {
					t.Fatalf("lost retry evidence: %v", err)
				}
			}
			a.store = store
			recorded, _, _ := a.recordGone(t.Context())
			if len(recorded) != 1 {
				t.Fatalf("retry did not record saved result: %v", recorded)
			}
			evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == verbMerged })
			if err != nil || len(evs) != 1 || evs[0].Subject != decisionRef || !strings.Contains(evs[0].Detail, "release v1.2.3") {
				t.Fatalf("retry lost merge confirmation: %+v, %v", evs, err)
			}
		})
	}
}
