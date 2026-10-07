package cmd

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

var keepNow = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

// keeper is who sets the keep markers of these tests.
const keeper = "test: keeper"

// staleAgent is an idle roster entry with no CLI, stale past 24h.
func staleAgent(name string) state.Agent {
	return state.Agent{Party: state.Party{Session: "s-" + name, Name: name}, IdleSince: keepNow.Add(-48 * time.Hour)}
}

// A stale entry with a keep marker, or with an open timer that wakes it by
// name, stays on the roster with its desktop session; an expired marker, a
// lifted one and a done timer leave it to the staleAfter rule.
func TestPlanChoresLeavesKeptEntries(t *testing.T) {
	kept, expired, woken, plain := staleAgent("Judge"), staleAgent("Spare"), staleAgent("BK 1"), staleAgent("Gone")
	kept.Keep = &state.Keep{Reason: "a person returns to it"}
	expired.Keep = &state.Keep{Until: keepNow.Add(-time.Minute), Reason: "over"}
	st := &state.State{
		Agents: []state.Agent{kept, expired, woken, plain},
		Timers: []state.Timer{{ID: 7, Due: keepNow.Add(2 * time.Hour), What: "the batch job ended", Wake: "bk 1"}},
	}
	plan := func() []string {
		var got []string
		for _, c := range planChores(st, nil, func(string) (*claude.Record, bool) { return nil, false }, func(state.Party) bool { return false }, 24*time.Hour, keepNow) {
			got = append(got, c.String())
		}
		return got
	}
	want := []string{
		`leave "Judge" on the roster and its desktop session unarchived (idle 2d, its CLI no longer runs, but kept: a person returns to it)`,
		`take "Spare" off the roster and archive its desktop session (idle 2d, its CLI no longer runs)`,
		`leave "BK 1" on the roster and its desktop session unarchived (idle 2d, its CLI no longer runs, but timer #7 wakes it)`,
		`take "Gone" off the roster and archive its desktop session (idle 2d, its CLI no longer runs)`,
	}
	if got := plan(); !slices.Equal(got, want) {
		t.Errorf("chores:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	st.Agents[0].Keep, st.Timers = nil, nil // lifted, and the timer done
	for _, c := range plan() {
		if strings.HasPrefix(c, "leave") {
			t.Errorf("nothing keeps an entry any more: %s", c)
		}
	}
}

// The doctor's removal checks the marker again under the lock: a keep set
// after the plan stops a removal for staleness, never one for a done task.
func TestRemoveAgentsHonoursAKeepSetSincePlan(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{StateDir: dir}, store: store, now: keepNow, out: &bytes.Buffer{}}
	stale, done := staleAgent("Judge"), staleAgent("Done")
	done.Done = true
	st := &state.State{Agents: []state.Agent{stale, done}}
	chores := planChores(st, nil, func(string) (*claude.Record, bool) { return nil, false }, func(state.Party) bool { return false }, 24*time.Hour, keepNow)
	stale.Keep, done.Keep = &state.Keep{}, &state.Keep{}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{stale, done}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	removed, err := a.removeAgents(chores, watchParty)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := store.Read()
	if len(removed) != 1 || removed[0].agent.Name != "Done" || len(after.Agents) != 1 || after.Agents[0].Name != "Judge" {
		t.Errorf("removed %v, roster %v", removed, after.Agents)
	}
}

func runAgents(a *app, args ...string) (string, error) {
	c := a.agentsCmd()
	usageArgs(c)
	c.SetArgs(args)
	c.SetOut(a.out)
	c.SetErr(a.out)
	c.SilenceUsage, c.SilenceErrors = true, true
	err := c.ExecuteContext(context.Background())
	return a.out.(*bytes.Buffer).String(), err
}

// agents keep sets the marker with its reason and end, --no-keep lifts it,
// and the list's KEPT column shows it.
func TestAgentsKeep(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{staleAgent("Judge")}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	newApp := func() *app {
		return &app{cfg: &config.Config{StateDir: dir}, store: store, now: keepNow, out: &bytes.Buffer{}, as: keeper}
	}
	out, err := runAgents(newApp(), "keep", "judge", "--until", "6h", "--reason", "a person  returns to it")
	if err != nil || !strings.Contains(out, "Judge: kept until") || !strings.Contains(out, ": a person returns to it") {
		t.Fatalf("keep: %q, %v", out, err)
	}
	st, _ := store.Read()
	k := st.Agents[0].Keep
	if k == nil || !k.Until.Equal(keepNow.Add(6*time.Hour)) || k.By.Name != keeper || !k.Holds(keepNow) || k.Holds(keepNow.Add(6*time.Hour)) {
		t.Errorf("marker %+v", k)
	}
	a := newApp()
	if v := a.agentViews(st, nil); v[0].Kept == "" {
		t.Errorf("view %+v carries no KEPT", v[0])
	}
	a.printAgents(a.agentViews(st, nil))
	if s := a.out.(*bytes.Buffer).String(); !strings.Contains(s, "KEPT") || !strings.Contains(s, "a person returns to it") {
		t.Errorf("list:\n%s", s)
	}
	if _, err := runAgents(newApp(), "keep", "Judge", "--no-keep", "--reason", "x"); err == nil {
		t.Errorf("--no-keep with --reason: %v", err)
	}
	if _, err := runAgents(newApp(), "keep", "Judge", "--until", "-1h"); Code(err) != ExitUsage {
		t.Errorf("--until in the past: %v", err)
	}
	if out, err := runAgents(newApp(), "keep", "Judge", "--no-keep"); err != nil || !strings.Contains(out, "back under agents.staleAfter") {
		t.Errorf("--no-keep: %q, %v", out, err)
	}
	if st, _ = store.Read(); st.Agents[0].Keep != nil {
		t.Errorf("marker not lifted: %+v", st.Agents[0].Keep)
	}
	if _, err := runAgents(newApp(), "keep", "Judge", "--no-keep"); Code(err) != ExitRefused {
		t.Errorf("lifting no marker: %v", err)
	}
}

// A session registering again keeps its entry's marker.
func TestRegisterAgentCarriesTheKeep(t *testing.T) {
	me := state.Party{Session: "s-judge", Name: "Judge"}
	st := &state.State{Agents: []state.Agent{{Party: me, Keep: &state.Keep{Reason: "a judge"}}}}
	if _, err := registerAgent(st, me, func(state.Party) bool { return false }, keepNow); err != nil {
		t.Fatal(err)
	}
	if len(st.Agents) != 1 || st.Agents[0].Keep == nil || st.Agents[0].Keep.Reason != "a judge" {
		t.Errorf("roster %+v", st.Agents)
	}
}

// The watch says no AGENTS STOPPED line for an entry parked on purpose.
func TestStoppedAgentsLeaveKeptEntriesOut(t *testing.T) {
	w, _, out := reportingWatch(t, t.TempDir())
	kept, woken, stopped := staleAgent("Judge"), staleAgent("BK 1"), staleAgent("Crashed")
	kept.Task, woken.Task, stopped.Task = "review", "a batch job", "a crash"
	kept.Keep = &state.Keep{}
	st := &state.State{Agents: []state.Agent{kept, woken, stopped}, Timers: []state.Timer{{ID: 1, Wake: "BK 1"}}}
	w.stoppedAgents(st, nil)
	if s := out.String(); !strings.Contains(s, `"Crashed"`) || strings.Contains(s, "Judge") || strings.Contains(s, "BK 1") {
		t.Errorf("stopped line:\n%s", s)
	}
}

// board next counts a parked agent's serve record as covering its item
// while a keep marker or a timer that wakes it keeps the agent, its CLI
// running or not, and offers the item again once nothing keeps it.
func TestNextFreeCoversParkedAgentsItems(t *testing.T) {
	listed := keepNow
	me := state.Party{Session: "me", Name: "Me"}
	parked, woken, idle, done := staleAgent("Judge"), staleAgent("BK 1"), staleAgent("Spare"), staleAgent("Finished")
	parked.Keep = &state.Keep{Reason: "reviews"}
	done.Keep, done.Done = &state.Keep{}, true
	alive := func(p state.Party) bool { return p.Is(idle.Party) }
	st := &state.State{
		Agents: []state.Agent{parked, woken, idle, done},
		Timers: []state.Timer{{ID: 4, Wake: "BK 1"}},
		Records: []state.Record{
			{Session: parked.Party, At: listed.Add(-time.Hour)},
			{Session: woken.Party, At: listed.Add(-time.Hour), Ended: listed.Add(-time.Minute)},
			{Session: idle.Party, At: listed.Add(-time.Hour)},
			{Session: done.Party, At: listed.Add(-time.Hour)},
		},
	}
	cands := boardCandidates(4)
	for i := range st.Records {
		st.Records[i].Issue = cands[i].Ref
	}
	res := nextFree(st, cands, me, alive, listed, nil)
	var got []string
	for _, c := range res.Skipped {
		got = append(got, c.Ref+": "+c.Skip)
	}
	want := []string{cands[0].Ref + `: served by "Judge" (parked, kept: reviews)`, cands[1].Ref + `: served by "BK 1" (parked, timer #4 wakes it)`}
	if !slices.Equal(got, want) || res.Pick == nil || res.Pick.Ref != cands[2].Ref {
		t.Errorf("pick %v, skipped:\n%s\nwant:\n%s", res.Pick, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	st.Agents[0].Keep, st.Timers = nil, nil
	if res = nextFree(st, cands, me, alive, listed, nil); res.Pick == nil || res.Pick.Ref != cands[0].Ref {
		t.Errorf("nothing keeps them any more: pick %v, skipped %+v", res.Pick, res.Skipped)
	}
}

// An agent on the roster busy with its task owns its serve record while its
// CLI is gone and the watch marked the record ended, without a keep marker:
// board next skips its item and a claim refuses it, until the agent reports
// idle.
func TestNextFreeCoversABusyAgentWithoutACLI(t *testing.T) {
	listed := keepNow
	me := state.Party{Session: "me", Name: "Me"}
	busy := staleAgent("Board pull 153")
	busy.Task, busy.IdleSince = "the next free board item to done", time.Time{}
	cands := boardCandidates(2)
	st := &state.State{
		Agents:  []state.Agent{busy},
		Records: []state.Record{{Session: busy.Party, Issue: cands[0].Ref, At: listed.Add(-time.Hour), Ended: listed.Add(-time.Minute)}},
	}
	gone := func(state.Party) bool { return false }
	res := nextFree(st, cands, me, gone, listed, nil)
	want := `served by "Board pull 153" (busy, no CLI)`
	if res.Pick == nil || res.Pick.Ref != cands[1].Ref || len(res.Skipped) != 1 || res.Skipped[0].Skip != want {
		t.Errorf("pick %v, skipped %+v; want %s skipped with %q", res.Pick, res.Skipped, cands[0].Ref, want)
	}

	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(s *state.State) ([]state.Event, error) { *s = *st; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	only := cands[:1]
	if res, err := claimNext(store, only, me, gone, listed, "picking up", func(string) bool { return true }, nil); err != nil || res.Claimed || res.Pick != nil {
		t.Errorf("claim over a busy agent's item: %+v, %v", res, err)
	}

	st.Agents[0].Task, st.Agents[0].IdleSince = "", listed
	if res = nextFree(st, cands, me, gone, listed, nil); res.Pick == nil || res.Pick.Ref != cands[0].Ref {
		t.Errorf("the agent reported idle: pick %v, skipped %+v", res.Pick, res.Skipped)
	}
}
