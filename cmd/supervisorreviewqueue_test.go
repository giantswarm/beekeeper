package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	queueReviewer   = "Review r 7 r1"
	queueGuide      = "guide-session"
	queueVerdict    = "Desk review (opus): changes requested\nhttps://example.test/review"
	queueNoSuchName = "no registered agent matches"
)

// A pile of ownerless pull requests costs a pass a bounded number of reads,
// least recently read first, so reviews still start; every one gets its
// turn, its stretch runs on while it waits, and the pile is counted.
func TestSweepConsumesAllAvailableObservationsAndCountsThePile(t *testing.T) {
	r := newLoopRig(t)
	total := loop.SweepReadsPerPass + 10
	var open []string
	for n := range total {
		ref := sweepRef(100 + n)
		open = append(open, ref)
		r.pulls[ref] = superloop.Pull{Ref: ref, URL: fmt.Sprintf("https://github.com/o/r/pull/%d", 100+n), State: superloop.Open, Head: loopHead, Green: true, Author: loopDesk, Branch: "bulk"}
	}
	listed := loop.ReadOpenPulls
	t.Cleanup(func() { loop.ReadOpenPulls = listed })
	loop.ReadOpenPulls = func(context.Context, func() ([]string, error), []string) ([]string, error) { return open, nil }
	var reads atomic.Int32
	read := loop.ReadLoopPull
	t.Cleanup(func() { loop.ReadLoopPull = read })
	loop.ReadLoopPull = func(ctx context.Context, ref string) (superloop.Pull, error) {
		if slices.Contains(open, ref) {
			reads.Add(1)
		}
		return read(ctx, ref)
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	settled := func() {
		l.PassActions.Wait()
		l.MergeActions.Wait()
		l.TimerActs.Wait()
	}
	r.loopPass(t, l)
	settled()
	if n := reads.Load(); n != int32(total) {
		t.Fatalf("first pass read %d, want %d", n, total)
	}
	var count loop.LoopSweepCount
	if found, err := r.a.store.ReadFile(loop.LoopSweepFile, &count); err != nil || !found {
		t.Fatalf("no count: %v", err)
	}
	if count.Listed != total || count.Ownerless != total || count.Unread != 0 || count.Oldest.IsZero() {
		t.Fatalf("count = %+v", count)
	}
	line := count.Line(r.a)
	for _, want := range []string{fmt.Sprintf("%d listed", total), fmt.Sprintf("%d without a live owner", total)} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q lacks %q", line, want)
		}
	}
	if !strings.Contains(r.out.String(), "SUPERVISOR SWEEP") {
		t.Errorf("the pile went unsaid:\n%s", r.out)
	}
	// The next pass reuses all observations.
	reads.Store(0)
	r.loopPass(t, l)
	settled()
	if n := reads.Load(); n != 0 {
		t.Fatalf("second pass read %d, want none", n)
	}
	count = loop.LoopSweepCount{}
	if _, err := r.a.store.ReadFile(loop.LoopSweepFile, &count); err != nil {
		t.Fatal(err)
	}
	if count.Ownerless != total || count.Unread != 0 {
		t.Fatalf("count = %+v", count)
	}
}

func TestSweepCountSaysAListAtGitHubsLimit(t *testing.T) {
	r := newLoopRig(t)
	if got := (loop.LoopSweepCount{}).Line(r.a); got != "" {
		t.Fatalf("nothing to say, said %q", got)
	}
	if got := (loop.LoopSweepCount{Listed: loop.SweepListLimit, AtLimit: true}).Line(r.a); !strings.Contains(got, "there may be more") {
		t.Fatalf("line %q", got)
	}
}

// A disk just under the floor stops a worker and lets a review through;
// well under it, a review waits too, and says the floor that held it.
func TestReviewStartHasALowerDiskFloor(t *testing.T) {
	k := config.Capacity{Ceiling: 2, ReviewHeadroom: 2}
	disk := func(free int) *capacity.Headroom {
		h := roomy()
		h.Disk = &watchDiskReading{Low: true, FreeMiB: free, MinMiB: 102400}
		return h
	}
	if v := capacity.NewStartCapacity(capacity.AgentCount{}, k, disk(98783), false, capacity.ForWorker); v.Room != 0 || !strings.Contains(v.Verdict(), "floor 102400 MiB") {
		t.Fatalf("worker: %s", v.Verdict())
	}
	if v := capacity.NewStartCapacity(capacity.AgentCount{}, k, disk(98783), false, capacity.ForReview); v.Room == 0 {
		t.Fatalf("review held just under the floor: %s", v.Verdict())
	}
	if v := capacity.NewStartCapacity(capacity.AgentCount{}, k, disk(1024), false, capacity.ForReview); v.Room != 0 || !strings.Contains(v.Verdict(), "floor 51200 MiB") {
		t.Fatalf("review on a nearly full disk: %s", v.Verdict())
	}
	h := disk(98783)
	if h.ForStart(capacity.ForReview); !h.Disk.Low {
		t.Fatal("a review's floor changed the shared reading")
	}
}

// supervisor.graphQLLimit moves the board's hold, and the hold is said.
func TestGraphQLLimitIsConfigurableAndItsHoldVisible(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.GitHub.Floor = 100
	now := r.a.timeNow()
	spend := loopGraphQLSpend{Reset: now.Add(time.Hour), Spent: 1535, Remaining: 4000, Steps: map[string]int{loop.LoopPullStep: 1535}}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		g := &state.GraphQL{Remaining: 3000, Limit: 5000, Reset: spend.Reset, At: now}
		st.Budget = &state.Budget{Remaining: 4000, Limit: 5000, Reset: spend.Reset, At: now, GraphQL: g}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := r.a.loopGraphQLCap(); got != loopGraphQLLimit {
		t.Fatalf("default cap %d", got)
	}
	line := r.a.heldLine(spend)
	if !strings.Contains(line, "board reads wait") || !strings.Contains(line, "reviews and merges go on") {
		t.Fatalf("held line %q", line)
	}
	r.a.cfg.Supervisor.GraphQLLimit = 4000
	if line := r.a.heldLine(spend); line != "" {
		t.Fatalf("a raised cap still holds: %q", line)
	}
	if !strings.Contains(loopSpendLine(r.a, spend), "1535 of 4000") {
		t.Fatal(loopSpendLine(r.a, spend))
	}
}

// removeRig has a loop whose agents remove answers code with out, the
// agent gone from the roster or not.
func removeRig(t *testing.T, onRoster bool, code int, out string) (*loopRig, *loop.Loop) {
	t.Helper()
	r := newLoopRig(t)
	if onRoster {
		if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
			st.Agents = append(st.Agents, state.Agent{Party: state.Party{Name: "worker-x"}, Task: "queue work"})
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	r.answer = func(args []string) (string, int) {
		if len(args) > 1 && args[1] == loop.LoopRemove {
			return out, code
		}
		return "", 0
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	return r, l
}

// An agent the doctor took off first is removed: the step goes on, where it
// used to fail on every retry.
func TestLoopRemoveOfAnAgentOffTheRosterIsDone(t *testing.T) {
	r, l := removeRig(t, false, ExitRefused, queueNoSuchName+` "worker-x"`)
	if err := l.Remove(context.Background(), "worker-x", "task done"); err != nil {
		t.Fatal(err)
	}
	if c := r.called(agentsName, loop.LoopRemove); !slices.Contains(c, "--"+reasonFlagName) || !slices.Contains(c, "task done") {
		t.Fatalf("no reason passed: %v", c)
	}
}

// A refusal of an agent still on the roster is a failure: an SSH execution
// must confirm termination first.
func TestLoopRemoveRefusedForAnAgentOnTheRosterFails(t *testing.T) {
	_, l := removeRig(t, true, ExitRefused, "SSH execution must have confirmed termination before removal")
	if err := l.Remove(context.Background(), "worker-x", "task done"); err == nil {
		t.Fatal("a refused removal counted as done")
	}
}

func TestRemovedDetailKeepsTheNameReadable(t *testing.T) {
	if got := removedDetail("w", "  why\n"); got != "w: why" {
		t.Fatalf("detail %q", got)
	}
	if removedDetail("w", "") != "w" {
		t.Fatal("no reason, no suffix")
	}
	for detail, want := range map[string]bool{"w": true, "w: why": true, "w2: why": false, "w2": false} {
		if loop.RemovedAgent(detail, "w") != want {
			t.Errorf("removedAgent(%q) != %v", detail, want)
		}
	}
}

// The verdict reaches who asked for the review, whether through agents
// review or agents start --review; the loop reads its own reviews itself.
func TestReviewStarterGetsTheVerdict(t *testing.T) {
	guide := state.Party{Session: queueGuide, Name: "guide"}
	reviewer := state.Party{Session: "queue-reviewer-session", Name: queueReviewer}
	ag := state.Agent{Party: reviewer}
	for _, tc := range []struct {
		name   string
		st     state.State
		report string
		want   state.Party
	}{
		{"by agents review", state.State{ReviewRequests: []state.ReviewRequest{{Ref: claimIssue, Head: loopHead, Round: 1, By: guide}}, Starts: []state.Start{{Party: reviewer, By: loop.LoopParty}}}, queueVerdict, guide},
		{"by agents start", state.State{Starts: []state.Start{{Party: reviewer, By: guide}}}, queueVerdict, guide},
		{"by the loop", state.State{ReviewRequests: []state.ReviewRequest{{Ref: claimIssue, Head: loopHead, Round: 1}}, Starts: []state.Start{{Party: reviewer, By: loop.LoopParty}}}, queueVerdict, state.Party{}},
		{"no verdict", state.State{Starts: []state.Start{{Party: reviewer, By: guide}}}, "shipped it", state.Party{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reviewStarter(&tc.st, ag, tc.report); got != tc.want {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

// A commented verdict is readable, as the loop reads it, on the head it
// judged; one from a stranger is not, and no verdict is exit 3.
func TestReviewVerdictsPrintACommentedVerdict(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Supervisor.Ownership.Authors = []string{loopDesk}
	p := superloop.Pull{Ref: loopPR, Head: loopFixed, Reviews: []superloop.Review{
		{Author: loopDesk, Commit: loopHead, Verdict: superloop.Verdict("COMMENTED", queueVerdict), Body: queueVerdict, At: r.a.now},
		{Author: "queue-stranger", Commit: loopFixed, Verdict: superloop.Approved, At: r.a.now.Add(time.Minute)},
	}}
	if err := r.a.printReviewVerdicts(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	out := r.out.String()
	if !strings.Contains(out, superloop.ChangesRequested) || !strings.Contains(out, "head is now") || strings.Contains(out, "queue-stranger") {
		t.Fatalf("verdicts:\n%s", out)
	}
	p.Reviews = p.Reviews[1:]
	if err := r.a.printReviewVerdicts(context.Background(), p); Code(err) != ExitRefused {
		t.Fatalf("no verdict: %v", err)
	}
}

// A wake that cannot reach the starter is said, not dropped.
func TestWatchSaysAVerdictItCouldNotDeliver(t *testing.T) {
	r := newLoopRig(t)
	w := &watcher{a: r.a, last: map[string]time.Time{}, reported: map[string]bool{}}
	w.tellReviewVerdicts(context.Background(), []state.WorkerReport{{By: state.Party{Name: queueReviewer}, Text: queueVerdict, TellTo: state.Party{Session: holderGone, Name: "gone-guide"}}})
	if !strings.Contains(r.out.String(), "REVIEW VERDICT NOT DELIVERED") {
		t.Fatalf("output:\n%s", r.out)
	}
}
