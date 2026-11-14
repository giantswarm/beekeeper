package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/decision"
	"github.com/giantswarm/beekeeper/internal/history"
)

// memReader is a history.Reader over decisions in memory, paged like the Postgres reader.
type memReader struct {
	rows []history.Decision
	page int
}

func (m *memReader) Decisions(_ context.Context, f history.DecisionFilter) ([]history.Decision, error) {
	var out []history.Decision
	for _, d := range m.rows {
		if d.ID > f.AfterID && (f.Point == "" || d.Point == f.Point) && (f.Subject == "" || d.Subject == f.Subject) {
			out = append(out, d)
			if m.page > 0 && len(out) == m.page {
				break
			}
		}
	}
	return out, nil
}

func (m *memReader) Events(context.Context, history.EventFilter) ([]history.Event, error) {
	return nil, nil
}
func (m *memReader) Durations(context.Context, string, string) ([]time.Duration, error) {
	return nil, nil
}
func (m *memReader) MaxIDs(context.Context) (int64, int64, error)        { return 0, 0, nil }
func (m *memReader) DecisionKeys(context.Context) ([]history.Key, error) { return nil, nil }

var spec = &v1alpha1.InstallationSpec{Ready: v1alpha1.ReadySpec{
	AcceptanceCriteria: `(?im)^#{1,4}\s*acceptance criteria`,
	TargetRepo:         `(?im)^(repo|repository):\s*([\w.-]+/[\w.-]+)`,
	PlanningRepos:      []string{"giantswarm/roadmap"},
}}

// generate makes n decisions of every phase 1 point, recorded as Postgres returns them: inputs
// round-tripped through JSON.
func generate(t testing.TB, n int) []history.Decision {
	t.Helper()
	rows := make([]history.Decision, 0, n)
	for i := range n {
		ref := fmt.Sprintf("giantswarm/roadmap#%d", 1000+i)
		var q decision.Question
		switch i % 4 {
		case 0:
			body := fmt.Sprintf("### Acceptance criteria\n- [ ] case %d\nRepo: giantswarm/klaus\n", i)
			if i%8 == 0 {
				body = fmt.Sprintf("We should look at %d at some point.", i)
			}
			q = decision.ReadyQuestion(spec, "bumblebee", ref, decision.ReadyFacts{Title: "Item", Body: body,
				Repo: "giantswarm/roadmap", Bug: i%12 == 0})
		case 1:
			q = decision.BugSeverityQuestion(spec, "bumblebee", ref, "Bug", "It breaks.")
		case 2:
			fp := []string{fmt.Sprintf("pkg/a%d", i%5)}
			other := []string{fmt.Sprintf("pkg/a%d", i%3)}
			q = decision.ParallelSafeQuestion(spec, "bumblebee", ref, "giantswarm/roadmap#1", "giantswarm/klaus", fp, other)
		default:
			p1, p2 := i%3, 2
			cands := []decision.Candidate{
				{Ref: ref, Title: "A", Priority: &p1, CreatedAt: "2026-11-10T10:00:00Z"},
				{Ref: "giantswarm/roadmap#2", Title: "B", Priority: &p2, CreatedAt: "2026-11-09T10:00:00Z", Unblocks: i%7 == 0},
			}
			q = decision.NextQuestion(spec, "bumblebee", "alice", cands)
		}
		b, err := json.Marshal(q.Inputs)
		if err != nil {
			t.Fatal(err)
		}
		var inputs map[string]any
		if err := json.Unmarshal(b, &inputs); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, history.Decision{ID: int64(i + 1), Point: q.Point, Subject: q.Subject, Other: q.Other,
			Inputs: inputs, InputsHash: q.InputsHash, Rule: q.Rule, Chosen: q.Rule, Mode: q.Mode})
	}
	return rows
}

// TestLargeHistory replays a generated history of 10,000 decisions (CI job test-large-history).
func TestLargeHistory(t *testing.T) {
	t.Parallel()
	const n = 10000
	r := &memReader{rows: generate(t, n), page: 1000}
	start := time.Now()
	decisions, mismatches, err := Run(context.Background(), r, WithSpec(spec))
	if err != nil {
		t.Fatal(err)
	}
	if decisions != n || mismatches != 0 {
		t.Fatalf("replayed %d decisions with %d mismatches, want %d and 0", decisions, mismatches, n)
	}
	t.Logf("replayed %d decisions in %s", decisions, time.Since(start))
}

func TestReplayFindsMismatches(t *testing.T) {
	t.Parallel()
	rows := generate(t, 40)
	rows[3].Rule = "giantswarm/roadmap#nobody"
	rows[10].Point = "unknown?"
	var seen []Mismatch
	res, err := Replay(context.Background(), &memReader{rows: rows, page: 7}, WithSpec(spec),
		OnMismatch(func(m Mismatch) { seen = append(seen, m) }))
	if err != nil {
		t.Fatal(err)
	}
	if res.Decisions != 40 || len(res.Mismatches) != 2 || len(seen) != 2 {
		t.Fatalf("result = %+v", res)
	}
	if res.Mismatches[0].ID != 4 || res.Mismatches[1].Err == nil {
		t.Errorf("mismatches = %+v", res.Mismatches)
	}
}

func TestReplayUnpagedReader(t *testing.T) {
	t.Parallel()
	d, m, err := Run(context.Background(), &memReader{rows: generate(t, 9)}, WithSpec(spec))
	if err != nil || d != 9 || m != 0 {
		t.Fatalf("Run = %d, %d, %v", d, m, err)
	}
}
