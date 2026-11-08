package cmd

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	productionDelete = "kubectl --context production delete job stale --namespace monitoring"
	noteApproval     = "yes"
)

func TestProductionNoteAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, channel, answer, question, want string
		age                                   time.Duration
	}{
		{name: "approved", channel: viaCLI, answer: "delete it"},
		{name: "declined", channel: viaCLI, answer: "no", want: "does not explicitly approve"},
		{name: "different object", channel: viaCLI, answer: noteApproval, question: strings.ReplaceAll(productionDelete, "stale", "other"), want: "exact command"},
		{name: "expired", channel: viaCLI, answer: noteApproval, age: 25 * time.Hour, want: "older"},
		{name: testFuture, channel: viaCLI, answer: noteApproval, age: -time.Hour, want: testFuture},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := noteApp(t)
			question := tc.question
			if question == "" {
				question = productionDelete
			}
			ev := state.Event{At: time.Now().Add(-tc.age), Verb: answerRecorded, Detail: fmt.Sprintf("o/r#1 answered by %q via %s, note #42 asked by %q for %q: %q: %s", "person", tc.channel, "agent", "person", "Run `"+question+"`?", tc.answer)}
			if err := a.store.Record(ev); err != nil {
				t.Fatal(err)
			}
			a.cfg.Kube.ProductionNoteDeletes = true
			h := guard.Hook{Production: "production", ProductionNote: a.productionNoteAuthority()}
			raw := []byte(fmt.Sprintf(`{"tool_name":"Bash","tool_input":{"command":%q}}`, "BEEKEEPER_PROD_NOTE=42 "+productionDelete))
			out := string(h.Decide(raw))
			if tc.want != "" {
				if !strings.Contains(out, tc.want) {
					t.Fatalf("got %s, want %s", out, tc.want)
				}
				return
			}
			if strings.Contains(out, "deny") {
				t.Fatal(out)
			}
			// Reopen as a fresh hook process: consumption must survive restart.
			a.cfg.StateDir = a.store.Dir()
			a.store = nil
			if out = string(h.Decide(raw)); !strings.Contains(out, "already been used") {
				t.Fatal(out)
			}
			evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "production.write" })
			if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Detail, "note #42") || !strings.Contains(evs[0].Detail, productionDelete) {
				t.Fatalf("audit: %v %v", evs, err)
			}
		})
	}
}

func TestProductionNoteConcurrentSingleUse(t *testing.T) {
	a, _ := noteApp(t)
	ev := state.Event{At: time.Now(), Verb: answerRecorded, Detail: fmt.Sprintf("o/r#1 answered by %q via cli, note #42 asked by %q for %q: %q: yes", "person", "agent", "person", "Run `"+productionDelete+"`?")}
	if err := a.store.Record(ev); err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if a.consumeProductionNote(42, productionDelete) == nil {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d writes allowed", wins.Load())
	}
}

// Merging the path opens nothing: unconfigured, the hook gets no authority and
// a marked deletion meets the ordinary refusal, however good its answer.
func TestProductionNoteIsOffByDefault(t *testing.T) {
	a, _ := noteApp(t)
	ev := state.Event{At: time.Now(), Verb: answerRecorded, Detail: fmt.Sprintf("o/r#1 answered by %q via cli, note #42 asked by %q for %q: %q: yes", "person", "agent", "person", "`"+productionDelete+"`")}
	if err := a.store.Record(ev); err != nil {
		t.Fatal(err)
	}
	if a.productionNoteAuthority() != nil {
		t.Fatal("note authority without kube.productionNoteDeletes")
	}
	h := guard.Hook{Production: "production", ProductionNote: a.productionNoteAuthority()}
	raw := []byte(fmt.Sprintf(`{"tool_name":"Bash","tool_input":{"command":%q}}`, "BEEKEEPER_PROD_NOTE=42 "+productionDelete))
	if out := string(h.Decide(raw)); !strings.Contains(out, "deny") || !strings.Contains(out, "a production cluster") {
		t.Fatal(out)
	}
	st, err := a.store.Read()
	if err != nil || len(st.ProductionNotes) != 0 {
		t.Fatalf("note consumed: %v, %v", st.ProductionNotes, err)
	}
}

// This pins the limit the configuration key warns of, not a wanted behaviour:
// switched on, an answer an agent recorded authorises as a person's does, since
// answer.recorded proves nobody's hand. If this test ever fails because a
// forged answer is refused, the warning can go.
func TestProductionNoteCannotTellWhoAnswered(t *testing.T) {
	a, _ := noteApp(t)
	a.cfg.Kube.ProductionNoteDeletes = true
	forged := state.Event{By: state.Party{Name: "an agent"}, At: time.Now(), Verb: answerRecorded, Detail: fmt.Sprintf("o/r#1 answered by %q via cli, note #42 asked by %q for %q: %q: yes", "person", "an agent", "person", "`"+productionDelete+"`")}
	if err := a.store.Record(forged); err != nil {
		t.Fatal(err)
	}
	if err := a.consumeProductionNote(42, productionDelete); err != nil {
		t.Fatalf("the documented limit changed: %v", err)
	}
}
