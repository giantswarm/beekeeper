//go:build scenario

package scenario

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The control key is {none: <reason>} with a reason in words, nothing
// else.
func TestControlKey(t *testing.T) {
	ok := validScenario + "control: {none: states behavior main already has}\n"
	s, err := Parse("ok.yaml", []byte(ok))
	if err != nil || s.Control == nil || s.Control.None != "states behavior main already has" {
		t.Fatalf("a valid opt-out: %+v, %v", s, err)
	}
	if s, err := Parse("none.yaml", []byte(validScenario)); err != nil || s.Control != nil {
		t.Fatalf("a scenario without control: %+v, %v", s, err)
	}
	for _, tc := range []struct{ name, doc, want string }{
		{"a bare none", validScenario + "control: none\n", "control is {none: <reason>}"},
		{"no reason", validScenario + "control: {none: ''}\n", "say in words why"},
		{"a reason of one word", validScenario + "control: {none: n/a}\n", "say in words why"},
		{"an unknown key", validScenario + "control: {none: states behavior main has, base: abc}\n", "field base not found in control"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.name+".yaml", []byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// writeScenarios writes files (path under root: content) and returns the
// root.
func writeScenarios(t *testing.T, files map[string]string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "scenarios")
	for rel, doc := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// scenarioDoc is a scenario of name and flow that runs version and expects
// exit, with extra top-level lines.
func scenarioDoc(name, flow string, exit int, extra ...string) string {
	return fmt.Sprintf("name: %s\nflow: %s\nwhen:\n  - run: {args: [version]}\nthen:\n  - exit: %d\n%s", name, flow, exit, strings.Join(extra, "\n"))
}

// The control runs what a pull request adds and what it changes in given,
// when, then or timeout; it lists metadata-only changes and the opt-outs
// the pull request writes; an opt-out of the base does not cover a new
// change; unchanged and removed scenarios are not part of it.
func TestPlanControl(t *testing.T) {
	const reason = "control: {none: states behavior main already has}"
	base := writeScenarios(t, map[string]string{
		"smoke/unchanged.yaml":  scenarioDoc("unchanged", "smoke", 0),
		"smoke/extended.yaml":   scenarioDoc("extended", "smoke", 0),
		"smoke/reworded.yaml":   scenarioDoc("reworded", "smoke", 0, "description: old words"),
		"smoke/unpended.yaml":   scenarioDoc("unpended", "smoke", 0, "pending: https://github.com/example/app/issues/1"),
		"smoke/moved.yaml":      scenarioDoc("moved", "smoke", 0),
		"smoke/covered.yaml":    scenarioDoc("covered", "smoke", 0, reason),
		"smoke/recovered.yaml":  scenarioDoc("recovered", "smoke", 0, reason),
		"smoke/newly-out.yaml":  scenarioDoc("newly-out", "smoke", 0),
		"smoke/removed.yaml":    scenarioDoc("removed", "smoke", 0),
		"smoke/renamed.yaml":    scenarioDoc("old-name", "smoke", 0),
		"smoke/unreadable.yaml": "name: [unreadable\n",
	})
	pr := writeScenarios(t, map[string]string{
		"smoke/unchanged.yaml":  scenarioDoc("unchanged", "smoke", 0),
		"smoke/extended.yaml":   scenarioDoc("extended", "smoke", 0) + "  - calls: {max: 0}\n",
		"smoke/reworded.yaml":   "# a comment\n" + scenarioDoc("reworded", "smoke", 0, "description: new words", "tags: [smoke]"),
		"smoke/unpended.yaml":   scenarioDoc("unpended", "smoke", 0),
		"other/moved.yaml":      scenarioDoc("moved", "other", 0),
		"smoke/covered.yaml":    scenarioDoc("covered", "smoke", 3, reason),
		"smoke/recovered.yaml":  scenarioDoc("recovered", "smoke", 3, "control: {none: the exit code only names a new error class}"),
		"smoke/newly-out.yaml":  scenarioDoc("newly-out", "smoke", 3, reason),
		"smoke/renamed.yaml":    scenarioDoc("new-name", "smoke", 3),
		"smoke/unreadable.yaml": scenarioDoc("unreadable", "smoke", 0),
		"smoke/added.yaml":      scenarioDoc("added", "smoke", 0),
		"smoke/added-out.yaml":  scenarioDoc("added-out", "smoke", 0, reason),
	})
	all, err := LoadDir(pr)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanControl(all, pr, base)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range plan.Run {
		got[c.Scenario.Name] = "run " + c.Change
		if c.Note != "" {
			got[c.Scenario.Name] += " with note"
		}
	}
	for _, c := range plan.Skip {
		got[c.Scenario.Name] = "skip " + c.Change
		if c.Reason != "" {
			got[c.Scenario.Name] += ": " + c.Reason
		}
	}
	want := map[string]string{
		"extended":   "run changed",
		"reworded":   "skip metadata only",
		"unpended":   "skip metadata only",
		"moved":      "skip metadata only",
		"covered":    "run changed with note",
		"recovered":  "skip opted out (changed): the exit code only names a new error class",
		"newly-out":  "skip opted out (changed): states behavior main already has",
		"new-name":   "run changed",
		"unreadable": "run changed",
		"added":      "run added",
		"added-out":  "skip opted out (added): states behavior main already has",
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s: %q, want %q", name, got[name], w)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s is in the control: %q", name, got[name])
		}
	}
}

// Before the harness the base has no scenarios: everything is added.
func TestPlanControlWithoutBaseScenarios(t *testing.T) {
	pr := writeScenarios(t, map[string]string{"smoke/a.yaml": scenarioDoc("a", "smoke", 0)})
	all, err := LoadDir(pr)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanControl(all, pr, filepath.Join(t.TempDir(), "no-such-dir"))
	if err != nil || len(plan.Run) != 1 || plan.Run[0].Change != ChangeAdded || len(plan.Skip) != 0 {
		t.Fatalf("plan %+v, %v; want a to run as added", plan, err)
	}
}

// A run against the base passes the control only when it fails on an
// assertion; the failure says what to do.
func TestControlVerdict(t *testing.T) {
	c := &ControlCase{Scenario: &Scenario{Name: "a", Flow: "smoke"}, Rel: "smoke/a.yaml", Change: ChangeChanged}
	if v, err := ControlVerdict(c, &Result{Failures: []string{"then[0] exit: exit 0, want 3"}}); v != VerdictFails || err != nil {
		t.Errorf("an assertion failure: %q, %v", v, err)
	}
	if v, err := ControlVerdict(c, &Result{}); v != VerdictPasses || err == nil || !strings.Contains(err.Error(), "control: {none: <why>}") {
		t.Errorf("a pass: %q, %v", v, err)
	}
	if v, err := ControlVerdict(c, &Result{Harness: "no sandbox", Failures: []string{"x"}}); v != VerdictHarness || err == nil {
		t.Errorf("a harness failure: %q, %v", v, err)
	}
	c.Note = "its control: none is the base's"
	if _, err := ControlVerdict(c, &Result{}); err == nil || !strings.Contains(err.Error(), c.Note) {
		t.Errorf("a pass of a stale opt-out does not say why it ran: %v", err)
	}
}

// The report lists every scenario of the control with its verdict and
// counts the failing ones.
func TestControlReportText(t *testing.T) {
	rep := &ControlReport{Base: "abc123"}
	var text strings.Builder
	if err := rep.Text(&text); err != nil || !strings.Contains(text.String(), "no scenario added or changed") {
		t.Fatalf("empty report: %q, %v", text.String(), err)
	}
	rep.Entries = []*ControlEntry{
		{File: "smoke/a.yaml", Change: ChangeAdded, Verdict: VerdictFails, Detail: "then[0] exit: exit 0, want 3", OK: true},
		{File: "smoke/b.yaml", Change: ChangeChanged, Verdict: VerdictPasses, Detail: "smoke/b.yaml: passes on the base", OK: false},
		{File: "smoke/c.yaml", Change: ChangeOptedOut + " (added)", Reason: "states behavior main already has", OK: true},
	}
	text.Reset()
	if err := rep.Text(&text); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"OK   smoke/a.yaml (added): fails on the base",
		"FAIL smoke/b.yaml (changed): passes on the base",
		"reason: states behavior main already has",
		"control: 3 scenario(s), 1 failing",
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, text.String())
		}
	}
}
