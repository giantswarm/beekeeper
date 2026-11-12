//go:build scenario && linux

package scenario

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A mutation control rebuilds the candidate with one historical defect of
// Mutations put back into its source and runs a scenario that must catch
// it: the scenario fails on an assertion (not on loading or the harness),
// and the candidate's own output names the defect's refusal.
type mutationControl struct {
	// mutation names the defect in Mutations.
	mutation string
	// scenario runs against the mutant, relative to scenarios/.
	scenario string
	// failure is a substring of the assertion failure the mutant must
	// cause; refusal one of a step's output that names why.
	failure, refusal string
}

// mutationControls are the defects the scenarios must catch.
var mutationControls = []mutationControl{
	{
		// The pull parser dropped the base branch, and a merge of a
		// reviewed pull request had to refuse before any merge: it was
		// internal/loopworkflow's TestHistoricalParserRegression.
		mutation: "pull-parser-drops-the-base",
		scenario: "merge/approved-pull-merges-without-a-checkout.yaml",
		failure:  "want merged",
		refusal:  "has no base branch",
	},
}

// TestMutationControls runs every mutation control: each scenario passes on
// the candidate (TestScenarios) and fails on its mutant.
func TestMutationControls(t *testing.T) {
	r := runner(t)
	for _, c := range mutationControls {
		t.Run(c.mutation, func(t *testing.T) {
			s, err := Load(filepath.Join("scenarios", c.scenario))
			if err != nil {
				t.Fatal(err)
			}
			mutant := buildMutant(t, c)
			artifacts := t.TempDir()
			res := (&Runner{Candidate: mutant, Fake: r.Fake, Artifacts: artifacts}).Run(t.Context(), s, t.TempDir())
			status, err := Outcome(res)
			if res.Harness != "" {
				t.Fatalf("the harness failed, not the scenario: %s", res.Harness)
			}
			if status != StatusFailed || err == nil {
				t.Fatalf("%s passes against the mutant %s: it does not catch the defect", c.scenario, c.mutation)
			}
			if !slices.ContainsFunc(res.Failures, func(f string) bool { return strings.Contains(f, c.failure) }) {
				t.Fatalf("the mutant failed elsewhere than on %q:\n%s", c.failure, strings.Join(res.Failures, "\n"))
			}
			if !outputNames(t, filepath.Join(artifacts, s.Flow, s.Name), c.refusal) {
				t.Fatalf("no step's output of the mutant names %q", c.refusal)
			}
			raw, _ := json.MarshalIndent(res, "", "  ")
			t.Logf("%s caught by %s:\n%s", c.mutation, c.scenario, raw)
		})
	}
}

// buildMutant builds the candidate with c's defect through a build overlay,
// leaving the source tree untouched.
func buildMutant(t *testing.T, c mutationControl) string {
	t.Helper()
	m, ok := Mutations[c.mutation]
	if !ok {
		t.Fatalf("no mutation %q", c.mutation)
	}
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	overlay, err := m.Overlay(repo, dir)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "beekeeper")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-tags", "scenario", "-overlay", overlay, "-o", out, ".") //nolint:gosec // the repository's own build
	cmd.Dir = repo
	started := time.Now()
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the mutant %s: %v\n%s", c.mutation, err, b)
	}
	t.Logf("mutant %s built in %s", c.mutation, time.Since(started).Round(time.Millisecond))
	return out
}

// outputNames says whether a step's output among a scenario's artifacts
// holds text.
func outputNames(t *testing.T, dir, text string) bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.std*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // the scenario's own artifacts
		if err == nil && strings.Contains(string(b), text) {
			return true
		}
	}
	return false
}
