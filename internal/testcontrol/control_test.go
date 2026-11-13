package testcontrol

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// historyEnv set to 1 runs TestControlProvesTheClockFix, which
// .circleci/scenario-control.sh does when a pull request changes the
// control.
const historyEnv = "BEEKEEPER_TEST_CONTROL_HISTORY"

// clockFix is the fix of https://github.com/giantswarm/beekeeper/issues/3830
// (https://github.com/giantswarm/beekeeper/pull/3832): its regression test
// is a Go test under internal/clock that builds with the scenario tag, the
// case the control of scenarios alone left unproven.
const clockFix = "f785bad40fb173690ef8ffed58bdec1f33aaf7a8"

// toplevel is the root of the repository this package is in.
func toplevel(t *testing.T) string {
	t.Helper()
	root, err := git(t.Context(), ".", "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(root)
}

// report logs the control's report and, with dir, writes it there.
func report(t *testing.T, rep *Report, dir string) {
	t.Helper()
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
}

// TestGoTestControl is the Go-test control of a pull request
// (.circleci/scenario-control.sh): the Test functions it adds or changes
// run against its merge base, checked out where
// BEEKEEPER_TEST_CONTROL_BASE names, with its test files applied, and at
// least one must fail there and pass on its code. Without the variable it
// skips.
func TestGoTestControl(t *testing.T) {
	base := os.Getenv(BaseEnv)
	if base == "" {
		t.Skipf("%s names no merge base: .circleci/scenario-control.sh runs the control", BaseEnv)
	}
	head := os.Getenv(HeadEnv)
	if head == "" {
		head = toplevel(t)
	}
	dir := os.Getenv(ReportEnv)
	o := Options{Head: head, Base: base}
	if dir != "" {
		o.Logs = filepath.Join(dir, "runs")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 11*time.Minute)
	defer cancel()
	rep, err := Control(ctx, o)
	if err != nil {
		t.Fatalf("the control could not run, so it shows nothing: %v", err)
	}
	report(t, rep, dir)
	if !rep.OK {
		t.Fatal(rep.Summary)
	}
}

// The control on the history of the clock fix: its regression test fails
// on the fix's parent, with the scenario tag its file needs, and passes
// on the fix, so the control passes; the test the fix only reworked
// passes on the parent.
func TestControlProvesTheClockFix(t *testing.T) {
	if os.Getenv(historyEnv) != "1" {
		t.Skipf("%s=1 runs the control on the clock fix: .circleci/scenario-control.sh does when the control changes", historyEnv)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	repo := toplevel(t)
	if _, err := git(ctx, repo, "cat-file", "-e", clockFix+"~1"); err != nil {
		if _, err := git(ctx, repo, "fetch", "--quiet", "origin", clockFix); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	head, base := filepath.Join(dir, "head"), filepath.Join(dir, "base")
	for tree, rev := range map[string]string{head: clockFix, base: clockFix + "^"} {
		if _, err := git(ctx, repo, "worktree", "add", "--quiet", "--detach", tree, rev); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = git(context.Background(), repo, "worktree", "remove", "--force", tree) })
	}
	var logs string
	if d := os.Getenv(ReportEnv); d != "" {
		logs = filepath.Join(d, "runs")
	}
	rep, err := Control(ctx, Options{Head: head, Base: base, Logs: logs})
	if err != nil {
		t.Fatal(err)
	}
	report(t, rep, os.Getenv(ReportEnv))
	var regression, reworked *Case
	for _, c := range rep.Cases {
		switch c.Dir + "." + c.Name {
		case "internal/clock.TestAdvanceDoesNotWaitForAProcessThatEndsDuringIt":
			regression = c
		case "internal/clock.TestAdvanceNamesAProcessThatDoesNotAnswer":
			reworked = c
		}
	}
	if regression == nil || regression.Change != ChangeAdded || !slices.Equal(regression.Tags, []string{"scenario"}) || !regression.Shows() {
		t.Errorf("the clock fix's regression test: %+v; want added, run with -tags scenario, failing on the parent and passing on the fix", regression)
	}
	if reworked == nil || reworked.Change != ChangeChanged || reworked.Base != Passes {
		t.Errorf("the test the fix reworked: %+v; want changed and passing on the parent", reworked)
	}
	if !rep.OK {
		t.Errorf("the control failed the clock fix: %s", rep.Summary)
	}
}
