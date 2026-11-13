//go:build scenario && linux

package scenario

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestScenarioControl is the control of a pull request
// (.circleci/scenario-control.sh): every scenario it adds or changes runs
// against the candidate built from its merge base, the source tree that
// BEEKEEPER_SCENARIO_CONTROL_BASE names, and must fail there on an
// assertion. Without the variable it skips. Under BEEKEEPER_SCENARIO_SHARD
// it controls the scenarios of that shard, the ones the shard's suite runs.
//
// control: none: it gains only the shard filter, which the merge base
// lacks; the control itself is unchanged and runs in CI's step
func TestScenarioControl(t *testing.T) {
	base := os.Getenv(ControlEnv)
	if base == "" {
		t.Skipf("%s names no merge base: .circleci/scenario-control.sh runs the control", ControlEnv)
	}
	all, err := LoadDir("scenarios")
	if err != nil {
		t.Fatalf("the pull request's scenarios do not load, so the control shows nothing: %v", err)
	}
	sh := shard(t)
	plan, err := PlanControl(sh.Of(all), "scenarios", filepath.Join(base, "internal", "scenario", "scenarios"))
	if err != nil {
		t.Fatal(err)
	}
	rep := &ControlReport{Base: "the merge base at " + base}
	if sh.Total > 1 {
		rep.Base += ", shard " + sh.String()
	}
	for _, c := range plan.Skip {
		rep.Entry(c).OK = true
	}
	var dir string
	if d := os.Getenv(ReportEnv); d != "" {
		dir = filepath.Join(d, "control")
	}
	defer func() {
		var text strings.Builder
		if err := rep.Text(&text); err != nil {
			t.Error(err)
		}
		t.Log("\n" + text.String())
		if dir != "" {
			if err := rep.Write(dir); err != nil {
				t.Errorf("control report: %v", err)
			}
		}
	}()
	if len(plan.Run) == 0 {
		return
	}
	if err := Namespaces(); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(t.TempDir(), "beekeeper")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	if err := Build(ctx, base, candidate); err != nil {
		t.Fatalf("the merge base does not build, so the control shows nothing: %v", err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A scenario's releases (given.releases) are builds of the merge base
	// too: the control judges the base, whatever version it is stamped with.
	releases := &ReleaseBuilder{Repo: base, Dir: filepath.Dir(candidate)}
	r := &Runner{Candidate: candidate, Fake: self, Artifacts: dir, Release: releases.Build}
	var mu sync.Mutex
	t.Run("control", func(t *testing.T) {
		for _, c := range plan.Run {
			t.Run(c.Scenario.Flow+"/"+c.Scenario.Name, func(t *testing.T) {
				t.Parallel()
				res := r.Run(t.Context(), c.Scenario, t.TempDir())
				verdict, err := ControlVerdict(c, res)
				mu.Lock()
				e := rep.Entry(c)
				e.Verdict, e.OK = verdict, err == nil
				if err != nil {
					e.Detail = err.Error()
				} else {
					e.Detail = firstFailures(res, 3)
				}
				mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("%s: %s", verdict, firstFailures(res, 3))
			})
		}
	})
}

// The control's verdicts on real runs, with this candidate standing in
// for a merge base: a scenario that passes on it shows nothing and fails
// the control; one that fails there on an assertion passes it; one whose
// run fails in the harness is no evidence.
func TestControlVerdictsOnRealRuns(t *testing.T) {
	r := runner(t)
	for _, tc := range []struct {
		name, doc, verdict string
		ok                 bool
	}{
		{"passes on the base", `
name: control-passes
flow: harness
when:
  - run: {args: [version]}
then:
  - exit: 0
`, VerdictPasses, false},
		{"fails on an assertion", `
name: control-fails
flow: harness
when:
  - run: {args: [version]}
then:
  - output: {contains: [behavior the base does not have]}
`, VerdictFails, true},
		{"fails on an unanswered call", `
name: control-unanswered
flow: harness
when:
  - run: {args: [board, next]}
then:
  - calls: {program: gh, min: 1}
`, VerdictFails, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := mustParse(t, tc.doc)
			c := &ControlCase{Scenario: s, Rel: "harness/" + s.Name + ".yaml", Change: ChangeAdded}
			res := r.Run(t.Context(), s, t.TempDir())
			verdict, err := ControlVerdict(c, res)
			if verdict != tc.verdict || (err == nil) != tc.ok {
				t.Fatalf("verdict %q, %v; want %q, ok %t (result %+v)", verdict, err, tc.verdict, tc.ok, res)
			}
		})
	}
	t.Run("harness failure", func(t *testing.T) {
		t.Parallel()
		s := mustParse(t, "name: control-harness\nflow: harness\nwhen:\n  - run: {args: [version]}\nthen:\n  - exit: 0\n")
		// The driver cannot read a scenario that is gone: a harness failure.
		s.File = filepath.Join(t.TempDir(), "gone.yaml")
		res := r.Run(t.Context(), s, t.TempDir())
		verdict, err := ControlVerdict(&ControlCase{Scenario: s, Rel: "harness/gone.yaml"}, res)
		if verdict != VerdictHarness || err == nil || !strings.Contains(err.Error(), "not on an assertion") {
			t.Fatalf("verdict %q, %v; want a harness failure that is no evidence", verdict, err)
		}
	})
}
