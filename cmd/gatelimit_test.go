//go:build unix

package cmd

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A gate whose devctl runs past the call's tool limit leaves at the limit
// with exit 76: devctl runs on, the merge keeps running in its lane, no heard
// marker is left (so its merge-child wakes the owner), and the watch records
// the run from its files once devctl ended.
func TestAGateAtItsToolLimitLeavesTheMergeRunning(t *testing.T) {
	g, base, pid, stderr := launchedMerge(t, `echo merging >&2; sleep 2; echo '`+mergedDoc+`'`)
	asked := stubGitHub(t, github.Open, "")
	g.leaveAt = time.Now().Add(300 * time.Millisecond)
	stdout := capture(t, &os.Stdout)
	start := time.Now()
	err := g.follow(base, pid, 0)
	took, doc, said := time.Since(start), stdout(), stderr()
	if Code(err) != ExitGateQueued {
		t.Fatalf("exit %d (%v), want %d\n%s", Code(err), err, ExitGateQueued, said)
	}
	if took >= 2*time.Second {
		t.Errorf("the gate left after %s, at devctl's end rather than its limit", took)
	}
	if doc != "" {
		t.Errorf("stdout %q, want nothing: the document is the wake's", doc)
	}
	if !strings.Contains(said, "o/r#7's devctl runs past this call's limit") || !strings.Contains(said, g.outcomeTo()) ||
		!strings.Contains(said, "do not run it again, do not poll") {
		t.Errorf("stderr:\n%s", said)
	}
	if _, err := os.Stat(heardFile(base, os.Getpid())); err == nil {
		t.Error("the heard marker is left: the owner would not be woken")
	}
	st, _ := g.store.Read()
	if i := slices.IndexFunc(st.Merges, func(m state.Merge) bool { return m.PR == 7 && m.Phase == state.Running && m.Child == pid }); i < 0 {
		t.Fatalf("the merge does not run on in its lane: %+v", st.Merges)
	}
	if d := lastEvent(t, g, "merge.left"); !strings.Contains(d, "o/r#7: devctl runs on outside the call") {
		t.Errorf("left event %q", d)
	}

	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(base + ".rc"); err == nil {
			break
		}
	}
	// The gate's process is gone, as the watch sees it once the call ended.
	if err := g.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges[0].PID = deadPID(t)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	w := &app{cfg: g.cfg, store: g.store, now: time.Now()}
	recorded, lost := w.recordGone(context.Background())
	if len(recorded) != 1 || !strings.Contains(recorded[0], "o/r#7 exit 0, release v1.2.4") || len(lost) != 0 {
		t.Fatalf("recorded %q, lost %+v", recorded, lost)
	}
	if *asked != 0 {
		t.Errorf("GitHub asked %d times about a run whose document is in its files", *asked)
	}
}

// A merge waiting for its turn leaves at the tool limit too, before its
// --wait ends: it is queued in a run of its own (exit 76).
func TestAWaitingMergeLeavesAtItsToolLimit(t *testing.T) {
	stubGitHub(t, github.Open, "")
	_, launched := stubSelf(t)
	a := busyLaneApp(t)
	start := time.Now()
	err := a.gate(context.Background(), mergeArgv(scratchRepo), time.Hour, 200*time.Millisecond, false)
	if Code(err) != ExitGateQueued {
		t.Fatalf("exit %d (%v), want %d", Code(err), err, ExitGateQueued)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("the gate left after %s, not at its limit", took)
	}
	if spec := launched(); !slices.Contains(spec.Argv, "--queued") {
		t.Errorf("not queued: %q", spec.Argv)
	}
}

// leaveAt: now plus the limit; none for a queued merge's own run or without
// a limit; a re-executed call keeps its own, which devctl never inherits.
func TestLeaveAt(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	if got := leaveAt(now, 9*time.Minute, false); !got.Equal(now.Add(9 * time.Minute)) {
		t.Errorf("limit 9m: %s", got)
	}
	for _, c := range []struct {
		limit  time.Duration
		queued bool
	}{{0, false}, {-time.Minute, false}, {9 * time.Minute, true}} {
		if got := leaveAt(now, c.limit, c.queued); !got.IsZero() {
			t.Errorf("limit %s, queued %v: %s, want none", c.limit, c.queued, got)
		}
	}
	carried := now.Add(-time.Minute)
	t.Setenv(gateLimitEnv, carried.Format(time.RFC3339Nano))
	if got := leaveAt(now, 9*time.Minute, false); !got.Equal(carried) {
		t.Errorf("re-executed: %s, want the call's own %s", got, carried)
	}
	if _, ok := os.LookupEnv(gateLimitEnv); ok {
		t.Errorf("%s is left for devctl", gateLimitEnv)
	}
}
