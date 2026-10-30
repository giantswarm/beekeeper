package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	loopPR     = "o/r#7"
	loopURL    = "https://github.com/o/r/pull/7"
	loopHead   = "1111111111111111111111111111111111111111"
	loopFixed  = "2222222222222222222222222222222222222222"
	loopWorker = "worker one"
	loopPerson = "pat"
	// loopLocal is a self-hosted provider.
	loopLocal = "local"
)

// loopRig is a loop's app over a scratch store, with the beekeeper commands
// and the pull request reads it makes faked.
type loopRig struct {
	a     *app
	out   *bytes.Buffer
	calls [][]string
	// answer answers a beekeeper command line (without the loop's --as):
	// its output and exit code.
	answer func(args []string) (string, int)
	pull   superloop.Pull
	// cmds are the configured commands run, with their stdin.
	cmds    [][]string
	stdin   []string
	command func(argv []string) (string, int)
}

func newLoopRig(t *testing.T) *loopRig {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cfg := &config.Config{Guide: config.Guide{Person: loopPerson}}
	cfg.Supervisor.ReviewRounds = 1
	cfg.Supervisor.ClassifierConfidence = 0.8
	cfg.Supervisor.LoopInterval.Duration = time.Second
	cfg.Supervisor.ReleaseEvery.Duration = time.Hour
	cfg.Supervisor.NoteDue.Duration = 24 * time.Hour
	r := &loopRig{a: &app{store: store, out: &out, now: time.Now(), cfg: cfg}, out: &out}
	r.answer = func([]string) (string, int) { return "", 0 }
	r.command = func([]string) (string, int) { return "", 0 }
	r.pull = superloop.Pull{Ref: loopPR, URL: loopURL, State: superloop.Open, Head: loopHead, Green: true}
	run, read, command := beekeeperRun, readLoopPull, loopCommand
	t.Cleanup(func() { beekeeperRun, readLoopPull, loopCommand = run, read, command })
	beekeeperRun = func(_ context.Context, args ...string) (string, int, error) {
		if len(args) >= 2 && args[0] == "--as" {
			args = args[2:]
		}
		r.calls = append(r.calls, args)
		o, code := r.answer(args)
		return o, code, nil
	}
	readLoopPull = func(_ context.Context, ref string) (superloop.Pull, error) {
		if ref != loopPR {
			t.Fatalf("read %s", ref)
		}
		return r.pull, nil
	}
	loopCommand = func(_ context.Context, stdin string, argv []string) (string, int, error) {
		r.cmds = append(r.cmds, argv)
		r.stdin = append(r.stdin, stdin)
		o, code := r.command(argv)
		return o, code, nil
	}
	return r
}

// park puts the worker on the roster, parked on on at.
func (r *loopRig) park(t *testing.T, on string, at time.Time) {
	t.Helper()
	err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == loopWorker })
		if i < 0 {
			st.Agents = append(st.Agents, state.Agent{Party: state.Party{Name: loopWorker}, Task: "fix " + loopURL})
			i = len(st.Agents) - 1
		}
		st.Agents[i].Park = &state.Park{On: on, At: at, Waits: "desk review"}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// pass runs one pass of a fresh loop, as a restarted one would.
func (r *loopRig) pass(t *testing.T) {
	t.Helper()
	r.calls = nil
	if err := r.a.runSupervisorLoop(context.Background(), true); err != nil {
		t.Fatal(err)
	}
}

// called is the call whose line starts with prefix.
func (r *loopRig) called(prefix ...string) []string {
	for _, c := range r.calls {
		if len(c) >= len(prefix) && slices.Equal(c[:len(prefix)], prefix) {
			return c
		}
	}
	return nil
}

func (r *loopRig) logged(t *testing.T, verb string) []state.Event {
	t.Helper()
	evs, err := r.a.store.Events(0, func(e state.Event) bool { return e.Verb == verb })
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestLoopTakesAParkedPullRequestFromReviewToTheMergeGo(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	start := r.called(agentsName, agentStartName)
	if start == nil || start[2] != "Review r 7 r1" || !slices.Contains(start, "--review") {
		t.Fatalf("no reviewer started: %v\n%s", r.calls, r.out)
	}
	brief, err := os.ReadFile(start[3])
	if err != nil || !strings.Contains(string(brief), loopURL) || !strings.Contains(string(brief), loopHead) {
		t.Errorf("the review brief = %q, %v", brief, err)
	}
	if evs := r.logged(t, "supervisor.review"); len(evs) != 1 || evs[0].Subject != loopPR {
		t.Errorf("logged %+v", evs)
	}
	// A restarted loop starts no second reviewer.
	r.pass(t)
	if c := r.called(agentsName, agentStartName); c != nil {
		t.Errorf("a restart started %v again", c)
	}
	r.pull.Reviews = []superloop.Review{{ID: "R1", Head: loopHead, Verdict: superloop.Approved, At: time.Now()}}
	r.pass(t)
	wake := r.called(agentsName, "wake", loopWorker)
	if wake == nil || !strings.Contains(wake[3], "--match-head-commit "+loopHead) {
		t.Fatalf("no merge go: %v\n%s", r.calls, r.out)
	}
	if evs := r.logged(t, "supervisor.go"); len(evs) != 1 || !strings.Contains(evs[0].Detail, "[go "+loopPR+"@"+loopHead+"]") {
		t.Errorf("the go logged as %+v", evs)
	}
	r.pass(t)
	if c := r.called(agentsName, "wake"); c != nil {
		t.Errorf("the go was given twice: %v", c)
	}
	r.pull.State = superloop.Merged
	r.pass(t)
	r.pass(t)
	if evs := r.logged(t, "supervisor.end"); len(evs) != 1 {
		t.Errorf("the end logged as %+v", evs)
	}
}

func TestLoopReviewOfASelfHostedWorkerExcludesTheSelfHostedProviders(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Capacity = config.Capacity{
		Providers: map[string]config.Provider{loopLocal: {Kind: config.KindSelfHosted}, config.ClaudeHarness: {}},
		Choices:   []config.Choice{{Harness: config.OmpHarness, Model: "qwen", Provider: loopLocal}, {Harness: config.ClaudeHarness, Model: "opus"}},
	}
	r.park(t, loopPR, time.Now())
	_ = r.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Agents[0].Model = "qwen"; return nil, nil })
	r.pass(t)
	start := r.called(agentsName, agentStartName)
	if !slices.Contains(start, "--harness") || !slices.Contains(start, "--exclude") || start[len(start)-1] != loopLocal {
		t.Errorf("the reviewer start = %v", start)
	}
}

func TestLoopWaitsForRoomForAReviewer(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.answer = func(args []string) (string, int) {
		if args[0] == agentsName && args[1] == agentStartName {
			return "capacity: no start", ExitRefused
		}
		return "", 0
	}
	r.pass(t)
	if evs := r.logged(t, "supervisor.review"); len(evs) != 0 {
		t.Errorf("a refused start was logged as taken: %+v", evs)
	}
	r.answer = func([]string) (string, int) { return "", 0 }
	r.pass(t)
	if evs := r.logged(t, "supervisor.review"); len(evs) != 1 {
		t.Errorf("the start once there was room: %+v", evs)
	}
}

func TestLoopForwardsChangesThenEscalatesUnfixedFindings(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	body := "Review: changes requested\nHead reviewed: `" + loopHead + "`\n- cmd/x.go:3: wrong"
	r.pull.Reviews = []superloop.Review{{ID: "R1", Head: loopHead, Verdict: superloop.ChangesRequested, Body: body}}
	r.pass(t)
	wake := r.called(agentsName, "wake", loopWorker)
	if wake == nil || !strings.HasSuffix(wake[3], body) {
		t.Fatalf("the findings were not forwarded word for word: %v", r.calls)
	}
	// The worker parks again without a new head: the guide decides.
	r.park(t, loopPR, time.Now().Add(time.Minute))
	r.pass(t)
	note := r.called("note", "add")
	if note == nil || !slices.Contains(note, loopPerson) || !slices.Contains(note, loopURL) {
		t.Fatalf("no note for the guide's person: %v\n%s", r.calls, r.out)
	}
	if evs := r.logged(t, "supervisor.escalate"); len(evs) != 1 {
		t.Errorf("escalate logged as %+v", evs)
	}
	r.pass(t)
	if len(r.calls) > 0 && r.called("note", "add") != nil {
		t.Errorf("escalated twice: %v", r.calls)
	}
}

func TestLoopGivesTheFixedHeadTheGoWithoutASecondReview(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.pull.Reviews = []superloop.Review{{ID: "R1", Head: loopHead, Verdict: superloop.ChangesRequested, Body: "Review: changes requested"}}
	r.pass(t)
	r.pull.Head = loopFixed
	r.park(t, loopPR, time.Now().Add(time.Minute))
	r.pass(t)
	if c := r.called(agentsName, agentStartName); c != nil {
		t.Errorf("a second review started: %v", c)
	}
	wake := r.called(agentsName, "wake", loopWorker)
	if wake == nil || !strings.Contains(wake[3], "--match-head-commit "+loopFixed) {
		t.Errorf("no go for the fixed head: %v", r.calls)
	}
}

func TestLoopTakesADoneWorkerOffTheRosterAndStartsTheNext(t *testing.T) {
	r := newLoopRig(t)
	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("Work {{.URL}}.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.a.cfg.Supervisor.WorkerBrief = brief
	r.park(t, loopPR, time.Now())
	r.pull.State = superloop.Merged
	r.pass(t) // the loop's start bounds its events
	// The caller's party carries its session's title, not its roster
	// name, and its done and report are logged a moment apart.
	titled := state.Party{Name: "#1743: a session title"}
	done := event(titled, "agents.done", "%s done: fix it", loopWorker)
	report := event(titled, "agents.report", "%s: merged %s", loopWorker, loopURL)
	report.At = done.At.Add(10 * time.Microsecond)
	if err := r.a.store.Record(done, report); err != nil {
		t.Fatal(err)
	}
	r.answer = func(args []string) (string, int) {
		if args[0] == "board" {
			return `{"pick":{"ref":"o/r#9","url":"https://github.com/o/r/issues/9","title":"next"}}`, 0
		}
		return "", 0
	}
	r.pass(t)
	if c := r.called(agentsName, "remove", loopWorker); c == nil {
		t.Fatalf("not taken off the roster: %v\n%s", r.calls, r.out)
	}
	if c := r.called("note", "add"); c != nil {
		t.Errorf("the final report went to the guide: %v", c)
	}
	r.pass(t)
	start := r.called(agentsName, agentStartName, "r 9")
	if start == nil {
		t.Fatalf("the next item did not start: %v\n%s", r.calls, r.out)
	}
	if b, _ := os.ReadFile(start[3]); string(b) != "Work https://github.com/o/r/issues/9.\n" {
		t.Errorf("the worker brief = %q", b)
	}
	r.pass(t)
	if c := r.called(agentsName); c != nil {
		t.Errorf("acted again: %v", c)
	}
}

func TestLoopInstallsANewerRelease(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Supervisor.PostInstall = []string{"restart-things"}
	installed := false
	r.answer = func(args []string) (string, int) {
		if slices.Equal(args, selfUpdateCheck) && !installed {
			return "v2.0.0 is newer than v1.0.0: beekeeper self-update installs it", ExitOutdated
		}
		if slices.Equal(args, []string{"self-update"}) {
			installed = true
		}
		return "", 0
	}
	r.pass(t)
	if !installed || len(r.cmds) != 1 || r.cmds[0][0] != "restart-things" {
		t.Fatalf("installed %v, post-install %v: %v\n%s", installed, r.cmds, r.calls, r.out)
	}
	if evs := r.logged(t, "supervisor.release"); len(evs) != 1 || !strings.Contains(evs[0].Detail, "[release v2.0.0]") {
		t.Errorf("logged %+v", evs)
	}
}

func TestLoopSendsWhatItCannotPlaceToTheGuide(t *testing.T) {
	for name, c := range map[string]struct {
		classifier []string
		answer     string
		want       string
	}{
		"no classifier":        {want: "(unclassified)"},
		"a sure classifier":    {classifier: []string{"classify"}, answer: `{"class":"blocked","confidence":0.9}`, want: "(blocked)"},
		"an unsure classifier": {classifier: []string{"classify"}, answer: `{"class":"blocked","confidence":0.5}`, want: "(unclassified)"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newLoopRig(t)
			r.a.cfg.Supervisor.Classifier = c.classifier
			r.command = func([]string) (string, int) { return c.answer, 0 }
			r.park(t, "", time.Now())
			r.pull.State = superloop.Merged
			r.pass(t)
			if err := r.a.store.Record(event(state.Party{Name: loopWorker}, "agents.problem", "%s: CI is broken on main", loopWorker)); err != nil {
				t.Fatal(err)
			}
			r.pass(t)
			note := r.called("note", "add")
			if note == nil || !strings.Contains(note[len(note)-1], c.want) || !slices.Contains(note, loopURL) {
				t.Fatalf("the note = %v\n%s", note, r.out)
			}
			if c.classifier != nil && !strings.Contains(r.stdin[0], "CI is broken on main") {
				t.Errorf("the classifier got %q", r.stdin)
			}
		})
	}
}

func TestLoopPutsAWorkersQuestionInTheGuidesQueue(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, "", time.Now())
	r.pass(t)
	err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.NextNote++
		st.Notes = append(st.Notes, state.Note{ID: st.NextNote, For: supervisorName, Text: "which lab?", By: state.Party{Name: loopWorker}})
		return []state.Event{event(state.Party{Name: loopWorker}, "note.add", "#%d which lab?", st.NextNote)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Notes[0].For != loopPerson {
		t.Errorf("the note is for %q", st.Notes[0].For)
	}
	if evs := r.logged(t, "supervisor.question"); len(evs) != 1 {
		t.Errorf("logged %+v", evs)
	}
}

func TestLoopIsQuietWhenNothingHappens(t *testing.T) {
	r := newLoopRig(t)
	r.pass(t)
	r.pass(t)
	// The release look is the one call an idle pass makes.
	if r.out.Len() != 0 || len(r.calls) != 1 || !slices.Equal(r.calls[0], selfUpdateCheck) {
		t.Errorf("an idle loop said %q and ran %v", r.out, r.calls)
	}
}

func TestParseLoopPull(t *testing.T) {
	green := `{"state":"OPEN","url":"` + loopURL + `","headRefOid":"` + loopHead + `",
	  "statusCheckRollup":[{"status":"COMPLETED","conclusion":"SUCCESS"},{"state":"SUCCESS"},{"status":"COMPLETED","conclusion":"SKIPPED"}],
	  "reviews":[{"id":"R2","state":"COMMENTED","body":"Review: approved\nHead reviewed: ` + "`1111111`" + `","submittedAt":"2026-10-30T10:00:00Z","commit":{"oid":"abc"}},
	             {"id":"R1","state":"CHANGES_REQUESTED","body":"no","submittedAt":"2026-10-30T09:00:00Z","commit":{"oid":"` + loopHead + `"}}]}`
	p, err := parseLoopPull(loopPR, []byte(green))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Green || len(p.Reviews) != 2 || p.Reviews[0].ID != "R1" || p.Reviews[1].Verdict != superloop.Approved || p.Reviews[1].Head != "1111111" {
		t.Errorf("pull = %+v", p)
	}
	for name, rollup := range map[string]string{
		"no check":  `[]`,
		"pending":   `[{"status":"IN_PROGRESS","conclusion":""}]`,
		"a failure": `[{"status":"COMPLETED","conclusion":"SUCCESS"},{"state":"FAILURE"}]`,
	} {
		p, err := parseLoopPull(loopPR, []byte(`{"state":"OPEN","headRefOid":"x","statusCheckRollup":`+rollup+`}`))
		if err != nil || p.Green {
			t.Errorf("%s: green %v, %v", name, p.Green, err)
		}
	}
}
