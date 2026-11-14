package cmd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// This harness blocks the normal poll before pending/recovery can run.
// The independent guide loop must neither discover this harness nor wait on it.
type blockedRecoveryHarness struct {
	Harness
	entered, release chan struct{}
	once             sync.Once
}

func (h *blockedRecoveryHarness) Discover(*config.Config, *proc.Table, time.Time) []*claude.Session {
	h.once.Do(func() { close(h.entered) })
	<-h.release
	return nil
}

// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestGuideRecoversWhileMainWatchIsBlocked(t *testing.T) {
	needsPlatform(t)
	w, out := loopWatcher(t, "30s", "", nil)
	clk := newFakeClock(t)
	w.Clock = clk
	watchApp(w.Host).now = clk.Now()
	guide := state.Party{Session: bgID, Name: guideSeven}
	seed(t, watchApp(w.Host), func(st *state.State) {
		st.Guide = &state.Role{
			Holder: &state.Supervisor{Party: guide, Since: watchApp(w.Host).now.Add(-time.Hour)},
			CLI:    &state.CLI{Supervisor: guide, Since: watchApp(w.Host).now.Add(-time.Hour), PID: 77},
			Run:    7,
		}
		st.Starts = []state.Start{{Party: guide, Role: guideName, Dir: t.TempDir(), Background: true}}
	})
	t.Setenv("CLAUDE_CODE_SESSION_ID", bgID)
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "local_parent")
	t.Setenv("CLAUDE_CODE_SESSION_NAME", "parent title")
	calls := stubBackground(t, "backgrounded · "+bgShortID+" · "+guideSeven+"\n")
	oldMachine := plat.Machine
	plat.Machine = daemonSnapshotMachine{Machine: oldMachine, snapshot: func() (*proc.Table, error) { return proc.NewTable(), nil }}
	t.Cleanup(func() { plat.Machine = oldMachine })
	h := &blockedRecoveryHarness{entered: make(chan struct{}), release: make(chan struct{})}
	oldHarnesses := harnesses
	harnesses = []Harness{h}
	t.Cleanup(func() { harnesses = oldHarnesses })
	runWatch(t, w)
	t.Cleanup(func() { close(h.release) })
	select {
	case <-h.entered:
	case <-time.After(time.Minute):
		t.Fatal("normal watch did not reach the blocking discovery")
	}
	// Main poll has no timer: machine sampling, upgrades and recovery do.
	clk.settle(loopTimers - 1)
	for range 2 {
		clk.advance(30 * time.Second)
		clk.settle(loopTimers - 1)
	}
	if !eventually(2*time.Second, func() bool { return strings.Contains(out.String(), "GUIDE RESUME:") }) {
		t.Fatalf("guide not resumed within a minute while main poll blocked:\n%s", out.String())
	}
	if w.Polls.Load() != 1 {
		t.Fatalf("main poll was not blocked: %d polls", w.Polls.Load())
	}
	if len(*calls) != 1 || (*calls)[0].argv[1] != bgFlag || (*calls)[0].argv[3] != bgID {
		t.Fatalf("did not resume the same background run: %+v", *calls)
	}
	for _, kv := range (*calls)[0].env {
		if isHarnessEnv(kv) {
			t.Fatal("resume inherited the caller's identity")
		}
	}
}

// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestGuideRecoveryHonorsStopAndUnknownProcesses(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), true)
	seed(t, watchApp(w.Host), func(st *state.State) {
		st.Guide = rebootedGuide()
		st.Guide.CLI.Gone = relayNow.Add(-time.Hour)
	})
	g := w.NewGuideRecovery()
	g.Clock = newFakeClock(t)
	g.Stand.Succeed = func(context.Context, role, state.Party) (state.Party, error) {
		t.Error("started on a stop or unknown process observation")
		return state.Party{}, nil
	}
	old := plat.Machine
	t.Cleanup(func() { plat.Machine = old })
	plat.Machine = daemonSnapshotMachine{Machine: old, snapshot: func() (*proc.Table, error) { return nil, errors.New("unreadable process snapshot") }}
	g.RecoverGuide(t.Context())
	if !strings.Contains(out.String(), "unreadable process snapshot") {
		t.Fatalf("missing failure diagnostic: %s", out.String())
	}
	seed(t, watchApp(w.Host), func(st *state.State) { st.Stop = &state.Stop{} })
	plat.Machine = daemonSnapshotMachine{Machine: old, snapshot: func() (*proc.Table, error) { return proc.NewTable(), nil }}
	g.RecoverGuide(t.Context())
	g.Stand.Inflight.Wait()
}
