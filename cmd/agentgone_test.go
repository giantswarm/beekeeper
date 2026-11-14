package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/watch"
)

func TestGoneAgentBoundaryAndProtections(t *testing.T) {
	now := keepNow
	old := now.Add(-24 * time.Hour)
	for _, tt := range []struct {
		name   string
		change func(*state.Agent, *state.State)
		live   bool
		gone   bool
	}{
		{name: "boundary", gone: true},
		{name: "between turns", change: func(ag *state.Agent, _ *state.State) { ag.ActivityAt = old.Add(time.Nanosecond) }},
		{name: "new task", change: func(ag *state.Agent, _ *state.State) { ag.AssignedAt = now }},
		{name: "keep marker", change: func(ag *state.Agent, _ *state.State) { ag.Keep = &state.Keep{} }},
		{name: "park marker", change: func(ag *state.Agent, _ *state.State) { ag.Park = &state.Park{Waits: reviewFlag} }},
		{name: "timer", change: func(ag *state.Agent, st *state.State) { st.Timers = []state.Timer{{Wake: ag.Name}} }},
		{name: "running headless turn", live: true},
		{name: "recent serve", change: func(ag *state.Agent, st *state.State) { st.Records = []state.Record{{Session: ag.Party, At: now}} }},
		{name: "unknown age", change: func(ag *state.Agent, _ *state.State) { ag.Registered = time.Time{} }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ag := tasked("gone-test")
			ag.Registered = old
			st := &state.State{}
			if tt.change != nil {
				tt.change(&ag, st)
			}
			st.Agents = []state.Agent{ag}
			var sessions []*claude.Session
			if tt.live {
				sessions = []*claude.Session{{ID: ag.Session, LastActive: old}}
			}
			_, gone := goneAgent(nil, st, sessions, ag, 24*time.Hour, now)
			if gone != tt.gone {
				t.Fatalf("gone = %v, want %v", gone, tt.gone)
			}
			c := countAgents(st, sessions, now, nil)
			if (len(c.Gone) == 1) != tt.gone {
				t.Fatalf("capacity = %+v", c)
			}
			pa := countProviderAgents(st, sessions, config.Capacity{}, now, nil)
			for _, p := range pa {
				if tt.gone && len(p.Busy) > 0 {
					t.Fatalf("gone holds provider: %+v", pa)
				}
			}
			chores := planChores(st, sessions, func(string) (*claude.Record, bool) { return nil, false }, func(state.Party) bool { return tt.live }, 24*time.Hour, now, nil)
			if (len(chores) == 1 && chores[0].kind == choreRemove) != tt.gone {
				t.Fatalf("chores = %+v", chores)
			}
			if _, gone := goneAgent(nil, st, sessions, ag, 0, now); gone {
				t.Fatal("disabled staleness retires agent")
			}
		})
	}
}

func TestGoneAgentReadsTranscriptBetweenTurns(t *testing.T) {
	a, _ := stubApp(t)
	ag := tasked("transcript-test")
	ag.Registered = a.now.Add(-48 * time.Hour)
	st := &state.State{Agents: []state.Agent{ag}}
	dir := filepath.Join(a.cfg.Claude.ProjectsDir, "project")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ag.Session+".jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fresh := a.now.Add(-time.Hour)
	if err := os.Chtimes(path, fresh, fresh); err != nil {
		t.Fatal(err)
	}
	if _, gone := goneAgent(a.cfg, st, nil, ag, 24*time.Hour, a.now); gone {
		t.Fatal("recent transcript lost its slot")
	}
	providerBusy := 0
	for _, p := range countProviderAgents(st, nil, config.Capacity{}, a.now, a.cfg) {
		providerBusy += len(p.Busy)
	}
	if len(countAgents(st, nil, a.now, a.cfg).Busy) != 1 || providerBusy != 1 {
		t.Fatal("recent transcript lost capacity")
	}
	old := a.now.Add(-24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, gone := goneAgent(a.cfg, st, nil, ag, 24*time.Hour, a.now); !gone {
		t.Fatal("boundary transcript still holds its slot")
	}
	views := a.agentViews(st, nil)
	if !strings.Contains(views[0].Reachable, strings.TrimSpace(goneActivityPrefix)) {
		t.Fatalf("view = %+v", views)
	}
	encoded, err := json.Marshal(views)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []struct {
		Name      string
		Reachable string
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 1 || decoded[0].Name != ag.Name || !strings.Contains(decoded[0].Reachable, strings.TrimSpace(goneActivityPrefix)) {
		t.Fatalf("JSON hides gone state: %s", encoded)
	}
}

// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestGoneRetirementReleasesBoardAndPreservesHandover(t *testing.T) {
	a, _ := stubApp(t)
	runLive(t, a, nil)
	ag := tasked("retire-test")
	ag.Registered = a.now.Add(-48 * time.Hour)
	const issue = "o/r#21"
	ag.Task = issue + ": unfinished work"
	seed(t, a, func(st *state.State) {
		st.Agents = []state.Agent{ag}
		st.Records = []state.Record{{Session: ag.Party, Issue: issue, At: ag.Registered}}
	})
	if err := a.store.Record(state.Event{At: ag.Registered, By: ag.Party, Verb: verbNote, Detail: "next: finish the regression proof"}); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	scope := pickScope{alive: func(state.Party) bool { return false }, listed: a.now}
	if boardOwners(st, scope)[issue] == "" {
		t.Fatal("precondition: item unowned")
	}
	chores := planChores(st, nil, func(string) (*claude.Record, bool) { return nil, false }, func(state.Party) bool { return false }, 24*time.Hour, a.now, a.cfg)
	removed, err := a.removeAgents(chores, watch.WatchParty)
	if err != nil {
		t.Fatal(err)
	}
	st, _ = a.store.Read()
	if len(removed) != 1 || !removed[0].archive || len(st.Agents) != 0 || st.Records[0].Ended.IsZero() {
		t.Fatalf("removed=%+v state=%+v", removed, st)
	}
	if boardOwners(st, scope)[issue] != "" {
		t.Fatal("retired task still holds board item")
	}
	evs, err := a.store.Events(10, func(e state.Event) bool { return e.Verb == "agents.gone" })
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Detail, ag.Task) || !strings.Contains(evs[0].Detail, "next: finish the regression proof") {
		t.Fatalf("events=%+v err=%v", evs, err)
	}
}

// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestGoneRetirementRechecksActivity(t *testing.T) {
	for _, change := range []string{agentWakeName, "keep", "transcript"} {
		t.Run(change, func(t *testing.T) {
			a, _ := stubApp(t)
			runLive(t, a, nil)
			ag := tasked("recheck-test")
			ag.Registered = a.now.Add(-48 * time.Hour)
			seed(t, a, func(st *state.State) { st.Agents = []state.Agent{ag} })
			st, _ := a.store.Read()
			chores := planChores(st, nil, func(string) (*claude.Record, bool) { return nil, false }, func(state.Party) bool { return false }, 24*time.Hour, a.now, a.cfg)
			if change == "transcript" {
				dir := filepath.Join(a.cfg.Claude.ProjectsDir, "project")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, ag.Session+".jsonl"), []byte("{}\n"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				seed(t, a, func(st *state.State) {
					if change == "keep" {
						st.Agents[0].Keep = &state.Keep{}
					} else {
						st.Agents[0].ActivityAt = a.now
					}
				})
			}
			removed, err := a.removeAgents(chores, watch.WatchParty)
			if err != nil || len(removed) != 0 {
				t.Fatalf("removed=%+v err=%v", removed, err)
			}
			st, _ = a.store.Read()
			if !slices.Equal(countAgents(st, nil, a.now, a.cfg).Gone, []string{}) {
				t.Fatal("activity did not restore capacity")
			}
		})
	}
}

func TestGoneWakeAndResumeRestoreCapacity(t *testing.T) {
	for _, action := range []string{agentWakeName, agentResumeName} {
		t.Run(action, func(t *testing.T) {
			const id = "gone-wake-proof"
			a, launcher, _ := wakeTwinSetup(t, id, false)
			ag := state.Agent{Party: state.Party{Session: id, Name: "test: wake gone"}, Registered: a.now.Add(-48 * time.Hour), Task: "unfinished task"}
			dir := t.TempDir()
			path := filepath.Join(a.cfg.Claude.ProjectsDir, "wake-proof", id+".jsonl")
			writeFile(t, path, `{"type":"user","cwd":"`+filepath.ToSlash(dir)+`"}`+"\n")
			old := a.now.Add(-48 * time.Hour)
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
			seed(t, a, func(st *state.State) { st.Agents = []state.Agent{ag} })
			st, _ := a.store.Read()
			if len(countAgents(st, nil, a.now, a.cfg).Gone) != 1 {
				t.Fatal("precondition: not gone")
			}
			var err error
			if action == agentResumeName {
				err = a.resumeParkedOpts(t.Context(), waker, ag.Name, false)
			} else {
				err = a.wakeAgent(t.Context(), waker, ag.Name, "continue", "")
			}
			if err != nil {
				t.Fatal(err)
			}
			st, _ = a.store.Read()
			if len(launcher.units) != 1 || len(countAgents(st, nil, a.now, a.cfg).Busy) != 1 {
				t.Fatalf("%s did not restore slot: %+v", action, st.Agents)
			}
		})
	}
}

// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestGoneWatchReportsAgeTransition(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	watchApp(w.Host).cfg.Claude.ProjectsDir = t.TempDir()
	watchApp(w.Host).cfg.Claude.DesktopDir = t.TempDir()
	ag := tasked("watch-gone-test")
	ag.Registered = watchApp(w.Host).now.Add(-23 * time.Hour)
	st := &state.State{Agents: []state.Agent{ag}}
	w.StoppedAgents(st, nil)
	if strings.Contains(out.String(), strings.TrimSpace(goneActivityPrefix)) {
		t.Fatal("recent worker shown gone")
	}
	watchApp(w.Host).now = watchApp(w.Host).now.Add(time.Hour)
	w.StoppedAgents(st, nil)
	w.StoppedAgents(st, nil)
	if strings.Count(out.String(), strings.TrimSpace(goneActivityPrefix)) != 1 {
		t.Fatalf("watch = %s", out)
	}
}

func TestGoneConfiguredAge(t *testing.T) {
	ag := tasked("configured-age-test")
	ag.Registered = keepNow.Add(-2 * time.Hour)
	st := &state.State{Agents: []state.Agent{ag}}
	cfg := &config.Config{Agents: config.Agents{StaleAfter: config.Duration{Duration: time.Hour}}}
	if len(countAgents(st, nil, keepNow, cfg).Gone) != 1 {
		t.Fatal("configured age ignored")
	}
	if len(countAgents(st, nil, keepNow, nil).Busy) != 1 {
		t.Fatal("default age ignored")
	}
	cfg.Agents.StaleAfter.Duration = -time.Hour
	if len(countAgents(st, nil, keepNow, cfg).Busy) != 1 {
		t.Fatal("disabled stale retirement releases capacity")
	}
}

func TestGoneFailedWakeRestoresAge(t *testing.T) {
	a, _ := stubApp(t)
	runLive(t, a, nil)
	ag := tasked("failed-wake-test")
	ag.Registered = a.now.Add(-48 * time.Hour)
	seed(t, a, func(st *state.State) { st.Agents = []state.Agent{ag} })
	if err := a.wakeAgent(t.Context(), waker, ag.Name, "continue", ""); err == nil {
		t.Fatal("wake without transcript succeeded")
	}
	st, _ := a.store.Read()
	if len(countAgents(st, nil, a.now, a.cfg).Gone) != 1 {
		t.Fatal("failed wake kept a dead task busy")
	}
}

// The retirement reservation applies only to roster entries: a session off
// the roster must still be reachable through the normal wake route.
func TestGoneProtectionAllowsWakeOffTheRoster(t *testing.T) {
	a, launcher, _ := wakeTwinSetup(t, offID, false)
	dir := t.TempDir()
	path := filepath.Join(a.cfg.Claude.ProjectsDir, "off-roster-proof", offID+".jsonl")
	writeFile(t, path, `{"type":"user","cwd":"`+filepath.ToSlash(dir)+`"}`+"\n")
	seed(t, a, func(st *state.State) { st.Agents = nil })
	if err := a.wakeAgent(t.Context(), waker, offID, "continue", ""); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(launcher.units) != 1 || len(st.Agents) != 0 {
		t.Fatalf("off-roster wake: units=%d agents=%+v", len(launcher.units), st.Agents)
	}
}

func TestGoneLoopRetiresOnlyInactiveTask(t *testing.T) {
	r := newLoopRig(t)
	a := r.a
	a.cfg.Agents.Mode = config.ModeBackground
	a.cfg.Agents.StaleAfter.Duration = 24 * time.Hour
	a.cfg.Claude.ProjectsDir = t.TempDir()
	a.cfg.Claude.DesktopDir = t.TempDir()
	a.cfg.Claude.SessionsDir = t.TempDir()
	runLive(t, a, nil)
	stubBackground(t, "")
	old := a.now.Add(-48 * time.Hour)
	gone, fresh := tasked("loop-gone"), tasked("loop-fresh")
	gone.Registered, fresh.Registered = old, old
	transcript := filepath.Join(a.cfg.Claude.ProjectsDir, "loop-proof", fresh.Session+".jsonl")
	writeFile(t, transcript, "{}\n")
	if err := os.Chtimes(transcript, a.now, a.now); err != nil {
		t.Fatal(err)
	}
	seed(t, a, func(st *state.State) {
		st.Agents = []state.Agent{gone, fresh}
		st.Records = []state.Record{{Session: gone.Party, At: old}}
	})
	l := &loop.Loop{Host: a}
	if err := l.Roster(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Agents) != 1 || !st.Agents[0].Is(fresh.Party) || st.Records[0].Ended.IsZero() {
		t.Fatalf("loop retirement: agents=%+v records=%+v", st.Agents, st.Records)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "agents.gone" })
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Detail, gone.Task) {
		t.Fatalf("gone events=%+v err=%v", evs, err)
	}
}

func TestStoppedAndIdleReviewersFreeCapacityAndRetire(t *testing.T) {
	now := keepNow
	capCfg := config.Capacity{
		Providers: map[string]config.Provider{
			config.AgyHarness: {},
			codex.Harness:     {},
		},
	}
	stoppedAgy := state.Agent{
		Party:      state.Party{HostSession: agy.HostPrefix + "rev-agy", Name: "Review heatpump-bridge 196 g37"},
		Harness:    agy.Harness,
		Model:      "gemini-4-argon-medium",
		Task:       "Review https://github.com/teemow/heatpump-bridge/pull/196",
		Registered: now.Add(-10 * time.Minute),
	}
	idleCodex := state.Agent{
		Party:      state.Party{HostSession: codex.HostPrefix + "rev-codex", Name: "Review beekeeper 3394 x1"},
		Harness:    codex.Harness,
		Model:      testGoneCodexModel,
		Task:       "Review https://github.com/giantswarm/beekeeper/pull/3394",
		Registered: now.Add(-10 * time.Minute),
		ActivityAt: now.Add(-5 * time.Minute),
	}
	busyAgy := state.Agent{
		Party:      state.Party{HostSession: agy.HostPrefix + "rev-busy", Name: "Review beekeeper 3592 g38"},
		Harness:    agy.Harness,
		Model:      "gemini-4-argon-medium",
		Task:       "Review https://github.com/giantswarm/beekeeper/pull/3592",
		Registered: now.Add(-10 * time.Minute),
	}
	staleIdleWorker := state.Agent{
		Party:      state.Party{HostSession: codex.HostPrefix + "worker-codex", Name: "beekeeper 3394"},
		Harness:    codex.Harness,
		Model:      testGoneCodexModel,
		Task:       "Work https://github.com/giantswarm/beekeeper/issues/3394",
		Registered: now.Add(-48 * time.Hour),
		ActivityAt: now.Add(-25 * time.Hour),
	}
	sessions := []*claude.Session{
		{
			HostID:     idleCodex.HostSession,
			Name:       idleCodex.Name,
			Harness:    codex.Harness,
			State:      omp.StateIdle,
			Started:    idleCodex.Registered,
			LastActive: idleCodex.ActivityAt,
		},
		{
			HostID:     busyAgy.HostSession,
			Name:       busyAgy.Name,
			Harness:    agy.Harness,
			State:      agy.StateBusy,
			Started:    busyAgy.Registered,
			LastActive: now.Add(-time.Minute),
		},
		{
			HostID:     staleIdleWorker.HostSession,
			Name:       staleIdleWorker.Name,
			Harness:    codex.Harness,
			State:      omp.StateIdle,
			Started:    staleIdleWorker.Registered,
			LastActive: staleIdleWorker.ActivityAt,
		},
	}
	st := &state.State{
		Agents: []state.Agent{stoppedAgy, idleCodex, busyAgy, staleIdleWorker},
	}

	c := countAgents(st, sessions, now, nil)
	if !slices.Equal(c.Busy, []string{busyAgy.Name}) {
		t.Fatalf("countAgents.Busy = %v, want [%s]", c.Busy, busyAgy.Name)
	}
	if len(c.Gone) != 3 {
		t.Fatalf("countAgents.Gone = %v, want 3 gone entries", c.Gone)
	}
	pa := countProviderAgents(st, sessions, capCfg, now, nil)
	if !slices.Equal(pa[config.AgyHarness].Busy, []string{busyAgy.Name}) {
		t.Fatalf("agy provider busy = %v, want [%s]", pa[config.AgyHarness].Busy, busyAgy.Name)
	}
	if len(pa[codex.Harness].Busy) != 0 {
		t.Fatalf("codex provider busy = %v, want empty", pa[codex.Harness].Busy)
	}
	chores := planChores(st, sessions, func(string) (*claude.Record, bool) { return nil, false }, func(p state.Party) bool {
		return p.Is(busyAgy.Party)
	}, 24*time.Hour, now, nil)
	var removed []string
	for _, ch := range chores {
		if ch.kind == choreRemove && ch.gone {
			removed = append(removed, ch.agent.Name)
		}
	}
	wantRemoved := []string{stoppedAgy.Name, idleCodex.Name, staleIdleWorker.Name}
	if !slices.Equal(removed, wantRemoved) {
		t.Fatalf("planChores removed = %v, want %v", removed, wantRemoved)
	}
}

const testGoneCodexModel = "gpt-6.1-sol"

func TestGoneAgentRecentRosterSkipsEndedTranscripts(t *testing.T) {
	a, _ := stubApp(t)
	const id = "codex-recent-roster"
	ag := state.Agent{
		Party:      state.Party{HostSession: codex.HostPrefix + id, Name: "beekeeper 3657 c1"},
		Harness:    codex.Harness,
		Model:      testGoneCodexModel,
		Task:       "Work https://github.com/giantswarm/beekeeper/issues/3657",
		Registered: a.now.Add(-time.Hour),
	}
	st := &state.State{Agents: []state.Agent{ag}}

	// Put a newer stream log on disk; while roster activity is within staleAfter,
	// goneAgent returns the roster timestamp without consulting disk transcripts.
	logPath := codex.LogPath(a.cfg.StateDir, id)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := a.now.Add(-5 * time.Minute)
	if err := os.Chtimes(logPath, fresh, fresh); err != nil {
		t.Fatal(err)
	}

	last, gone := goneAgent(a.cfg, st, nil, ag, 24*time.Hour, a.now)
	if gone || !last.Equal(ag.Registered) {
		t.Fatalf("recent roster goneAgent = (%v, %v), want (%v, false)", last, gone, ag.Registered)
	}

	// Once roster activity exceeds staleAfter, goneAgent checks the transcript on disk.
	ag.Registered = a.now.Add(-48 * time.Hour)
	st.Agents[0] = ag
	last, gone = goneAgent(a.cfg, st, nil, ag, 24*time.Hour, a.now)
	if gone || !last.Equal(fresh) {
		t.Fatalf("stale roster with fresh log goneAgent = (%v, %v), want (%v, false)", last, gone, fresh)
	}
}
