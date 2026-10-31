package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/claude"
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
	// loopQwen is a self-hosted model, loopPostInstall a post-install
	// command, loopClassifier a classifier.
	loopQwen        = "qwen"
	loopPostInstall = "restart-things"
	loopClassifier  = "classify"
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
	// stderr is what a configured command writes on stderr.
	stderr string
	// replaced says the loop's binary was replaced; execs counts its
	// re-executions.
	replaced bool
	execs    int
	// broker answers the broker stage of a release.
	broker func() (string, error)
	// failRecord fails the store's Record of the first event it accepts.
	failRecord func(state.Event) bool
	// inTurn says a session runs a turn.
	inTurn func(state.Party) bool
}

// loopFakeBinary is the loop's binary as the rig says it.
type loopFakeBinary struct{ r *loopRig }

func (b loopFakeBinary) Replaced() bool { return b.r.replaced }

func (b loopFakeBinary) Exec(...string) error {
	b.r.execs++
	b.r.replaced = false
	return nil
}

// rigStore fails a Record the rig names, once.
type rigStore struct {
	state.Store
	r *loopRig
}

func (s rigStore) Record(events ...state.Event) error {
	if f := s.r.failRecord; f != nil && slices.ContainsFunc(events, f) {
		s.r.failRecord = nil
		return errors.New("disk full")
	}
	return s.Store.Record(events...)
}

// logs is the line the real command logs as the loop for args, which a
// claim's reconciliation reads.
func logs(args []string) (state.Event, bool) {
	if len(args) < 3 {
		return state.Event{}, false
	}
	switch {
	case args[0] == agentsName && args[1] == agentStartName:
		return event(loopParty, "agents.start", "%s: background session x", args[2]), true
	case args[0] == agentsName && args[1] == agentWakeName:
		return event(loopParty, "agents.wake", "%s: sent by name", args[2]), true
	case args[0] == agentsName && args[1] == loopRemove:
		return event(loopParty, "agents.remove", "%s", args[2]), true
	case args[0] == "note" && args[1] == loopAdd:
		return event(loopParty, "note.add", "#1 %s", args[len(args)-1]), true
	}
	return state.Event{}, false
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
	r := &loopRig{out: &out}
	r.a = &app{store: rigStore{Store: store, r: r}, out: &out, now: time.Now(), cfg: cfg}
	r.answer = func([]string) (string, int) { return "", 0 }
	r.command = func([]string) (string, int) { return "", 0 }
	r.pull = superloop.Pull{Ref: loopPR, URL: loopURL, State: superloop.Open, Head: loopHead, Green: true}
	r.broker = func() (string, error) { return "no broker runs", nil }
	r.inTurn = func(state.Party) bool { return false }
	run, read, command, binary, broker, turn := beekeeperRun, readLoopPull, loopCommand, loopBinary, loopBroker, loopInTurn
	t.Cleanup(func() {
		beekeeperRun, readLoopPull, loopCommand, loopBinary, loopBroker, loopInTurn = run, read, command, binary, broker, turn
	})
	loopInTurn = func(_ *app, p state.Party) bool { return r.inTurn(p) }
	loopBinary = func() replaceable { return loopFakeBinary{r} }
	loopBroker = func(context.Context) (string, error) { return r.broker() }
	beekeeperRun = func(_ context.Context, args ...string) (string, int, error) {
		if len(args) >= 2 && args[0] == "--as" {
			args = args[2:]
		}
		r.calls = append(r.calls, args)
		o, code := r.answer(args)
		if e, ok := logs(args); ok && code == 0 {
			if err := store.Record(e); err != nil {
				t.Fatal(err)
			}
		}
		return o, code, nil
	}
	readLoopPull = func(_ context.Context, ref string) (superloop.Pull, error) {
		if ref != loopPR {
			t.Fatalf("read %s", ref)
		}
		return r.pull, nil
	}
	loopCommand = func(_ context.Context, stdin string, argv []string) (string, string, int, error) {
		r.cmds = append(r.cmds, argv)
		r.stdin = append(r.stdin, stdin)
		o, code := r.command(argv)
		return o, r.stderr, code, nil
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
	if err := r.passErr(); err != nil {
		t.Fatal(err)
	}
}

func (r *loopRig) passErr() error {
	r.calls = nil
	return r.a.runSupervisorLoop(context.Background(), true)
}

// verdict has the round's reviewer post its verdict on head now.
func (r *loopRig) verdict(id, head, verdict, body string) {
	r.pull.Reviews = append(r.pull.Reviews, superloop.Review{ID: id, Head: head, Verdict: verdict, Body: body, At: time.Now()})
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
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.pass(t)
	wake := r.called(agentsName, agentWakeName, loopWorker)
	if wake == nil || !strings.Contains(wake[3], "--match-head-commit "+loopHead) {
		t.Fatalf("no merge go: %v\n%s", r.calls, r.out)
	}
	if evs := r.logged(t, "supervisor.go"); len(evs) != 1 || !strings.Contains(evs[0].Detail, "[go "+loopPR+"@"+loopHead+"]") {
		t.Errorf("the go logged as %+v", evs)
	}
	r.pass(t)
	if c := r.called(agentsName, agentWakeName); c != nil {
		t.Errorf("the go was given twice: %v", c)
	}
	r.pull.State = superloop.Merged
	r.pass(t)
	r.pass(t)
	if evs := r.logged(t, "supervisor.end"); len(evs) != 1 {
		t.Errorf("the end logged as %+v", evs)
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
	r.verdict("R1", loopHead, superloop.ChangesRequested, body)
	r.pass(t)
	wake := r.called(agentsName, agentWakeName, loopWorker)
	if wake == nil || !strings.HasSuffix(wake[3], body) {
		t.Fatalf("the findings were not forwarded word for word: %v", r.calls)
	}
	// The worker parks again without a new head: the guide decides.
	r.park(t, loopPR, time.Now().Add(time.Minute))
	r.pass(t)
	note := r.called("note", loopAdd)
	if note == nil || !slices.Contains(note, loopPerson) || !slices.Contains(note, loopURL) {
		t.Fatalf("no note for the guide's person: %v\n%s", r.calls, r.out)
	}
	if evs := r.logged(t, "supervisor.escalate"); len(evs) != 1 {
		t.Errorf("escalate logged as %+v", evs)
	}
	r.pass(t)
	if len(r.calls) > 0 && r.called("note", loopAdd) != nil {
		t.Errorf("escalated twice: %v", r.calls)
	}
}

func TestLoopGivesTheFixedHeadTheGoWithoutASecondReview(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.ChangesRequested, "Review: changes requested")
	r.pass(t)
	r.pull.Head = loopFixed
	r.park(t, loopPR, time.Now().Add(time.Minute))
	r.pass(t)
	if c := r.called(agentsName, agentStartName); c != nil {
		t.Errorf("a second review started: %v", c)
	}
	wake := r.called(agentsName, agentWakeName, loopWorker)
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
		if args[0] == loopBoard {
			return `{"pick":{"ref":"o/r#9","url":"https://github.com/o/r/issues/9","title":"next"}}`, 0
		}
		return "", 0
	}
	r.pass(t)
	if c := r.called(agentsName, loopRemove, loopWorker); c == nil {
		t.Fatalf("not taken off the roster: %v\n%s", r.calls, r.out)
	}
	if c := r.called("note", loopAdd); c != nil {
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

// started puts name on the roster as a session beekeeper started.
func (r *loopRig) started(t *testing.T, name, session string) {
	t.Helper()
	p := state.Party{Name: name, Session: session}
	err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		if i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == name }); i >= 0 {
			st.Agents[i].Party = p
		} else {
			st.Agents = append(st.Agents, state.Agent{Party: p, Task: "review " + loopURL})
		}
		st.Starts = append(st.Starts, state.Start{Party: p, Mode: "background", By: loopParty, At: time.Now()})
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLoopRetiresTheReviewerAndTheMergedWorker(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.started(t, loopWorker, "worker-session")
	r.pass(t)
	reviewer := r.called(agentsName, agentStartName)[2]
	r.started(t, reviewer, "reviewer-session")
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.pass(t)
	if c := r.called(agentsName, agentWakeName, loopWorker); c == nil {
		t.Fatalf("no merge go: %v\n%s", r.calls, r.out)
	}
	// The reviewer still writes its report: the retirement waits.
	r.inTurn = func(p state.Party) bool { return p.Session == "reviewer-session" }
	r.pass(t)
	if c := r.called(agentsName, loopRemove); c != nil {
		t.Fatalf("removed in its turn: %v", c)
	}
	r.inTurn = func(state.Party) bool { return false }
	r.pass(t)
	if c := r.called(agentsName, loopRemove, reviewer); c == nil {
		t.Fatalf("the reviewer stays: %v\n%s", r.calls, r.out)
	}
	if c := r.called(agentsName, loopRemove, loopWorker); c != nil {
		t.Errorf("the worker left before its merge: %v", c)
	}
	// The worker merges and never reports.
	r.pull.State = superloop.Merged
	r.pass(t)
	if c := r.called(agentsName, loopRemove, loopWorker); c == nil {
		t.Fatalf("the merged worker stays: %v\n%s", r.calls, r.out)
	}
	if evs := r.logged(t, "supervisor.done"); len(evs) != 2 || !strings.Contains(evs[1].Detail, "merged after the merge go") {
		t.Errorf("logged %+v", evs)
	}
	r.pass(t)
	if c := r.called(agentsName); c != nil {
		t.Errorf("acted again: %v", c)
	}
}

func TestLoopNeverRetiresASessionItsPersonStarted(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.pass(t)
	r.pull.State = superloop.Merged
	r.pass(t)
	r.pass(t)
	if c := r.called(agentsName, loopRemove); c != nil || len(r.logged(t, "supervisor.done")) != 0 {
		t.Errorf("a person's session was retired: %v", c)
	}
}

func TestLoopInstallsANewerRelease(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Supervisor.PostInstall = []string{loopPostInstall}
	installed := false
	r.answer = func(args []string) (string, int) {
		if slices.Equal(args, selfUpdateCheck) && !installed {
			return "v2.0.0 is newer than v1.0.0: beekeeper self-update installs it", ExitOutdated
		}
		if slices.Equal(args, []string{selfUpdateCmdName}) {
			installed = true
		}
		return "", 0
	}
	r.pass(t)
	if !installed || len(r.cmds) != 1 || r.cmds[0][0] != loopPostInstall {
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
		"a sure classifier":    {classifier: []string{loopClassifier}, answer: `{"class":"blocked","confidence":0.9}`, want: "(blocked)"},
		"an unsure classifier": {classifier: []string{loopClassifier}, answer: `{"class":"blocked","confidence":0.5}`, want: "(unclassified)"},
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
			note := r.called("note", loopAdd)
			if note == nil || !strings.Contains(note[len(note)-1], c.want) || !slices.Contains(note, loopURL) {
				t.Fatalf("the note = %v\n%s", note, r.out)
			}
			if c.classifier != nil && !strings.Contains(r.stdin[0], "CI is broken on main") {
				t.Errorf("the classifier got %q", r.stdin)
			}
		})
	}
}

// A done report's problem filed nowhere reaches the guide's queue to file,
// and the done follows; it is no decision.
func TestLoopFilesADoneReportsProblemThroughTheGuidesQueue(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pull.State = superloop.Merged
	r.pass(t)
	done := event(state.Party{Name: loopWorker}, "agents.done", "%s done: fix %s", loopWorker, loopURL)
	problem := event(state.Party{Name: loopWorker}, "agents.problem", "%s: the gate refuses a green head, o/r", loopWorker)
	problem.At = done.At.Add(10 * time.Microsecond)
	if err := r.a.store.Record(done, problem); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	note := r.called("note", loopAdd)
	if note == nil || !strings.Contains(note[len(note)-1], "File in its owning repository what "+loopWorker+" found (1 problem(s), no decision)") ||
		!strings.Contains(strings.Join(note, " "), "the gate refuses a green head") {
		t.Fatalf("the note = %v\n%s", note, r.out)
	}
	if strings.Contains(strings.Join(note, " "), "Decide what") {
		t.Errorf("filed as a decision: %v", note)
	}
	if c := r.called(agentsName, loopRemove, loopWorker); c != nil {
		t.Errorf("off the roster before its problem reached the guide's queue: %v", c)
	}
	r.pass(t)
	if c := r.called(agentsName, loopRemove, loopWorker); c == nil {
		t.Fatalf("not taken off the roster after the file: %v\n%s", r.calls, r.out)
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

func TestLoopGivesNoGoToAnApprovedHeadWithoutGreenChecks(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.pull.Green = false
	r.pass(t)
	if c := r.called(agentsName, agentWakeName); c != nil {
		t.Fatalf("a red head got the go: %v", c)
	}
	r.pull.Green = true
	r.pass(t)
	if c := r.called(agentsName, agentWakeName, loopWorker); c == nil {
		t.Errorf("no go once green: %v", r.calls)
	}
}

// An earlier round's approval authorizes no later head, and a round still
// running authorizes nothing.
func TestLoopAnOldApprovalAuthorizesNoNewHead(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Supervisor.ReviewRounds = 2
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.pull.Head = loopFixed // the head moved before the go
	r.park(t, loopPR, time.Now().Add(time.Minute))
	r.pass(t)
	if start := r.called(agentsName, agentStartName); start == nil || start[2] != "Review r 7 r2" {
		t.Fatalf("no second round for the new head: %v", r.calls)
	}
	r.pass(t)
	if c := r.called(agentsName, agentWakeName); c != nil {
		t.Errorf("the old approval gave the in-flight round's head the go: %v", c)
	}
}

// A request for changes reaches the worker even when the head moved before
// the loop saw it; its fixes get the go only after a park past the forward.
func TestLoopForwardsFindingsOnAMovedHead(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.ChangesRequested, "Review: changes requested\n- x")
	r.pull.Head = loopFixed
	r.pass(t)
	wake := r.called(agentsName, agentWakeName, loopWorker)
	if wake == nil || !strings.Contains(wake[3], "- x") || strings.Contains(wake[3], "--match-head-commit") {
		t.Fatalf("the findings were not forwarded: %v", r.calls)
	}
	r.pass(t)
	if c := r.called(agentsName, agentWakeName); c != nil {
		t.Errorf("a go before the worker parked again: %v", c)
	}
	r.park(t, loopPR, time.Now().Add(time.Minute))
	r.pass(t)
	if wake := r.called(agentsName, agentWakeName, loopWorker); wake == nil || !strings.Contains(wake[3], "--match-head-commit "+loopFixed) {
		t.Errorf("no go for the fixes: %v", r.calls)
	}
}

// A side effect whose completion line was lost is not repeated: its claim
// and the line its command logged settle it.
func TestLoopDoesNotRepeatAnActionWhoseRecordWasLost(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.failRecord = func(e state.Event) bool { return e.Verb == "supervisor.go" }
	if err := r.passErr(); err == nil {
		t.Fatal("the lost record was not an error")
	}
	if r.called(agentsName, agentWakeName) == nil {
		t.Fatalf("no wake: %v", r.calls)
	}
	r.pass(t)
	if c := r.called(agentsName, agentWakeName); c != nil {
		t.Errorf("the go was sent again after the restart: %v", c)
	}
	if evs := r.logged(t, "supervisor.go"); len(evs) != 1 || !strings.Contains(evs[0].Detail, "before an interruption") {
		t.Errorf("the go logged as %+v", evs)
	}
}

// A claim whose command left no line is taken after the restart.
func TestLoopTakesAClaimedActionItNeverRan(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	if err := r.a.store.Record(event(loopParty, loopClaimVerb, "[go %s@%s] %s", loopPR, loopHead, loopWorker)); err != nil {
		t.Fatal(err)
	}
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.pass(t)
	if c := r.called(agentsName, agentWakeName, loopWorker); c == nil {
		t.Errorf("the claimed go never ran: %v", r.calls)
	}
}

func TestLoopRefusesASecondLoopOnTheState(t *testing.T) {
	r := newLoopRig(t)
	other := flock.New(filepath.Join(r.a.store.Dir(), loopLockFile))
	if ok, err := other.TryLock(); !ok || err != nil {
		t.Fatal(ok, err)
	}
	defer func() { _ = other.Unlock() }()
	if err := r.passErr(); Code(err) != ExitRefused {
		t.Errorf("a second loop = %v", err)
	}
}

// A post-install that failed after the install is retried and the release
// finished, though --check finds nothing newer by then.
func TestLoopResumesAReleaseWhosePostInstallFailed(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Supervisor.PostInstall = []string{loopPostInstall}
	installed := false
	r.answer = func(args []string) (string, int) {
		if slices.Equal(args, selfUpdateCheck) && !installed {
			return "v2.0.0 is newer than v1.0.0", ExitOutdated
		}
		if slices.Equal(args, []string{selfUpdateCmdName}) {
			installed = true
		}
		return "", 0
	}
	r.command = func([]string) (string, int) { return "", 1 }
	r.pass(t)
	if !installed || len(r.logged(t, "supervisor.release")) != 0 {
		t.Fatalf("installed %v, logged %v", installed, r.logged(t, "supervisor.release"))
	}
	r.command = func([]string) (string, int) { return "", 0 }
	r.pass(t)
	if c := r.called(selfUpdateCmdName); len(c) == 1 {
		t.Errorf("installed again: %v", r.calls)
	}
	if evs := r.logged(t, "supervisor.release"); len(evs) != 1 || len(r.cmds) != 2 {
		t.Errorf("release %+v after %d post-install runs", evs, len(r.cmds))
	}
	r.pass(t)
	if len(r.cmds) != 2 {
		t.Errorf("post-install ran again: %v", r.cmds)
	}
}

// The release is finished only once the broker runs the installed binary.
func TestLoopReleaseWaitsForTheBroker(t *testing.T) {
	r := newLoopRig(t)
	installed := false
	r.answer = func(args []string) (string, int) {
		if slices.Equal(args, selfUpdateCheck) && !installed {
			return "v2.0.0 is newer than v1.0.0", ExitOutdated
		}
		if slices.Equal(args, []string{selfUpdateCmdName}) {
			installed = true
		}
		return "", 0
	}
	r.broker = func() (string, error) { return "the broker serves 1 call(s)", errLoopLater }
	r.pass(t)
	r.pass(t)
	if evs := r.logged(t, "supervisor.release"); len(evs) != 0 {
		t.Fatalf("finished with the broker's restart due: %+v", evs)
	}
	r.broker = func() (string, error) { return "the broker restarted", nil }
	r.pass(t)
	if evs := r.logged(t, "supervisor.release"); len(evs) != 1 || !strings.Contains(evs[0].Detail, "the broker restarted") {
		t.Errorf("release %+v", evs)
	}
}

// A loop whose binary was replaced runs on the installed one.
func TestLoopReexecutesAReplacedBinary(t *testing.T) {
	r := newLoopRig(t)
	r.pass(t)
	if r.execs != 0 {
		t.Fatal("re-executed an unreplaced binary")
	}
	r.replaced = true
	r.pass(t)
	if r.execs != 1 {
		t.Errorf("re-executions %d", r.execs)
	}
}

func TestLoopStartsTheNextWorkerOnCapacitysChoice(t *testing.T) {
	r := newLoopRig(t)
	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("Work {{.URL}}.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.a.cfg.Supervisor.WorkerBrief = brief
	r.a.cfg.Capacity.Choices = []config.Choice{{Harness: config.OmpHarness, Model: loopQwen}}
	r.pass(t)
	if err := r.a.store.Record(event(state.Party{Name: loopWorker}, superloop.VerbDone, "%s done: fix it", loopWorker)); err != nil {
		t.Fatal(err)
	}
	r.answer = func(args []string) (string, int) {
		if args[0] == loopBoard {
			return `{"pick":{"ref":"o/r#9","url":"https://github.com/o/r/issues/9"}}`, 0
		}
		return "", 0
	}
	r.pass(t)
	r.pass(t)
	start := r.called(agentsName, agentStartName, "r 9")
	if start == nil || !slices.Contains(start, harnessAuto) {
		t.Errorf("the next start = %v", start)
	}
}

// A question asked before the loop's first start is the guide's too.
func TestLoopRoutesAQuestionAskedBeforeItStarted(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, "", time.Now())
	err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.NextNote++
		st.Notes = append(st.Notes, state.Note{ID: st.NextNote, For: supervisorName, Text: "which lab?", By: state.Party{Name: loopWorker}, At: time.Now().Add(-time.Hour)})
		return nil, nil
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
}

// The classifier's answer is its stdout; what it says on stderr is no
// part of it.
func TestLoopReadsTheClassifiersStdoutAlone(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Supervisor.Classifier = []string{loopClassifier}
	r.command = func([]string) (string, int) { return `{"class":"failed","confidence":0.95}`, 0 }
	r.stderr = "model loaded in 2s\n"
	r.park(t, "", time.Now())
	r.pass(t)
	if err := r.a.store.Record(event(state.Party{Name: loopWorker}, superloop.VerbProblem, "%s: the lab is gone", loopWorker)); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	if note := r.called("note", loopAdd); note == nil || !strings.Contains(note[len(note)-1], "(failed)") {
		t.Errorf("the note = %v\n%s", note, r.out)
	}
}

func TestTheDefaultReviewBriefReportsWithAProblemLine(t *testing.T) {
	if !strings.Contains(defaultReviewBrief, "--problem none") {
		t.Errorf("the brief's report lacks --problem: %s", defaultReviewBrief)
	}
}

// The loop, with no supervisor session or watch, takes the same timer
// transitions as watch and delivers the wake only after saving them.
func TestSupervisorLoopFiresTimersWithoutSupervisor(t *testing.T) {
	const probeTimer = "probe"
	for _, kind := range []string{"wall-clock", probeTimer} {
		t.Run(kind, func(t *testing.T) {
			r := newLoopRig(t)
			ready := false
			checks := 0
			fakeRead(t, func(string) ([]byte, error) {
				checks++
				if !ready {
					return nil, errors.New("not ready")
				}
				return nil, nil
			})
			wakes := 0
			was := wakeOwner
			wakeOwner = func(_ *app, _ context.Context, _ state.Party, who, msg, _ string) error {
				wakes++
				st, err := r.a.store.Read()
				if err != nil || len(st.Timers) != 0 {
					t.Errorf("wake before timer saved: %+v, %v", st, err)
				}
				if who != loopWorker || !strings.Contains(msg, "timer #1") {
					t.Errorf("wake %q: %s", who, msg)
				}
				return nil
			}
			t.Cleanup(func() { wakeOwner = was })
			timer := state.Timer{ID: 1, Due: time.Now().Add(-time.Minute), Wake: loopWorker, What: "check result", Every: time.Nanosecond}
			if kind == probeTimer {
				timer.Probe = probeHolds
			}
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Timers = []state.Timer{timer}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			if kind == probeTimer {
				r.pass(t)
				st, _ := r.a.store.Read()
				if checks != 1 || wakes != 0 || len(st.Timers) != 1 || st.Timers[0].Reason != "not ready" {
					t.Fatalf("pending probe: checks %d, wakes %d, timers %+v", checks, wakes, st.Timers)
				}
			}
			ready = true
			r.pass(t)
			if wakes != 1 {
				t.Fatalf("due %s timer: got %d wakes, want 1", kind, wakes)
			}
			// A new app and store reopen the persisted state after a restart.
			reopened, err := state.Open(r.a.store.Dir())
			if err != nil {
				t.Fatal(err)
			}
			restarted := *r.a
			restarted.store = reopened
			if err := restarted.runSupervisorLoop(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			if wakes != 1 || len(r.logged(t, "timer.fired")) != 1 {
				t.Fatalf("timer fired again: %d wakes", wakes)
			}
		})
	}
}

// Line-only timers belong to the supervisor's watch, including its desktop
// notification. The loop must neither check nor consume them.
func TestSupervisorLoopLeavesLineOnlyTimersForWatch(t *testing.T) {
	for _, conditional := range []bool{false, true} {
		t.Run(fmt.Sprintf("conditional=%t", conditional), func(t *testing.T) {
			r := newLoopRig(t)
			checks, wakes := 0, 0
			wasWake := wakeOwner
			wakeOwner = func(_ *app, _ context.Context, _ state.Party, _, _, _ string) error {
				wakes++
				return nil
			}
			t.Cleanup(func() { wakeOwner = wasWake })
			fakeRead(t, func(string) ([]byte, error) { checks++; return nil, nil })
			timer := state.Timer{ID: 1, Due: time.Now().Add(-time.Minute), What: "look at result"}
			if conditional {
				timer.Probe = probeHolds
			}
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Timers = []state.Timer{timer, {ID: 2, Due: timer.Due, Wake: loopWorker, What: "wake worker"}}
				st.Supervisor = &state.Supervisor{Party: four, Since: time.Now().Add(-time.Hour)}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			r.pass(t)
			st, err := r.a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if checks != 0 || wakes != 1 || len(st.Timers) != 1 || !st.Timers[0].Fired.IsZero() || !st.Timers[0].Checked.IsZero() {
				t.Fatalf("loop took watch-only timer: checks %d, wakes %d, timers %+v", checks, wakes, st.Timers)
			}
			if !conditional {
				lines, evs := firePending(st, nil, r.a.now)
				if len(lines) != 1 || len(evs) != 1 || evs[0].Verb != "timer.due" {
					t.Fatalf("firePending lost plain timer after loop: %v, %+v", lines, evs)
				}
			}
			w, desktop, out := notifyingWatch(t, r.a.store.Dir(), false)
			w.now = r.a.now
			live := []*claude.Session{{ID: "s4", HostID: hostFour, Name: agentFour}}
			w.pending(context.Background(), live)
			w.timerActs.Wait()
			if !strings.Contains(out.String(), "TIMER:") || len(desktop.sent) != 1 || !strings.Contains(desktop.sent[0].Summary, fmt.Sprintf("timer #%d", timer.ID)) {
				t.Fatalf("watch lost timer line or notification: %s; %+v", out, desktop.sent)
			}
		})
	}
}
