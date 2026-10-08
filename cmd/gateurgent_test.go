//go:build unix

package cmd

import (
	"bytes"
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// supervisorName is who marks a merge urgent in these tests.
const supervisorName = "supervisor"

// urgentApp is queueApp under the budget floor: 40 left of the floor 100,
// its reset in half an hour, an urgent merge's own bound 20.
func urgentApp(t *testing.T, merges ...state.Merge) *app {
	t.Helper()
	a := queueApp(t, merges...)
	a.cfg.Merge.BudgetFresh = config.Duration{Duration: time.Hour}
	a.cfg.GitHub.UrgentBound = 20
	setBudget(t, a, 40)
	return a
}

func setBudget(t *testing.T, a *app, remaining int) {
	t.Helper()
	reset := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Budget = &state.Budget{Remaining: remaining, Limit: 5000, Reset: reset, At: time.Now()}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// markUrgent runs lanes urgent repo pr as the supervisor and returns its
// output.
func markUrgent(t *testing.T, a *app, pr string) (string, error) {
	t.Helper()
	t.Setenv("CLAUDE_CODE_SESSION_NAME", supervisorName)
	defer t.Setenv("CLAUDE_CODE_SESSION_NAME", ownerName)
	var out bytes.Buffer
	a.out = &out
	c := a.urgentCmd()
	c.SetArgs([]string{scratchRepo, pr, "--reason", "a privacy fix"})
	c.SetOut(&out)
	c.SetErr(&out)
	err := c.Execute()
	return out.String(), err
}

// A merge marked urgent runs while the budget is under the floor: once, as
// the reset window's urgent merge, against a bound of its own; the log and
// beekeeper budget name it and who asked.
func TestAnUrgentMergeRunsUnderTheBudgetFloor(t *testing.T) {
	noSystemd(t)
	fakeDevctl(t, `echo '`+mergedDoc+`'`)
	stubGitHub(t, github.Merged, "")
	stubBaseRelease(t, github.BaseRelease{Base: mainBranch, Auto: true}, nil)
	a := urgentApp(t)
	if out, err := markUrgent(t, a, "7"); err != nil || !strings.Contains(out, "marked o/r#7 urgent") {
		t.Fatalf("lanes urgent: %v\n%s", err, out)
	}

	if err := a.gate(context.Background(), mergeArgv(scratchRepo), time.Minute, false); err != nil {
		t.Fatalf("exit %d (%v), want the merge run under the floor", Code(err), err)
	}
	if d := lastEventOf(t, a, verbMerged); !strings.Contains(d, "o/r#7 exit 0") {
		t.Errorf("merged event %q", d)
	}
	if d := lastEventOf(t, a, "merge.urgent"); !strings.Contains(d, "o/r#7 runs under the floor (budget 40, floor 100, its own bound 20)") ||
		!strings.Contains(d, `asked by "supervisor": a privacy fix`) {
		t.Errorf("urgent event %q", d)
	}
	if d := lastEventOf(t, a, "merge.urgent.spent"); d != "o/r#7 drew 0 of its own bound 20 under the floor" {
		t.Errorf("spent event %q", d)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if l := urgentLines(a, st); len(l) != 1 || !strings.HasPrefix(l[0], `urgent: o/r#7 ran under the floor at budget 40, asked by "supervisor": a privacy fix; drew 0 of its own bound 20`) {
		t.Errorf("budget lines %q", l)
	}
}

// A second urgent merge in the same reset window is refused with the
// reason: its mark, and a mark that raced to its gate, which then waits for
// the reset like any other merge. The next window takes a mark again.
func TestASecondUrgentMergeInTheWindowIsRefused(t *testing.T) {
	_, launched := stubSelf(t)
	stubGitHub(t, github.Open, "")
	a := urgentApp(t)
	window := time.Now().Add(30 * time.Minute).UTC()
	spent := 12
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Urgent = []state.Urgent{
			{Repo: scratchRepo, PR: 6, By: state.Party{Name: supervisorName}, Reason: "a privacy fix", At: time.Now(), Window: window, Remaining: 40, Spent: &spent},
			// A mark that raced past lanes urgent's check.
			{Repo: scratchRepo, PR: 7, By: state.Party{Name: supervisorName}, Reason: "another fix", At: time.Now()},
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	_, err := markUrgent(t, a, "8")
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "o/r#6 ran under the floor in this reset window, asked by \"supervisor\"") {
		t.Errorf("a second mark: exit %d (%v), want refused with the window's urgent merge", Code(err), err)
	}
	if d := lastEventOf(t, a, "merge.urgent.refused"); !strings.HasPrefix(d, "o/r#8: o/r#6 ran under the floor") {
		t.Errorf("refused mark event %q", d)
	}

	if err := a.gate(context.Background(), mergeArgv(scratchRepo), time.Minute, false); Code(err) == 0 {
		t.Fatal("the raced urgent merge ran under the floor a second time in the window")
	}
	if spec := launched(); !slices.Contains(spec.Argv, "--queued") {
		t.Errorf("not queued for the reset: %q", spec.Argv)
	}
	if d := lastEventOf(t, a, "merge.urgent.refused"); !strings.HasPrefix(d, "o/r#7: o/r#6 ran under the floor") || !strings.HasSuffix(d, "it waits for the reset") {
		t.Errorf("refused gate event %q", d)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Urgent) != 1 || st.Urgent[0].PR != 6 {
		t.Errorf("urgent %+v, want only the window's run", st.Urgent)
	}

	a.now = window.Add(time.Second)
	if out, err := markUrgent(t, a, "8"); err != nil || !strings.Contains(out, "marked o/r#8 urgent") {
		t.Errorf("a mark after the reset: %v\n%s", err, out)
	}
}

// An urgent merge keeps its own bound: under it, the merge waits for the
// reset and says why.
func TestAnUrgentMergeWaitsUnderItsOwnBound(t *testing.T) {
	a := urgentApp(t)
	setBudget(t, a, 10)
	if _, err := markUrgent(t, a, "7"); err != nil {
		t.Fatal(err)
	}
	g := &gateRun{app: a, ctx: context.Background(), argv: mergeArgv(scratchRepo), repo: scratchRepo, pr: 7, lane: a.cfg.LaneOf(scratchRepo),
		me: state.Party{Session: "s1", Name: ownerName}, pid: os.Getpid(), queued: true}
	if why, err := g.step(); err != nil || !strings.Contains(why, `o/r#7 is urgent (asked by "supervisor"), and the GitHub budget 10 is under its own bound 20`) {
		t.Errorf("step: %q, %v; want a wait under the urgent bound", why, err)
	}
}
