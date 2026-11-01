package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
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

func TestGoneRetirementReleasesBoardAndPreservesHandover(t *testing.T) {
	a, _ := stubApp(t)
	runLive(t, a, nil)
	ag := tasked("retire-test")
	ag.Registered = a.now.Add(-48 * time.Hour)
	const issue = "o/r#21"
	ag.Task = issue + ": unfinished work"
	seed(t, a, func(st *state.State) {
		st.Supervisor = nil
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
	removed, err := a.removeAgents(chores, watchParty)
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

func TestGoneRetirementRechecksActivity(t *testing.T) {
	for _, change := range []string{agentWakeName, "keep", "transcript"} {
		t.Run(change, func(t *testing.T) {
			a, _ := stubApp(t)
			runLive(t, a, nil)
			ag := tasked("recheck-test")
			ag.Registered = a.now.Add(-48 * time.Hour)
			seed(t, a, func(st *state.State) { st.Supervisor = nil; st.Agents = []state.Agent{ag} })
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
			removed, err := a.removeAgents(chores, watchParty)
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
			seed(t, a, func(st *state.State) { st.Supervisor = nil; st.Agents = []state.Agent{ag} })
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

func TestGoneWatchReportsAgeTransition(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	w.cfg.Claude.ProjectsDir = t.TempDir()
	w.cfg.Claude.DesktopDir = t.TempDir()
	ag := tasked("watch-gone-test")
	ag.Registered = w.now.Add(-23 * time.Hour)
	st := &state.State{Agents: []state.Agent{ag}}
	w.stoppedAgents(st, nil)
	if strings.Contains(out.String(), strings.TrimSpace(goneActivityPrefix)) {
		t.Fatal("recent worker shown gone")
	}
	w.now = w.now.Add(time.Hour)
	w.stoppedAgents(st, nil)
	w.stoppedAgents(st, nil)
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
	seed(t, a, func(st *state.State) { st.Supervisor = nil; st.Agents = []state.Agent{ag} })
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
	seed(t, a, func(st *state.State) { st.Supervisor = nil; st.Agents = nil })
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
	l := &supervisorLoop{a: a}
	if err := l.roster(t.Context(), nil); err != nil {
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
