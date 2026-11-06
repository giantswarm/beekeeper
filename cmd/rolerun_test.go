package cmd

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// personAsker is the person who asks for a guide run from a terminal.
const personAsker = "the person"

// rebootedGuide is a guide whose CLI its own watch last saw running, at
// pid 77, before a reboot ended it: no record says since when it is gone.
func rebootedGuide() *state.Role {
	guide := state.Party{Session: sessionG7, HostSession: hostG7, Name: guideSeven}
	return &state.Role{Holder: &state.Supervisor{Party: guide, Since: relayNow.Add(-time.Hour)}, Run: 7,
		CLI: &state.CLI{Supervisor: guide, Since: relayNow.Add(-time.Hour), PID: 77}}
}

// A guide whose own watch went with it (a reboot, a crash) is recorded
// gone by the standby watch at its first poll, so its grace runs out and
// its successor starts, instead of a grace that starts anew on every poll.
func TestStandbyRecordsAGuideGoneWithItsWatch(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), true)
	var started atomic.Int32
	w.stand = standbyWatch{
		turning: func(context.Context, string) bool { return false },
		succeed: func(context.Context, role, state.Party) (state.Party, error) {
			started.Add(1)
			return state.Party{Session: "sG8", Name: guideEight}, nil
		},
	}
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) { st.Guide = rebootedGuide(); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Duration{0, 30 * time.Second, 2 * time.Minute} {
		w.now = relayNow.Add(at)
		w.pending(context.Background(), nil)
		w.stand.inflight.Wait()
	}
	if n := started.Load(); n != 1 {
		t.Fatalf("%d successors, want 1 once the grace from the first poll ran out:\n%s", n, out)
	}
	st, _ := w.store.Read()
	if g := st.Guide.CLI.Gone; !g.Equal(relayNow) {
		t.Errorf("gone recorded at %v, want the first poll %v", g, relayNow)
	}
	if !strings.Contains(out.String(), `GUIDE GONE: "Guide run 7"`) {
		t.Errorf("watch lines:\n%s", out)
	}
}

// guide status says the guide is gone once its grace ran out, counted from
// its first read: never a restart whose grace moves with every read.
func TestGuideStatusSaysAGoneGuideIsGone(t *testing.T) {
	a, out := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	a.as = personAsker
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) { st.Guide = rebootedGuide(); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	a.now = relayNow
	if err := a.roleStatus(guideRole); err != nil || !strings.Contains(out.String(), "runs no CLI since") {
		t.Fatalf("within the grace: %v\n%s", err, out)
	}
	a.now = relayNow.Add(2 * time.Minute)
	err := a.roleStatus(guideRole)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), `no guide runs: "Guide run 7"`) || !strings.Contains(err.Error(), "guide start --new") {
		t.Fatalf("past the grace: %v", err)
	}
}

// Only a run beekeeper started takes the guide's role: a session started by
// other means is refused, --take-over too; a guide run is not.
func TestOnlyABeekeeperRunTakesTheGuide(t *testing.T) {
	a, _ := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	a.as = ""
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide = rebootedGuide()
		st.Guide.CLI.Gone = relayNow.Add(-time.Hour)
		st.Starts = append(st.Starts,
			state.Start{Party: state.Party{Session: "s5", Name: agentFive}, Mode: state.ModeBypass},
			state.Start{Party: state.Party{Session: "s6", Name: guideEight}, Mode: state.ModeBypass, Role: guideName})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	a.now = relayNow
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
	for _, s := range []struct{ id, name string }{{"s4", guideEight}, {"s5", agentFive}} {
		t.Setenv("CLAUDE_CODE_SESSION_ID", s.id)
		t.Setenv("CLAUDE_CODE_SESSION_NAME", s.name)
		for _, takeOver := range []bool{false, true} {
			if err := a.runStart(context.Background(), guideRole, takeOver); Code(err) != ExitRefused || !strings.Contains(err.Error(), "no guide run beekeeper started") {
				t.Fatalf("%s took the role (take-over %v): %v", s.name, takeOver, err)
			}
		}
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s6")
	t.Setenv("CLAUDE_CODE_SESSION_NAME", guideEight)
	if err := a.runStart(context.Background(), guideRole, false); err != nil {
		t.Fatalf("the guide run beekeeper started: %v", err)
	}
	st, _ := a.store.Read()
	if h := st.Guide.Holder; h == nil || h.Session != "s6" || st.Guide.Run != 8 {
		t.Errorf("holder %+v, run %d", h, st.Guide.Run)
	}
}

// guide start --new starts the next run as a background session with
// Remote Control when none is recorded; its start records the role, so its
// own guide start takes it.
func TestNewGuideRunWithoutAGuide(t *testing.T) {
	a, out := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	a.now = relayNow
	a.as = personAsker
	a.cfg.Agents.Mode = config.ModeBackground
	a.cfg.Guide.Dir = t.TempDir()
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) { st.Guide = &state.Role{Run: 11}; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	calls := stubBackground(t, "backgrounded · "+bgShortID+" · Guide run 12\n", bgSession{ID: bgShortID, SessionID: bgID})
	if err := a.runNewRun(t.Context(), guideRole, false); err != nil {
		t.Fatal(err)
	}
	if l := out.String(); !strings.Contains(l, `started "Guide run 12" (background session `+bgID) || !strings.Contains(l, "claude attach "+bgShortID) {
		t.Errorf("output:\n%s", l)
	}
	if len(*calls) != 1 || (*calls)[0].dir != a.cfg.Guide.Dir {
		t.Fatalf("calls %+v", *calls)
	}
	if brief := (*calls)[0].argv[len((*calls)[0].argv)-1]; !strings.Contains(brief, "which no session holds") || !strings.Contains(brief, "`beekeeper guide start`") {
		t.Errorf("brief:\n%s", brief)
	}
	st, _ := a.store.Read()
	if st.Guide.Relay == nil || st.Guide.Relay.To.Session != bgID || !guideRole.startedAs(st, state.Party{Session: bgID}) {
		t.Fatalf("relay %+v, starts %+v", st.Guide.Relay, st.Starts)
	}
	a.as = ""
	t.Setenv("CLAUDE_CODE_SESSION_ID", bgID)
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_NAME", "Guide run 12")
	if err := a.runStart(context.Background(), guideRole, false); err != nil {
		t.Fatal(err)
	}
	if st, _ := a.store.Read(); st.Guide.Holder == nil || st.Guide.Holder.Session != bgID || st.Guide.Run != 12 || st.Guide.Relay != nil {
		t.Errorf("guide %+v", st.Guide)
	}
}

// guide start --new leaves a guide whose CLI runs alone without
// --take-over, and a run that is starting already.
func TestNewGuideRunRefusals(t *testing.T) {
	a, _ := stubApp(t)
	a.now = relayNow
	a.as = personAsker
	runLive(t, a, map[int]string{9: guideNine})
	live := state.Party{Session: "s9", Name: guideNine}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide = &state.Role{Holder: &state.Supervisor{Party: live, Since: relayNow.Add(-time.Hour)}, Run: 9}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.runNewRun(t.Context(), guideRole, false); Code(err) != ExitRefused || !strings.Contains(err.Error(), "its CLI runs") {
		t.Fatalf("a live guide: %v", err)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide.Holder.Party = state.Party{Session: holderGone, Name: guideNine}
		st.Guide.Relay = &state.Relay{From: st.Guide.Holder.Party, To: state.Party{Name: "Guide run 10"}, At: relayNow, Expires: relayNow.Add(15 * time.Minute)}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.runNewRun(t.Context(), guideRole, false); Code(err) != ExitRefused || !strings.Contains(err.Error(), `"Guide run 10" is starting already`) {
		t.Fatalf("a starting run: %v", err)
	}
}

// beekeeper resume starts the guide whose session runs no CLI after the
// stop and a reboot; none when no guide is recorded.
func TestResumeStartsAGoneGuide(t *testing.T) {
	a, out := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	a.now = relayNow
	a.as = personAsker
	var runs []state.Party
	orig := startRunAfterResume
	t.Cleanup(func() { startRunAfterResume = orig })
	startRunAfterResume = func(_ context.Context, _ *app, rl role, by state.Party) (state.Party, string, error) {
		runs = append(runs, by)
		return state.Party{Session: bgID, Name: rl.runName(8)}, "", nil
	}
	stop := &state.Stop{Reason: "reboot", By: state.Party{Name: personAsker}, At: relayNow.Add(-time.Hour)}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide, st.Stop = rebootedGuide(), stop
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Name != personAsker || !strings.Contains(out.String(), `resume: "Guide run 7" runs no CLI: started "Guide run 8"`) {
		t.Fatalf("runs %+v:\n%s", runs, out)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide, st.Stop = &state.Role{Run: 8}, stop
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.resume(t.Context()); err != nil || len(runs) != 1 {
		t.Fatalf("no guide recorded: %v, runs %+v", err, runs)
	}
}

// The first background launch reserves its run before the daemon returns.
func TestInitialGuideRunReservesLaunch(t *testing.T) {
	for _, run := range []int{0, 11} {
		t.Run(guideRole.runName(run+1), func(t *testing.T) {
			a, _ := stubApp(t)
			plat.Machine = tableMachine{plat.Machine}
			a.now = relayNow
			a.cfg.Agents.Mode = config.ModeBackground
			a.cfg.Guide.Dir = t.TempDir()
			a.cfg.Guide.RelayTTL.Duration = 15 * time.Minute
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Guide = nil
				if run > 0 {
					st.Guide = &state.Role{Run: run}
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			calls := stubBackground(t, "backgrounded · "+bgShortID+" · guide\n", bgSession{ID: bgShortID, SessionID: bgID})
			launch := bgRun
			nested := false
			bgRun = func(ctx context.Context, dir string, env, argv []string) (string, error) {
				if !nested {
					nested = true
					_, _, err := a.newRun(ctx, guideRole, state.Party{Name: personAsker}, true)
					if Code(err) != ExitRefused || !strings.Contains(err.Error(), "is starting already") {
						t.Errorf("second launch: %v", err)
					}
					waitCtx, cancel := context.WithCancel(ctx)
					waiting := make(chan error, 1)
					go func() {
						waiting <- a.awaitRoleStart(waitCtx, guideRole, state.Party{Session: bgID, Name: guideRole.runName(run + 1)})
					}()
					select {
					case err := <-waiting:
						t.Errorf("role start returned before launch was recorded: %v", err)
					case <-time.After(50 * time.Millisecond):
					}
					cancel()
					select {
					case <-waiting: //nolint:errcheck // only assert that the cancelled start finishes
					case <-time.After(time.Second):
						t.Error("role start did not honor cancellation")
					}
					st, err := a.store.Read()
					if err != nil {
						t.Fatal(err)
					}
					if r := guideRole.get(st).Relay; !r.Open(a.now) || r.To.Session != "" || r.To.Name != guideRole.runName(run+1) {
						t.Errorf("pending launch: %+v", r)
					}
				}
				return launch(ctx, dir, env, argv)
			}
			to, _, err := a.newRun(t.Context(), guideRole, state.Party{Name: personAsker}, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(*calls) != 1 {
				t.Errorf("daemon launches = %d, want 1", len(*calls))
			}
			st, _ := a.store.Read()
			if !guideRole.startedAs(st, to) {
				t.Fatal("start not recorded")
			}
		})
	}
}

func TestInitialGuideRunLaunchFailureWithdrawsReservation(t *testing.T) {
	a, _ := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	a.now = relayNow
	a.cfg.Agents.Mode = config.ModeBackground
	a.cfg.Guide.Dir = t.TempDir()
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) { st.Guide = nil; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	calls := stubBackground(t, "backgrounded · "+bgShortID+" · guide\n", bgSession{ID: bgShortID, SessionID: bgID})
	launch := bgRun
	bgRun = func(context.Context, string, []string, []string) (string, error) {
		return "", errors.New("launch failed")
	}
	if _, _, err := a.newRun(t.Context(), guideRole, state.Party{Name: personAsker}, false); err == nil {
		t.Fatal("launch succeeded")
	}
	st, _ := a.store.Read()
	if st.Guide.Relay != nil || st.Guide.Holder != nil {
		t.Fatalf("failed launch left reservation: %+v", st.Guide)
	}
	bgRun = launch
	to, _, err := a.newRun(t.Context(), guideRole, state.Party{Name: personAsker}, false)
	if err != nil {
		t.Fatal(err)
	}
	if to.Name != guideRole.runName(2) || len(*calls) != 1 {
		t.Fatalf("retry: %+v, launches %d", to, len(*calls))
	}
}
