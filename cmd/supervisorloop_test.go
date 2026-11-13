package cmd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	loopOverlapWait        = "overlapping PR"
	loopWaitingIssueWorker = "waiting issue worker"
	loopOverlapSession     = "overlap-session"
	loopClosingIssue       = "o/r#70"
	loopUnblock            = "unblock"
	loopPR                 = "o/r#7"
	loopURL                = "https://github.com/o/r/pull/7"
	loopHead               = "1111111111111111111111111111111111111111"
	loopFixed              = "2222222222222222222222222222222222222222"
	loopWorker             = "worker one"
	loopSecondWorker       = "worker two"
	loopPerson             = "pat"
	// loopLocal is a self-hosted provider.
	loopLocal = "local"
	// loopQwen is a self-hosted model, loopPostInstall a post-install
	// command, loopClassifier a classifier.
	loopQwen        = "qwen"
	loopPostInstall = "restart-things"
	loopClassifier  = "classify"
	loopNewRelease  = "v2.0.0 is newer than v1.0.0"
	// loopDesk is the login the desk opens its pull requests as, loopTeamB
	// another team, loopReviewer the reviewer of the rig's first round,
	// superloop.ReviewerName(loopPR, 2) of its second round.
	loopDesk     = "desk"
	loopTeamB    = "team-b"
	loopReviewer = "Review r 7 r1"
	// loopApproved is a desk review's approving body, loopIssueTask the
	// task of a worker on the issue the rig's pull request closes.
	loopApproved  = "Review: approved"
	loopIssueTask = "Work https://github.com/o/r/issues/10"
)

// loopRig is a loop's app over a scratch store, with the beekeeper commands
// and the pull request reads it makes faked.
type loopRig struct {
	mu    sync.Mutex
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
	lastMerged string
	replaced   bool
	execs      int
	// broker answers the broker stage of a release.
	broker func() (string, error)
	// failRecord fails the store's Record of the first event it accepts.
	failRecord func(state.Event) bool
	// inTurn says a session runs a turn; sessions are the running CLIs.
	inTurn   func(state.Party) bool
	sessions []*claude.Session
	// pulls are further pull requests by ref; teams the team files'
	// answer, teamReads how often they were read.
	pulls     map[string]superloop.Pull
	teams     map[string]string
	teamsErr  error
	teamReads int
	// baseMerge answers whether a head only merges its base into the
	// approved one; baseReads are the pairs asked, as approved..head.
	baseMerge func(p superloop.Pull, approved string) (bool, error)
	baseReads []string
}

// loopFakeBinary is the loop's binary as the rig says it.
type loopFakeBinary struct{ r *loopRig }

func (b loopFakeBinary) Replaced() bool {
	b.r.mu.Lock()
	defer b.r.mu.Unlock()
	return b.r.replaced
}

func (b loopFakeBinary) Exec(...string) error {
	b.r.mu.Lock()
	defer b.r.mu.Unlock()
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
	s.r.mu.Lock()
	f := s.r.failRecord
	if f != nil && slices.ContainsFunc(events, f) {
		s.r.failRecord = nil
		s.r.mu.Unlock()
		return errors.New("disk full")
	}
	s.r.mu.Unlock()
	return s.Store.Record(events...)
}

func (s rigStore) Update(fn func(*state.State) ([]state.Event, error)) error {
	return s.Store.Update(func(st *state.State) ([]state.Event, error) {
		evs, err := fn(st)
		s.r.mu.Lock()
		if err == nil && s.r.failRecord != nil && slices.ContainsFunc(evs, s.r.failRecord) {
			s.r.failRecord = nil
			s.r.mu.Unlock()
			return nil, errors.New("disk full")
		}
		s.r.mu.Unlock()
		return evs, err
	})
}

// logs is the line the real command logs as the loop for args, which a
// claim's reconciliation reads.
func logs(args []string) (state.Event, bool) {
	if len(args) < 3 {
		return state.Event{}, false
	}
	switch {
	case args[0] == agentsName && args[1] == agentStartName:
		return event(loop.LoopParty, "agents.start", "%s: background session x", args[2]), true
	case args[0] == agentsName && args[1] == agentWakeName:
		return event(loop.LoopParty, "agents.wake", "%s: sent by name", args[2]), true
	case args[0] == agentsName && args[1] == loop.LoopRemove:
		return event(loop.LoopParty, "agents.remove", "%s", args[2]), true
	case args[0] == paramNote && args[1] == loop.LoopAdd:
		return event(loop.LoopParty, "note.add", "#1 %s", args[len(args)-1]), true
	case args[0] == loop.LoopBoard && args[1] == loopBoardMove:
		return event(loop.LoopParty, "board.move", "%s: → %s", args[2], args[3]), true
	case args[0] == loop.LoopBoard && args[1] == "rank":
		return event(loop.LoopParty, "board.rank", "%s: %s %s", args[2], args[3], args[len(args)-1]), true
	}
	return state.Event{}, false
}

func newLoopRig(t *testing.T) *loopRig {
	t.Helper()
	oldDeliveryRelease := loop.ReadDeliveryRelease
	t.Cleanup(func() { loop.ReadDeliveryRelease = oldDeliveryRelease })
	loop.ReadDeliveryRelease = func(context.Context, superloop.Pull) (string, string, error) { return "", "", nil }
	oldParents := reviewParents
	t.Cleanup(func() { reviewParents = oldParents })
	reviewParents = func(context.Context, []string) ([]string, error) { return nil, nil }
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
	previousRefs := refStates
	t.Cleanup(func() { refStates = previousRefs })
	refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
		out := map[github.PR]github.RefState{}
		for _, ref := range refs {
			out[ref] = github.RefState{Kind: github.RefPullRequest, State: github.Open}
		}
		return out, nil
	}
	oldAlerts, oldResume := readLoopAlerts, loopResumable
	t.Cleanup(func() { readLoopAlerts, loopResumable = oldAlerts, oldResume })
	readLoopAlerts = func(context.Context, *app) ([]alerts.Firing, error) { return nil, nil }
	loopResumable = func(*app, *state.State, state.Agent) bool { return true }
	r := &loopRig{out: &out}
	prevCheck, prevQueue := reviewedMergeCheck, readReviewedMergeQueue
	t.Cleanup(func() { reviewedMergeCheck, readReviewedMergeQueue = prevCheck, prevQueue })
	reviewedMergeCheck = func(context.Context, *app, superloop.Pull, string) (bool, string, int) { return false, "", 0 }
	readReviewedMergeQueue = func(context.Context, superloop.Pull) (bool, error) { return true, nil }
	wasBehind := readReviewedBehind
	oldRevision := loop.ReadRepoPullRevision
	t.Cleanup(func() { loop.ReadRepoPullRevision = oldRevision })
	loop.ReadRepoPullRevision = func(context.Context, string, string) (string, bool, error) { return "", false, nil }
	oldCloseIssue, oldEditBody, oldOpenPulls := loop.ClosePullIssue, loop.EditPullBody, readRepoOpenPulls
	t.Cleanup(func() {
		readReviewedBehind = wasBehind
		loop.ClosePullIssue, loop.EditPullBody, readRepoOpenPulls = oldCloseIssue, oldEditBody, oldOpenPulls
	})
	readReviewedBehind = func(context.Context, superloop.Pull) (int, error) { return 0, nil }
	loop.ClosePullIssue = func(context.Context, github.PR, string) error { return nil }
	loop.EditPullBody = func(context.Context, github.PR, string) error { return nil }
	readRepoOpenPulls = func(context.Context, string) ([]unmergedPull, error) { return nil, nil }
	r.a = &app{store: rigStore{Store: store, r: r}, out: &out, now: time.Now(), cfg: cfg}
	oldBudget := readLoopGraphQLBudget
	t.Cleanup(func() { readLoopGraphQLBudget = oldBudget })
	readLoopGraphQLBudget = func(a *app, _ context.Context) (github.Budget, error) {
		st, err := a.store.Read()
		if err == nil && st.Budget == nil && a.cfg.GitHub.Floor <= 0 {
			return github.Budget{Remaining: 4900, Limit: 5000, Reset: a.timeNow().Add(time.Hour)}, nil
		}
		if err != nil || st.Budget == nil {
			return github.Budget{}, err
		}
		return github.Budget{Remaining: st.Budget.Remaining, Limit: st.Budget.Limit, Reset: st.Budget.Reset, GraphQL: graphqlOf(st.Budget.GraphQL)}, nil
	}
	if err := store.Record(event(watchParty, "watch.budget-state", "false")); err != nil {
		t.Fatal(err)
	}
	// The loop first started a minute ago: the rig's parks are requests
	// for its review, not parks from before it.
	first := event(loop.LoopParty, loop.LoopStartVerb, "started")
	first.At = first.At.Add(-time.Minute)
	if err := store.Record(first); err != nil {
		t.Fatal(err)
	}
	r.answer = func([]string) (string, int) { return "", 0 }
	r.command = func(argv []string) (string, int) {
		if len(argv) >= 4 && slices.Equal(argv[:4], strings.Fields(devctlBinary+" auth exec --")) {
			argv = argv[4:]
		}
		if len(argv) > 2 && argv[0] == "gh" && argv[1] == "pr" && argv[2] == superloop.KindMerge {
			r.mu.Lock()
			repo := r.pullRepo()
			if idx := slices.Index(argv, "--repo"); idx >= 0 && idx+1 < len(argv) {
				repo = argv[idx+1]
			}
			ref := repo + "#" + argv[3]
			if p, ok := r.pulls[ref]; ok {
				p.State = superloop.Merged
				p.MergeCommit = loopFixed
				r.pulls[ref] = p
			} else {
				r.pull.State = superloop.Merged
				r.pull.MergeCommit = loopFixed
				r.lastMerged = r.pull.Head
			}
			r.mu.Unlock()
		}
		return "", 0
	}
	r.pull = superloop.Pull{Ref: loopPR, URL: loopURL, State: superloop.Open, Head: loopHead, Green: true, Author: loopDesk}
	r.pulls = map[string]superloop.Pull{}
	r.broker = func() (string, error) { return "no broker runs", nil }
	r.inTurn = func(state.Party) bool { return false }
	r.baseMerge = func(superloop.Pull, string) (bool, error) { return false, nil }
	observation, sweep, watch, pollOnce := loop.ReadObservedLoopPull, loop.ReadLoopSweep, loop.StartLoopPullWatcher, loop.PollOncePullWatcher
	t.Cleanup(func() {
		loop.ReadObservedLoopPull, loop.ReadLoopSweep, loop.StartLoopPullWatcher, loop.PollOncePullWatcher = observation, sweep, watch, pollOnce
	})
	loop.StartLoopPullWatcher = func(context.Context, *loop.Loop) func() { return func() {} }
	loop.PollOncePullWatcher = func(context.Context, *loop.Loop) {}
	loop.ReadLoopSweep = fixtureLoopSweep
	loop.ReadObservedLoopPull = fixtureLoopObservation
	run, read, command, binary, broker, turn, sessions, login, teams, baseMerge := loop.BeekeeperRun, loop.ReadLoopPull, loop.LoopCommand, loop.LoopBinary, loopBroker, loopInTurn, loopSessions, loop.ReadDeskLogin, loop.ReadTeams, loop.ReadBaseMerge
	t.Cleanup(func() {
		loop.BeekeeperRun, loop.ReadLoopPull, loop.LoopCommand, loop.LoopBinary, loopBroker, loopInTurn, loopSessions, loop.ReadDeskLogin, loop.ReadTeams, loop.ReadBaseMerge = run, read, command, binary, broker, turn, sessions, login, teams, baseMerge
	})
	loop.ReadBaseMerge = func(_ context.Context, p superloop.Pull, approved string) (bool, error) {
		r.mu.Lock()
		r.baseReads = append(r.baseReads, approved+".."+p.Head)
		r.mu.Unlock()
		return r.baseMerge(p, approved)
	}
	loop.ReadDeskLogin = func(context.Context) (string, error) { return loopDesk, nil }
	loop.ReadTeams = func(context.Context, config.Ownership) (map[string]string, error) {
		r.mu.Lock()
		r.teamReads++
		r.mu.Unlock()
		return r.teams, r.teamsErr
	}
	loopInTurn = func(a *app, p state.Party) bool { return r.inTurn(p) || a.executionInTurn(p) }
	loopSessions = func(*app) ([]*claude.Session, *proc.Table, error) { return r.sessions, &proc.Table{}, nil }
	loop.LoopBinary = func() loop.Replaceable { return loopFakeBinary{r} }
	loopBroker = func(context.Context) (string, error) { return r.broker() }
	loop.BeekeeperRun = func(_ context.Context, args ...string) (string, int, error) {
		if len(args) >= 2 && args[0] == "--as" {
			args = args[2:]
		}
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		o, code := r.answer(args)
		if len(args) > 0 && args[0] == gateCmdName && code == 0 && o == "" {
			idx := slices.Index(args, "--reviewed-head")
			if idx >= 0 && idx+1 < len(args) {
				repo := r.pullRepo()
				pr := 7
				if sep := slices.Index(args, "--"); sep >= 0 && sep+5 < len(args) {
					repo = args[sep+4]
					if n, err := strconv.Atoi(args[sep+5]); err == nil && n > 0 {
						pr = n
					}
				}
				doc := r.a.reviewedMerge(context.Background(), repo, pr, args[idx+1])
				code = doc.ExitCode
				b, _ := json.Marshal(doc)
				o = string(b)
			}
		}
		if e, ok := logs(args); ok && code == 0 && !strings.HasPrefix(o, "left ") {
			if err := store.Record(e); err != nil {
				t.Fatal(err)
			}
		}
		return o, code, nil
	}
	loop.ReadLoopPull = func(_ context.Context, ref string) (superloop.Pull, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if p, ok := r.pulls[ref]; ok {
			return p, nil
		}
		if ref != loopPR {
			t.Fatalf("read %s", ref)
		}
		return r.pull, nil
	}
	loop.LoopCommand = func(_ context.Context, stdin string, argv []string) (string, string, int, error) {
		r.mu.Lock()
		r.cmds = append(r.cmds, argv)
		r.stdin = append(r.stdin, stdin)
		r.mu.Unlock()
		o, code := r.command(argv)
		return o, r.stderr, code, nil
	}
	return r
}

// park puts the worker on the roster, parked on on at.
func (r *loopRig) park(t *testing.T, on string, at time.Time) {
	t.Helper()
	if r.lastMerged != "" && r.pull.Head != r.lastMerged && r.pull.State == superloop.Merged {
		r.pull.State = superloop.Open
	}
	err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == loopWorker })
		if i < 0 {
			st.Agents = append(st.Agents, state.Agent{Party: state.Party{Name: loopWorker}, Task: "fix " + loopURL})
			i = len(st.Agents) - 1
		}
		// A park verifies the head the worker published: the pull
		// request's head now.
		st.Agents[i].Park = &state.Park{On: on, At: at, Waits: deskReviewWait, ReviewHead: cmp.Or(r.pull.Head, loopHead)}
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
	r.mu.Lock()
	r.calls = nil
	r.mu.Unlock()
	return r.a.runSupervisorLoop(context.Background(), true)
}

// verdict has the round's reviewer post its verdict on head now.
func (r *loopRig) verdict(id, head, verdict, body string) {
	r.pull.Reviews = append(r.pull.Reviews, superloop.Review{ID: id, Author: loopDesk, Head: head, Verdict: verdict, Body: body, At: time.Now()})
}

// called is the call whose line starts with prefix.
func (r *loopRig) called(prefix ...string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if len(c) >= len(prefix) && slices.Equal(c[:len(prefix)], prefix) {
			return c
		}
	}
	return nil
}

func (r *loopRig) pullRepo() string { repo, _, _ := strings.Cut(r.pull.Ref, "#"); return repo }

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
	if start == nil || start[2] != loopReviewer || !slices.Contains(start, "--review") {
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
	wake := r.called(gateCmdName)
	if wake == nil || !slices.Contains(wake, loopHead) {
		t.Fatalf("no merge go: %v\n%s", r.calls, r.out)
	}
	if evs := r.logged(t, "supervisor.merge"); len(evs) != 1 || !strings.Contains(evs[0].Detail, "[go "+loopPR+"@"+loopHead+"]") {
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

// https://github.com/giantswarm/beekeeper/issues/1795: findings that stand
// with no review round left go back to the worker; the person gets no merge
// question.
func TestLoopForwardsChangesThenReworksUnfixedFindings(t *testing.T) {
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
	// The worker parks again without a new head: back to it to rework.
	r.park(t, loopPR, time.Now().Add(time.Minute))
	r.pass(t)
	if note := r.called("note", loop.LoopAdd); note != nil {
		t.Fatalf("used-up review rounds reached the guide's person: %v", note)
	}
	wake = r.called(agentsName, agentWakeName, loopWorker)
	if wake == nil || !strings.Contains(wake[3], "fix every finding") || !strings.Contains(wake[3], loopURL) {
		t.Fatalf("no rework told: %v\n%s", r.calls, r.out)
	}
	if evs := r.logged(t, "supervisor.rework"); len(evs) != 1 {
		t.Errorf("rework logged as %+v", evs)
	}
	if evs := r.logged(t, "supervisor.escalate"); len(evs) != 0 {
		t.Errorf("escalated: %+v", evs)
	}
	r.pass(t)
	if evs := r.logged(t, "supervisor.rework"); len(evs) != 1 {
		t.Errorf("reworked twice for one park: %+v", evs)
	}
	// The fixes pushed and parked on: the merge go for the new head.
	r.pull.Head = loopFixed
	r.park(t, loopPR, time.Now().Add(2*time.Minute))
	r.pass(t)
	wake = r.called(gateCmdName)
	if wake == nil || !slices.Contains(wake, loopFixed) {
		t.Fatalf("no merge go for the fixed head: %v\n%s", r.calls, r.out)
	}
}

// An answer to a note the loop filed is the loop's to read from the log:
// no session is woken for it.
func TestAnAnswerToTheLoopsNoteWakesNoSession(t *testing.T) {
	r := newLoopRig(t)
	got := r.a.tellFiler(context.Background(), state.Party{Name: loopPerson}, &state.Note{ID: 3, By: loop.LoopParty}, "NOTE ANSWERED #3 by pat: Merge")
	if !strings.Contains(got, "the supervisor loop acts on the answer") || strings.Contains(got, "failed") {
		t.Errorf("tellFiler = %q", got)
	}
}

// A new head after an approval with a change of its own gets the second
// review, whatever supervisor.reviewRounds says; past it the loop merges
// the reviewed, green head itself, and no note asks the person whether to
// merge (https://github.com/giantswarm/beekeeper/issues/2207).
func TestLoopReviewsANewHeadAfterAnApproval(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.pass(t)
	if r.called(gateCmdName) == nil {
		t.Fatalf("no merge go for the approved head: %v", r.calls)
	}
	// The worker pushes one more commit and parks again.
	r.pull.Head = loopFixed
	r.park(t, loopPR, time.Now().Add(time.Minute))
	r.pass(t)
	if note := r.called("note", loop.LoopAdd); note != nil {
		t.Fatalf("a new head after an approval escalated: %v", note)
	}
	start := r.called(agentsName, agentStartName)
	if start == nil || start[2] != superloop.ReviewerName(loopPR, 2) {
		t.Fatalf("no review of the new head: %v\n%s", r.calls, r.out)
	}
	if evs := r.logged(t, "supervisor.review"); len(evs) != 2 || !strings.Contains(evs[1].Detail, loopFixed) {
		t.Errorf("review logged as %+v", evs)
	}
	// The second review approves, and the worker pushes a change of its
	// own once more: no third reviewer and no question, the loop merges.
	r.verdict("R2", loopFixed, superloop.Approved, "")
	r.pass(t)
	const own = "3333333333333333333333333333333333333333"
	r.pull.Head = own
	r.park(t, loopPR, time.Now().Add(2*time.Minute))
	r.calls = nil
	r.pass(t)
	if start := r.called(agentsName, agentStartName); start != nil {
		t.Fatalf("a third review started: %v", start)
	}
	if merge := r.called(gateCmdName); merge == nil || !slices.Contains(merge, own) {
		t.Fatalf("the loop did not merge the reviewed, green head %s: %v\n%s", own, r.calls, r.out)
	}
	r.pass(t)
	if note := r.called("note", loop.LoopAdd); note != nil {
		t.Errorf("a note asked the person: %v", note)
	}
}

// https://github.com/giantswarm/beekeeper/issues/1841: the gate's
// --update-branch moved the approved head on by a merge of the base; the
// worker parked on it gets the go without a second review, naming the
// gate's command. A head with more than the base merge gets its review.
func TestLoopGivesABaseMergeOfAnApprovedHeadTheGo(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Merge.DevctlOwners = []string{"o"}
	r.pull.Base = mainBranch
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.pass(t)
	wake := r.called(gateCmdName)
	if wake == nil || !slices.Contains(wake, "--reviewed-head") || !slices.Contains(wake, loopHead) {
		t.Fatalf("no gated merge go: %v\n%s", r.calls, r.out)
	}
	r.pull.Head = loopFixed
	r.baseMerge = func(p superloop.Pull, approved string) (bool, error) {
		return p.Base == mainBranch && approved == loopHead && p.Head == loopFixed, nil
	}
	r.park(t, loopPR, time.Now().Add(time.Minute))
	r.pass(t)
	if c := r.called(agentsName, agentStartName); c != nil {
		t.Errorf("a base merge started a review: %v", c)
	}
	wake = r.called(gateCmdName)
	if wake == nil || !slices.Contains(wake, loopFixed) {
		t.Fatalf("no go for the base merge: %v\n%s", r.calls, r.out)
	}
	if evs := r.logged(t, "supervisor.merge"); len(evs) != 2 || !strings.Contains(evs[1].Detail, "[go "+loopPR+"@"+loopFixed+"]") {
		t.Errorf("the go logged as %+v", evs)
	}
	// The planner compares the base once; the executor receives the loop's
	// separate exact-head authorization, so it needs no second comparison.
	// An ownership claim alone is still refused by the authority tests.
	if !slices.Equal(r.baseReads, []string{loopHead + ".." + loopFixed}) {
		t.Errorf("base merge reads = %v", r.baseReads)
	}
	// A further head with a change of its own: its review, no go.
	const own = "3333333333333333333333333333333333333333"
	r.pull.Head = own
	r.baseMerge = func(superloop.Pull, string) (bool, error) { return false, nil }
	r.park(t, loopPR, time.Now().Add(2*time.Minute))
	r.pass(t)
	if c := r.called(agentsName, agentWakeName); c != nil {
		t.Errorf("a head with its own change got %v", c)
	}
	if start := r.called(agentsName, agentStartName); start == nil {
		t.Fatalf("no review of a head with its own change: %v\n%s", r.calls, r.out)
	}
	// A repository in merge.admin gets --admin --match-head-commit in its go.
	r.a.cfg.Merge.Admin = []string{"O/R"}
	r.verdict("R2", own, superloop.Approved, "")
	r.pass(t)
	wake = r.called(gateCmdName)
	if wake == nil || !slices.Contains(wake, own) {
		t.Fatalf("no admin merge go: %v\n%s", r.calls, r.out)
	}
}

// A head that only merges main into the head a review asked changes on is
// no fix: the worker reworks, and no go comes.
func TestLoopGivesNoGoToABaseMergeOfFindings(t *testing.T) {
	r := newLoopRig(t)
	r.pull.Base = mainBranch
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.ChangesRequested, "Review: changes requested\n- cmd/x.go:3: wrong")
	r.pass(t)
	if r.called(agentsName, agentWakeName, loopWorker) == nil {
		t.Fatalf("the findings were not forwarded: %v", r.calls)
	}
	r.pull.Head = loopFixed
	r.baseMerge = func(p superloop.Pull, approved string) (bool, error) {
		return approved == loopHead && p.Head == loopFixed, nil
	}
	r.park(t, loopPR, time.Now().Add(time.Minute))
	r.pass(t)
	if evs := r.logged(t, "supervisor.merge"); len(evs) != 0 {
		t.Fatalf("a base merge of unfixed findings got the go: %+v", evs)
	}
	wake := r.called(agentsName, agentWakeName, loopWorker)
	if wake == nil || !strings.Contains(wake[3], "only brings main into head") || !strings.Contains(wake[3], "stand unfixed") {
		t.Fatalf("no rework for the base merge: %v\n%s", r.calls, r.out)
	}
}

// A comparison GitHub does not answer is no base merge: the head gets its
// review, and the failure is said.
func TestLoopReviewsAHeadItCannotCompare(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.pass(t)
	r.pull.Head = loopFixed
	r.baseMerge = func(superloop.Pull, string) (bool, error) { return false, errors.New("gh: 502") }
	r.park(t, loopPR, time.Now().Add(time.Minute))
	r.pass(t)
	if start := r.called(agentsName, agentStartName); start == nil || !strings.Contains(r.out.String(), "cannot tell whether "+loopPR) {
		t.Fatalf("no review after a failed comparison: %v\n%s", r.calls, r.out)
	}
}

// After an approval and a second round's findings, the green fixes receive
// the loop's merge go without a third round or a question to the person.
func TestLoopMergesFixesAfterExhaustedRounds(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.pass(t)
	r.pull.Head = loopFixed
	r.park(t, loopPR, time.Now().Add(time.Minute))
	r.pass(t)
	r.verdict("R2", loopFixed, superloop.ChangesRequested, "Review: changes requested")
	r.pass(t)
	const fixedAgain = "3333333333333333333333333333333333333333"
	r.pull.Head = fixedAgain
	r.park(t, loopPR, time.Now().Add(2*time.Minute))
	r.pass(t)
	if c := r.called(agentsName, agentStartName); c != nil {
		t.Fatalf("unexpected third review: %v", r.calls)
	}
	if c := r.called(gateCmdName); c == nil || !slices.Contains(c, fixedAgain) {
		t.Fatalf("fixed head got no go: %v", r.calls)
	}
	if c := r.called("note", loop.LoopAdd); c != nil {
		t.Fatalf("merge question reached the person: %v", c)
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
	wake := r.called(gateCmdName)
	if wake == nil || !slices.Contains(wake, loopFixed) {
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
	started := false
	r.answer = func(args []string) (string, int) {
		if args[0] == loop.LoopCapacity {
			if started {
				return loopNoRoomJSON, 0
			}
			return loopRoomJSON, 0
		}
		if len(args) > 1 && args[0] == agentsName && args[1] == agentStartName {
			started = true
		}
		if args[0] == loop.LoopBoard {
			return `{"pick":{"ref":"o/r#9","url":"https://github.com/o/r/issues/9","title":"next"}}`, 0
		}
		return "", 0
	}
	r.pass(t)
	if c := r.called(agentsName, loop.LoopRemove, loopWorker); c == nil {
		t.Fatalf("not taken off the roster: %v\n%s", r.calls, r.out)
	}
	if c := r.called("note", loop.LoopAdd); c != nil {
		t.Errorf("the final report went to the guide: %v", c)
	}
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

// sshLoopHost puts sshTestSelector in the rig's configuration as an
// execution host mapping sshTestRepository, and the loop's placement in
// supervisor.hosts; choices gives the desk a capacity.choices so a remote
// start has a model to name.
func (r *loopRig) sshLoopHost(t *testing.T, choices bool) {
	t.Helper()
	host := execution.Host{Endpoint: execution.Endpoint{SSH: sshTestSelector + ".example", Executable: sshTestExecutable, Config: sshTestConfig}, Identity: execution.HostID(sshTestSelector), WorkspaceRoot: "/srv/workspaces", Repositories: map[string]string{sshTestRepository: "/srv/source"}, Harnesses: []string{claudeComm}, PolicyDigest: execution.Digest([]byte("fixture policy")), MaxBusy: 2}
	r.a.cfg.Agents.Hosts = map[string]config.ExecutionHost{sshTestSelector: host}
	r.a.cfg.Supervisor.Hosts = []string{sshTestSelector}
	if choices {
		r.a.cfg.Capacity = config.Capacity{Choices: []config.Choice{{Harness: claudeComm, Model: bgModel}}}
	}
}

// The capacity --json answers the placement tests hand out.
const (
	loopRoomJSON   = `{"ceiling": 1, "room": 1}`
	loopNoRoomJSON = `{"ceiling": 1, "room": 0}`
	loopRoomJSON2  = `{"ceiling": 2, "room": 2}`
	loopAsFlagArg  = "--" + loop.AsFlag
)

// nextPickJSON is board next --json's answer naming the second item of the
// mapped repository.
const loopNextPickJSON = `{"pick":{"ref":"` + sshTestRepository + `#9","url":"https://github.com/` + sshTestRepository + `/issues/9","title":"next"}}`

// nextBrief writes the worker brief template into the rig.
func nextBrief(t *testing.T, r *loopRig) {
	t.Helper()
	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("Work {{.URL}}.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.a.cfg.Supervisor.WorkerBrief = brief
}

// loopNextDone records the done of worker one with a report, so the next
// transition plans.
func loopNextDone(t *testing.T, r *loopRig) {
	t.Helper()
	titled := state.Party{Name: "#1743: a session title"}
	done := event(titled, "agents.done", "%s done: fix it", loopWorker)
	report := event(titled, "agents.report", "%s: merged %s", loopWorker, loopURL)
	report.At = done.At.Add(10 * time.Microsecond)
	if err := r.a.store.Record(done, report); err != nil {
		t.Fatal(err)
	}
}

// splitAs drops the loop's own party arguments from a beekeeper line.
func splitAs(args []string) []string {
	if len(args) >= 2 && args[0] == loopAsFlagArg {
		return args[2:]
	}
	return args
}

// TestLoopStartsTheNextWorkerOnAnExecutionHost is the issue's case: the
// coordinator's machine has no room, a checked host has room and maps the
// board item's repository; the loop starts the worker there, on the
// repository's default branch, instead of waiting for the local machine.
func TestLoopStartsTheNextWorkerOnAnExecutionHost(t *testing.T) {
	r := newLoopRig(t)
	nextBrief(t, r)
	r.sshLoopHost(t, true)
	r.park(t, loopPR, time.Now())
	r.pull.State = superloop.Merged
	r.pass(t)
	loopNextDone(t, r)
	run, branch := loop.BeekeeperRun, loop.GhDefaultBranch
	t.Cleanup(func() { loop.BeekeeperRun, loop.GhDefaultBranch = run, branch })
	branchReads := 0
	loop.GhDefaultBranch = func(context.Context, string) (string, error) {
		branchReads++
		return mainBranch, nil
	}
	loop.BeekeeperRun = func(_ context.Context, args ...string) (string, int, error) {
		args = splitAs(args)
		r.calls = append(r.calls, args)
		switch args[0] {
		case loop.LoopCapacity:
			// The coordinator's machine is full; the host has room.
			if slices.Contains(args, loop.LoopHostFlag) {
				return loopRoomJSON, 0, nil
			}
			return loopNoRoomJSON, 0, nil
		case loop.LoopBoard:
			return loopNextPickJSON, 0, nil
		}
		if len(args) > 1 && args[0] == agentsName && args[1] == agentStartName {
			r.started(t, args[2], "remote-next-session")
		}
		if e, ok := logs(args); ok {
			if err := r.a.store.Record(e); err != nil {
				t.Fatal(err)
			}
		}
		return "", 0, nil
	}
	// The done removes the worker; the next starts on the host.
	r.pass(t)
	if c := r.called(agentsName, loop.LoopRemove, loopWorker); c == nil {
		t.Fatalf("not taken off the roster: %v\n%s", r.calls, r.out)
	}
	start := r.called(agentsName, agentStartName, loop.RepoName(sshTestRepository)+" 9")
	if start == nil {
		t.Fatalf("the next item did not start: %v\n%s", r.calls, r.out)
	}
	for _, want := range []string{loop.LoopHostFlag, sshTestSelector, loop.LoopRepoFlag, sshTestRepository, loop.LoopRefFlag, mainBranch} {
		if !slices.Contains(start, want) {
			t.Errorf("the remote start names no %q: %v", want, start)
		}
	}
	if !slices.Contains(start, "--harness") {
		t.Errorf("the placed start loses capacity's pick: %v", start)
	}
	if evs := r.logged(t, "supervisor.next"); !slices.ContainsFunc(evs, func(e state.Event) bool { return strings.Contains(e.Detail, "on "+sshTestSelector) }) {
		t.Errorf("the next logged as %+v", evs)
	}
	// A restarted loop takes the transition no second time.
	r.pass(t)
	if c := r.called(agentsName, agentStartName); c != nil {
		t.Errorf("a restart started %v again", c)
	}
	if branchReads != 1 {
		t.Errorf("the default branch read %d times", branchReads)
	}
}

// TestLoopKeepsTheNextWorkerLocalWithRoom says the host preference does
// not steal a start the coordinator's own machine still fits: local room
// without supervisor.hostsFirst keeps it local.
func TestLoopKeepsTheNextWorkerLocalWithRoom(t *testing.T) {
	r := newLoopRig(t)
	nextBrief(t, r)
	r.sshLoopHost(t, true)
	r.park(t, loopPR, time.Now())
	r.pull.State = superloop.Merged
	r.pass(t)
	loopNextDone(t, r)
	branch := loop.GhDefaultBranch
	t.Cleanup(func() { loop.GhDefaultBranch = branch })
	loop.GhDefaultBranch = func(context.Context, string) (string, error) { return mainBranch, nil }
	r.answer = func(args []string) (string, int) {
		if args[0] == loop.LoopCapacity {
			return loopRoomJSON2, 0
		}
		if args[0] == loop.LoopBoard {
			return loopNextPickJSON, 0
		}
		return "", 0
	}
	r.pass(t)
	start := r.called(agentsName, agentStartName, loop.RepoName(sshTestRepository)+" 9")
	if start == nil {
		t.Fatalf("the next item did not start: %v\n%s", r.calls, r.out)
	}
	if slices.Contains(start, loop.LoopHostFlag) {
		t.Errorf("a machine with room placed the start remotely: %v", start)
	}
}

// TestLoopWaitsWhenNoHostHasRoom is supervisor.hostsFirst with the
// coordinator full and the checked host full too: the loop waits instead
// of starting anywhere.
func TestLoopWaitsWhenNoHostHasRoom(t *testing.T) {
	r := newLoopRig(t)
	nextBrief(t, r)
	r.a.cfg.Supervisor.HostsFirst = true
	r.sshLoopHost(t, true)
	r.park(t, loopPR, time.Now())
	r.pull.State = superloop.Merged
	r.pass(t)
	loopNextDone(t, r)
	r.answer = func(args []string) (string, int) {
		if args[0] == loop.LoopCapacity {
			return loopNoRoomJSON, 0
		}
		if args[0] == loop.LoopBoard {
			return loopNextPickJSON, 0
		}
		return "", 0
	}
	r.pass(t)
	r.pass(t)
	if c := r.called(agentsName, agentStartName); c != nil {
		t.Errorf("a full desk and a full host started %v anyway", c)
	}
	if evs := r.logged(t, "supervisor.next"); slices.ContainsFunc(evs, func(e state.Event) bool { return strings.Contains(e.Detail, "started on") }) {
		t.Errorf("the wait logged as taken: %+v", evs)
	}
}

// Without supervisor host placement configured, reviewers use local capacity.
func TestLoopReviewersWithoutPlacementStayOnCoordinator(t *testing.T) {
	for _, first := range []bool{false, true} {
		for _, room := range []bool{false, true} {
			for _, selfHosted := range []bool{false, true} {
				t.Run(fmt.Sprintf("first=%t/room=%t/selfHosted=%t", first, room, selfHosted), func(t *testing.T) {
					r := newLoopRig(t)
					r.sshLoopHost(t, true)
					r.a.cfg.Supervisor.Hosts = nil
					r.a.cfg.Supervisor.HostsFirst = first
					r.answer = func(args []string) (string, int) {
						if args[0] == loop.LoopCapacity {
							if slices.Contains(args, loop.LoopHostFlag) {
								t.Fatal("review placement probed an execution host")
							}
							if !slices.Contains(args, "--review") {
								t.Fatal("review capacity lost its start kind")
							}
							if slices.Contains(args, "--"+reviewOfSelfHostedFlag) != selfHosted {
								t.Fatal("review capacity lost self-hosted provenance")
							}
							if room {
								return loopRoomJSON, 0
							}
							return loopNoRoomJSON, 0
						}
						return "", 0
					}
					act := superloop.Action{Ref: sshTestRepository + "#7", URL: "https://github.com/" + sshTestRepository + "/pull/7", Head: loopHead, Round: 1, Key: "local-review", SelfHosted: selfHosted}
					p := r.pull
					p.Ref, p.URL = act.Ref, act.URL
					r.pulls[act.Ref] = p
					_, err := (&loop.Loop{Host: r.a, Said: map[string]bool{}}).StartReviewer(context.Background(), act)
					if !room {
						if !errors.Is(err, loop.ErrLoopLater) {
							t.Fatalf("full coordinator: %v", err)
						}
						if start := r.called(agentsName, agentStartName); start != nil {
							t.Fatalf("full coordinator started %v", start)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					start := r.called(agentsName, agentStartName)
					if start == nil {
						t.Fatal("no local reviewer started")
					}
					for _, flag := range []string{loop.LoopHostFlag, loop.LoopRepoFlag, loop.LoopRefFlag, "--" + excludeFlag} {
						if slices.Contains(start, flag) {
							t.Errorf("local review has placement flag %s: %v", flag, start)
						}
					}
					if !slices.Contains(start, "--review") || !slices.Contains(start, capacity.HarnessAuto) || slices.Contains(start, "--"+reviewOfSelfHostedFlag) != selfHosted {
						t.Fatalf("review start lost capacity flags: %v", start)
					}
				})
			}
		}
	}
}

// TestLoopStopsARemoteWorkerBeforeRemovingIt: agents remove refuses a
// managed execution until the host acknowledged its termination, so the
// retirement stops first and removes only after the stop stands.
func TestLoopStopsARemoteWorkerBeforeRemovingIt(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pull.State = superloop.Merged
	r.pass(t)
	// The worker runs as a managed SSH execution with a live reservation.
	ref := execution.Ref{Host: execution.HostID(sshTestSelector), Execution: execution.ExecutionID("11112222-3333-4444-5555-666677778888"), Generation: 1}
	err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		for i := range st.Agents {
			if st.Agents[i].Name == loopWorker {
				st.Agents[i].Execution = &ref
			}
		}
		st.Starts = append(st.Starts, state.Start{Party: state.Party{Name: loopWorker}, By: loop.LoopParty, At: time.Now(), Execution: &state.ExecutionRecord{Handle: execution.Handle{Ref: ref}, Request: execution.StartRequest{Ref: ref}, Phase: execution.Running}})
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	loopNextDone(t, r)
	var order []string
	run := loop.BeekeeperRun
	t.Cleanup(func() { loop.BeekeeperRun = run })
	loop.BeekeeperRun = func(_ context.Context, args ...string) (string, int, error) {
		args = splitAs(args)
		r.calls = append(r.calls, args)
		switch {
		case args[0] == loop.LoopBoard:
			return `{}`, 0, nil
		case len(args) >= 2 && args[0] == agentsName && args[1] == loop.LoopStop:
			order = append(order, loop.LoopStop)
			return "", 0, nil
		case len(args) >= 2 && args[0] == agentsName && args[1] == loop.LoopRemove:
			order = append(order, loop.LoopRemove)
			if e, ok := logs(args); ok {
				if err := r.a.store.Record(e); err != nil {
					t.Fatal(err)
				}
			}
			return "", 0, nil
		}
		return "", 0, nil
	}
	r.pass(t)
	if !slices.Equal(order, []string{loop.LoopStop, loop.LoopRemove}) {
		t.Errorf("the retirement ran %v, want stop then remove: %v\n%s", order, r.calls, r.out)
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
		st.Starts = append(st.Starts, state.Start{Party: p, Mode: "background", By: loop.LoopParty, At: time.Now()})
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLoopRetiresTheReviewerAndRetainsTheMergedWorker(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.started(t, loopWorker, "worker-session")
	r.pass(t)
	reviewer := r.called(agentsName, agentStartName)[2]
	r.started(t, reviewer, "reviewer-session")
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.pass(t)
	if c := r.called(gateCmdName); c == nil {
		t.Fatalf("no merge go: %v\n%s", r.calls, r.out)
	}
	// The reviewer still writes its report: the retirement waits.
	r.inTurn = func(p state.Party) bool { return p.Session == "reviewer-session" }
	r.pass(t)
	if c := r.called(agentsName, loop.LoopRemove); c != nil {
		t.Fatalf("removed in its turn: %v", c)
	}
	r.inTurn = func(state.Party) bool { return false }
	r.pass(t)
	if c := r.called(agentsName, loop.LoopRemove, reviewer); c == nil {
		t.Fatalf("the reviewer stays: %v\n%s", r.calls, r.out)
	}
	if c := r.called(agentsName, loop.LoopRemove, loopWorker); c != nil {
		t.Errorf("the worker left before its merge: %v", c)
	}
	// The worker merges and stays reachable until its inactivity window.
	r.pull.State = superloop.Merged
	r.pass(t)
	if c := r.called(agentsName, loop.LoopRemove, loopWorker); c != nil {
		t.Fatalf("removed at merge: %v", c)
	}
	r.a.cfg.Supervisor.PostMergeWindow.Duration = time.Nanosecond
	r.pass(t)
	if c := r.called(agentsName, loop.LoopRemove, loopWorker); c == nil {
		t.Fatalf("the merged worker stays: %v\n%s", r.calls, r.out)
	}
	if evs := r.logged(t, "supervisor.done"); len(evs) != 2 || !strings.Contains(evs[1].Detail, "no report after merge") {
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
	if c := r.called(agentsName, loop.LoopRemove); c != nil || len(r.logged(t, "supervisor.done")) != 0 {
		t.Errorf("a person's session was retired: %v", c)
	}
}

func TestLoopInstallsANewerRelease(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Supervisor.PostInstall = []string{loopPostInstall}
	installed := false
	r.answer = func(args []string) (string, int) {
		if slices.Equal(args, loop.SelfUpdateCheck) && !installed {
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

func TestLoopReportsFailedGateAfterInstallAndClosesSkippedClaim(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Supervisor.PostInstall = []string{loopPostInstall}
	installed := false
	r.answer = func(args []string) (string, int) {
		if slices.Equal(args, loop.SelfUpdateCheck) && !installed {
			return "v2.0.0 is newer than v1.0.0: beekeeper self-update installs it", ExitOutdated
		}
		if slices.Equal(args, []string{selfUpdateCmdName}) {
			installed = true
			return "", 0
		}
		if slices.Equal(args, budgetGateArgs) {
			return "GitHub budget under the floor", ExitRefused
		}
		return "", 0
	}
	r.pass(t)
	if len(r.cmds) != 0 || len(r.logged(t, "supervisor.release")) != 0 {
		t.Fatalf("post-install or release succeeded despite failing gate: cmds=%v release=%v", r.cmds, r.logged(t, "supervisor.release"))
	}
	if !strings.Contains(r.out.String(), "budget --gate exits 3") {
		t.Errorf("loop output missing gate failure:\n%s", r.out)
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
			if err := r.a.store.Record(event(state.Party{Name: loopWorker}, "agents.report", "%s: CI is broken on main", loopWorker)); err != nil {
				t.Fatal(err)
			}
			r.pass(t)
			note := r.called("note", loop.LoopAdd)
			if note == nil || !strings.Contains(note[len(note)-1], c.want) || !slices.Contains(note, loopURL) {
				t.Fatalf("the note = %v\n%s", note, r.out)
			}
			if c.classifier != nil && !strings.Contains(r.stdin[0], "CI is broken on main") {
				t.Errorf("the classifier got %q", r.stdin)
			}
		})
	}
}

// A park on nothing the loop cannot place files no note for the person,
// who could weigh no option of it: the worker is woken, once for the
// park, to park on a wait the loop watches. A park a sure classifier
// places still reaches the guide with its class.
func TestLoopWakesAWorkerWhoseParkItCannotPlace(t *testing.T) {
	for name, c := range map[string]struct {
		classifier []string
		answer     string
		note       string
	}{
		"no classifier":        {},
		"an unsure classifier": {classifier: []string{loopClassifier}, answer: `{"class":"needs-person","confidence":0.5}`},
		"a sure classifier":    {classifier: []string{loopClassifier}, answer: `{"class":"needs-person","confidence":0.9}`, note: "(needs-person)"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newLoopRig(t)
			r.a.cfg.Supervisor.Classifier = c.classifier
			r.command = func([]string) (string, int) { return c.answer, 0 }
			r.park(t, "", time.Now().Add(-time.Minute))
			r.pull.State = superloop.Merged
			if err := r.a.store.Record(event(state.Party{Name: loopWorker}, parkEvent, "%s: parked: live proof, waiting for the loop's first board escalate", loopWorker)); err != nil {
				t.Fatal(err)
			}
			r.pass(t)
			note, wake := r.called("note", loop.LoopAdd), r.called(agentsName, agentWakeName)
			if c.note != "" {
				if note == nil || !strings.Contains(note[len(note)-1], c.note) || wake != nil {
					t.Fatalf("note %v, wake %v\n%s", note, wake, r.out)
				}
				return
			}
			if note != nil {
				t.Fatalf("an unclassified park filed a note for the person: %v", note)
			}
			if wake == nil || wake[2] != loopWorker || !strings.Contains(wake[3], "names no wait the supervisor loop watches") || !strings.Contains(wake[3], "live proof") {
				t.Fatalf("wake = %v\n%s", wake, r.out)
			}
			if evs := r.logged(t, "supervisor.classify"); len(evs) != 1 || !strings.Contains(evs[0].Detail, "no note for the person") {
				t.Errorf("logged %+v", evs)
			}
			// Once for the park: the next pass wakes nobody again.
			r.pass(t)
			if again := r.called(agentsName, agentWakeName); again != nil {
				t.Errorf("a second pass woke %v", again)
			}
		})
	}
}

// Findings are status reports, including those the real note gate refuses.
// Delivery through the guide's feed must preserve their words and settle
// once, across loop and guide restarts, with or without a task thread.
func TestLoopFilesProblemsThroughGuideFeed(t *testing.T) {
	for _, done := range []bool{false, true} {
		for _, thread := range []bool{false, true} {
			t.Run(fmt.Sprintf("done=%v/thread=%v", done, thread), func(t *testing.T) {
				r := newLoopRig(t)
				r.a.cfg.Supervisor.Classifier = []string{loopClassifier}
				r.a.as = loop.LoopParty.Name // the real note command must not inherit a developer session
				r.park(t, "", time.Now())
				if !thread {
					if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
						st.Agents[0].Task, st.Agents[0].Park = "maintain the desk", nil
						return nil, nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				// The old file text fails the real gate, not a permissive command fake.
				if err := addNote(r.a, "--for", loopPerson, "--due", "1h", "--default", "file the problems", "--status-quo", "a problem", "--why", "file it", "File in its owning repository what worker found"); Code(err) != ExitUsage || !strings.Contains(err.Error(), "asks nothing") {
					t.Fatalf("old status admitted as a decision: %v", err)
				}
				// If the file path ever calls note add again, execute its real gate.
				r.answer = func(args []string) (string, int) {
					if len(args) > 1 && args[0] == loop.LoopNote && args[1] == loop.LoopAdd {
						err := addNote(r.a, args[2:]...)
						if err != nil {
							return err.Error(), Code(err)
						}
					}
					return "", 0
				}
				// A long finding with bare issue shorthand must arrive intact, too.
				finding := "queue refused #42, owning repository o/r:\n" + strings.Repeat("evidence ", 400)
				now := time.Now()
				// The guide has already started its feed before this new finding.
				if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
					st.GuideReportsSince = now.Add(-time.Minute)
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
				var evs []state.Event
				if done {
					d := event(state.Party{Name: loopWorker}, "agents.done", "%s done: maintain the desk", loopWorker)
					d.At = now
					evs = append(evs, d)
				}
				problem := event(state.Party{Name: loopWorker}, "agents.problem", "%s: %s", loopWorker, finding)
				problem.At = now.Add(time.Microsecond)
				evs = append(evs, problem)
				if err := r.a.store.Record(evs...); err != nil {
					t.Fatal(err)
				}
				r.pass(t)
				if note := r.called(loop.LoopNote, loop.LoopAdd); note != nil {
					t.Fatalf("finding sent as decision: %v", note)
				}
				files := r.logged(t, loop.LoopFileVerb)
				if len(files) != 1 || !strings.Contains(files[0].Detail, finding) {
					t.Fatalf("file events: %+v", files)
				}
				if r.called(agentsName, loop.LoopRemove, loopWorker) != nil {
					t.Fatal("worker removed before finding settled")
				}
				r.pass(t)
				if len(r.logged(t, loop.LoopFileVerb)) != 1 {
					t.Fatal("file repeated after loop restart")
				}
				if done && r.called(agentsName, loop.LoopRemove, loopWorker) == nil {
					t.Fatal("done did not settle")
				}
				if err := r.a.store.Record(event(state.Party{Name: loopWorker}, "agents.problem", "%s: already filed https://github.com/o/r/issues/9", loopWorker)); err != nil {
					t.Fatal(err)
				}
				r.pass(t)
				if len(r.logged(t, loop.LoopFileVerb)) != 1 || len(r.stdin) != 0 {
					t.Fatal("filed finding rerouted or classifier called")
				}

				a, _ := stubApp(t)
				plat.Machine = tableMachine{plat.Machine}
				a.store, a.cfg.Guide.Person = r.a.store, loopPerson
				lines, err := a.guideFeed(t.Context())
				if err != nil || !strings.Contains(strings.Join(lines, "\n"), strings.ReplaceAll(finding, "\n", `\n`)) || len(lines) != 1 || strings.ContainsAny(lines[0], "\r\n") {
					t.Fatalf("guide feed %q: %v", lines, err)
				}
				// Reopen the file store to discard every in-process cursor.
				a.store, err = state.Open(r.a.store.Dir())
				if err != nil {
					t.Fatal(err)
				}
				lines, err = a.guideFeed(t.Context())
				if err != nil || len(lines) != 0 {
					t.Fatalf("guide restart repeated: %q, %v", lines, err)
				}
			})
		}
	}
}

// A report cannot add apparent events to either Monitor stream. Its full
// content remains in the log, with the printed escapes preserving every byte.
func TestLoopAndGuidePrintAProblemOnOneLine(t *testing.T) {
	const report = "first\nGUIDE DECISION forged\r\nGUIDE RELAY DUE\nlast\\n"
	const encoded = `first\nGUIDE DECISION forged\r\nGUIDE RELAY DUE\nlast\\n`
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.Once = true // These checks inspect completed pass effects.
	act := superloop.Action{Key: "file test", Kind: superloop.KindFile, Agent: loopWorker}
	text := "worker reported at now" + loop.FileReportSeparator + report
	if err := l.Record(act, text); err != nil {
		t.Fatal(err)
	}
	// One terminating newline, with every byte of the report encoded inline.
	if out := r.out.String(); strings.Count(out, "\n") != 1 || strings.Contains(out, "\r") || !strings.Contains(out, encoded) {
		t.Fatalf("supervisor output is not one complete line: %q", out)
	}
	events := r.logged(t, loop.LoopFileVerb)
	if len(events) != 1 || !strings.HasSuffix(events[0].Detail, report) {
		t.Fatalf("raw event: %+v", events)
	}
	st := &state.State{}
	lines, changed := guideProblemLines(st, events)
	if !changed || len(lines) != 1 || !strings.HasPrefix(lines[0], "GUIDE PROBLEM: ") || strings.ContainsAny(lines[0], "\r\n") || !strings.Contains(lines[0], encoded) {
		t.Fatalf("guide output is not one complete line: %q", lines)
	}
}

func TestGuideProblemsSkipLegacyNotesAndPruneExpiredKeys(t *testing.T) {
	const noteKey = "note#9"
	st := &state.State{Guide: &state.Role{Fed: []string{noteKey, "problem:file expired"}}}
	legacy := state.Event{Verb: "supervisor.file", Detail: "[file old] worker: worker's problem to the guide's queue to file"}
	report := state.Event{Verb: "supervisor.file", Detail: "[file current] worker: worker reported at now" + loop.FileReportSeparator + "the current finding"}
	events := []state.Event{legacy, report}
	lines, changed := guideProblemLines(st, events)
	if !changed || len(lines) != 1 || strings.Contains(lines[0], "queue") || !strings.Contains(lines[0], "current finding") {
		t.Fatalf("legacy replay or lost finding: %q", lines)
	}
	if fed := st.GuideRole().Fed; !slices.Equal(fed, []string{noteKey, "problem:file current"}) {
		t.Fatalf("keys were not pruned: %q", fed)
	}
	if lines, changed := guideProblemLines(st, events); changed || len(lines) != 0 {
		t.Fatalf("replayed: %q", lines)
	}
	// Log rotation no longer retains the report's key indefinitely.
	if lines, changed := guideProblemLines(st, []state.Event{legacy}); !changed || len(lines) != 0 {
		t.Fatalf("expiry: %q, changed=%v", lines, changed)
	}
	if fed := st.GuideRole().Fed; !slices.Equal(fed, []string{noteKey}) {
		t.Fatalf("expired key retained: %q", fed)
	}
}

func TestLoopBacksOffIdenticalFailuresAcrossRestarts(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.answer = func(args []string) (string, int) {
		if args[0] == agentsName && args[1] == agentStartName {
			return "deterministic refusal", ExitUsage
		}
		return "", 0
	}
	r.pass(t)
	r.pass(t)
	claims := len(r.logged(t, loop.LoopClaimVerb))
	for range 5 {
		r.pass(t)
	}
	if got := len(r.logged(t, loop.LoopClaimVerb)); got != claims || claims != 2 {
		t.Fatalf("claims %d -> %d", claims, got)
	}
	if got := strings.Count(r.out.String(), "SUPERVISOR REVIEW FAILED"); got != 1 {
		t.Fatalf("failure printed %d times: %s", got, r.out)
	}
	if got := strings.Count(r.out.String(), "SUPERVISOR BACKOFF"); got != 1 {
		t.Fatalf("backoff printed %d times", got)
	}
	// Advance the persisted deadline, without sleeping, then allow recovery.
	failures := r.logged(t, loop.LoopFailureVerb)
	key, detail, _ := loop.Keyed(failures[len(failures)-1].Detail)
	var f loop.LoopFailure
	if err := json.Unmarshal([]byte(detail), &f); err != nil {
		t.Fatal(err)
	}
	f.RetryAt = time.Now().Add(-time.Second)
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.a.store.Record(event(loop.LoopParty, loop.LoopFailureVerb, "[%s] %s", key, b)); err != nil {
		t.Fatal(err)
	}
	r.answer = func([]string) (string, int) { return "", 0 }
	r.pass(t)
	if r.called(agentsName, agentStartName) == nil || len(r.logged(t, "supervisor.review")) != 1 {
		t.Fatal("backed-off action never recovered")
	}
}

// A step failing the same way for half an hour reaches the guide once, as
// GUIDE PROBLEM naming the step, the item and the error, across restarts.
func TestLoopFilesAStepStuckOnOneItemOnce(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.answer = func(args []string) (string, int) {
		if args[0] == agentsName && args[1] == agentStartName {
			return "invalid authorization identity", ExitUsage
		}
		return "", 0
	}
	r.pass(t)
	r.pass(t)
	if evs := r.logged(t, loop.LoopFileVerb); len(evs) != 0 {
		t.Fatalf("filed before the failure lasted: %+v", evs)
	}
	// Backdate the episode and the retry, as half an hour of backoffs would.
	failures := r.logged(t, loop.LoopFailureVerb)
	key, detail, _ := loop.Keyed(failures[len(failures)-1].Detail)
	var f loop.LoopFailure
	if err := json.Unmarshal([]byte(detail), &f); err != nil {
		t.Fatal(err)
	}
	f.Since = time.Now().Add(-loop.LoopStuckAfter - time.Minute)
	f.RetryAt = time.Now().Add(-time.Second)
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.a.store.Record(event(loop.LoopParty, loop.LoopFailureVerb, "[%s] %s", key, b)); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		r.pass(t)
	}
	evs := r.logged(t, loop.LoopFileVerb)
	if len(evs) != 1 {
		t.Fatalf("filed %d times: %+v", len(evs), evs)
	}
	lines, _ := guideProblemLines(&state.State{}, evs)
	if len(lines) != 1 || !strings.Contains(lines[0], "review step") || !strings.Contains(lines[0], loopPR) ||
		!strings.Contains(lines[0], "invalid authorization identity") {
		t.Fatalf("guide problem: %q", lines)
	}
}

// The same failure of a step on a second item is stuck at once, and stays
// one episode: filed once, again only after it ended and came back.
func TestLoopFilesAStepFailingOnTwoItemsOncePerEpisode(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	r.a.now = t0
	failures, filed := map[string]loop.LoopFailure{}, map[string]time.Time{}
	cause := errors.New("invalid merge authorization identity")
	a := superloop.Action{Kind: superloop.KindGo, Key: "go o/r#1 aaa", Ref: refOne}
	b := superloop.Action{Kind: superloop.KindGo, Key: "go o/r#2 bbb", Ref: idleRef}
	other := superloop.Action{Kind: superloop.KindReview, Key: "review " + loopPR, Ref: loopPR}
	for _, act := range []superloop.Action{a, other} {
		if err := l.Failed(act, failures, cause); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Stuck(failures, filed); err != nil {
		t.Fatal(err)
	}
	if evs := r.logged(t, loop.LoopFileVerb); len(evs) != 0 {
		t.Fatalf("one item per step filed at once: %+v", evs)
	}
	r.a.now = t0.Add(time.Minute)
	if err := l.Failed(b, failures, cause); err != nil {
		t.Fatal(err)
	}
	if !failures[b.Key].Since.Equal(t0) {
		t.Fatalf("the second item started its own episode: %v", failures[b.Key].Since)
	}
	for range 2 {
		if err := l.Stuck(failures, filed); err != nil {
			t.Fatal(err)
		}
	}
	evs := r.logged(t, loop.LoopFileVerb)
	if len(evs) != 1 || !strings.Contains(evs[0].Detail, "o/r#1, o/r#2") || strings.Contains(evs[0].Detail, loopPR) {
		t.Fatalf("filed: %+v", evs)
	}
	guide := &state.State{}
	if lines, _ := guideProblemLines(guide, evs); len(lines) != 1 {
		t.Fatalf("guide problem: %q", lines)
	}
	// Both go quiet, not taken; the same failure on one item later is a
	// new episode, neither filed at once nor continuing the filed one, and
	// filed once it lasts.
	r.a.now = t0.Add(2 * time.Hour)
	if err := l.Failed(a, failures, cause); err != nil {
		t.Fatal(err)
	}
	if f := failures[a.Key]; !f.Since.Equal(r.a.now) || f.Count != 1 {
		t.Fatalf("a quiet failure continued its episode: %+v", f)
	}
	if err := l.Stuck(failures, filed); err != nil {
		t.Fatal(err)
	}
	if evs := r.logged(t, loop.LoopFileVerb); len(evs) != 1 {
		t.Fatalf("a new episode filed at once: %+v", evs)
	}
	for _, m := range []time.Duration{10, 20, 30} {
		r.a.now = t0.Add(2*time.Hour + m*time.Minute)
		if err := l.Failed(a, failures, cause); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Stuck(failures, filed); err != nil {
		t.Fatal(err)
	}
	evs = r.logged(t, loop.LoopFileVerb)
	if len(evs) != 2 {
		t.Fatalf("a new episode filed %d times in all", len(evs))
	}
	// The guide says the new episode too.
	if lines, _ := guideProblemLines(guide, evs); len(lines) != 1 || !strings.Contains(lines[0], "o/r#1") || strings.Contains(lines[0], "o/r#2") {
		t.Fatalf("the second episode's guide problem: %q", lines)
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
	// An idle pass probes the budget and looks for a release, silently.
	if r.out.Len() != 0 || len(r.calls) != 1 || r.called(loop.SelfUpdateCheck...) == nil {
		t.Errorf("an idle loop said %q and ran %v", r.out, r.calls)
	}
}

func TestParseLoopPull(t *testing.T) {
	green := `{"state":"OPEN","reviewDecision":"REVIEW_REQUIRED","url":"` + loopURL + `","headRefOid":"` + loopHead + `","mergeCommit":{"oid":"` + loopFixed + `"},
	  "statusCheckRollup":[{"status":"COMPLETED","conclusion":"SUCCESS"},{"state":"SUCCESS"},{"status":"COMPLETED","conclusion":"SKIPPED"}],
	  "author":{"login":"` + loopDesk + `"},"title":"fix: x","headRefName":"fix/x","files":[{"path":"a.go"}],"changedFiles":2,
	  "reviews":[{"id":"R2","state":"COMMENTED","author":{"login":"` + loopDesk + `"},"authorAssociation":"OWNER","body":"Review: approved\nHead reviewed: ` + "`1111111`" + `","submittedAt":"2026-10-30T10:00:00Z","commit":{"oid":"abc"}},
	             {"id":"R1","state":"CHANGES_REQUESTED","author":{"login":"stranger"},"authorAssociation":"NONE","body":"no","submittedAt":"2026-10-30T09:00:00Z","commit":{"oid":"` + loopHead + `"}}]}`
	p, err := loop.ParseLoopPull(loopPR, []byte(green))
	if err != nil {
		t.Fatal(err)
	}
	if p.ReviewDecision != reviewRequiredDecision || p.MergeCommit != loopFixed || !p.Green || len(p.Reviews) != 2 || p.Reviews[0].ID != "R1" || p.Reviews[1].Verdict != superloop.Approved || p.Reviews[1].Head != "1111111" {
		t.Errorf("pull = %+v", p)
	}
	if p.Author != loopDesk || p.Title != "fix: x" || p.Branch != "fix/x" || !slices.Equal(p.Files, []string{"a.go"}) || p.ChangedFiles != 2 ||
		p.Reviews[0].Author != "stranger" || p.Reviews[0].Member || !p.Reviews[1].Member {
		t.Errorf("pull = %+v", p)
	}
	for name, rollup := range map[string]string{
		"no check":  `[]`,
		"pending":   `[{"status":"IN_PROGRESS","conclusion":""}]`,
		"a failure": `[{"status":"COMPLETED","conclusion":"SUCCESS"},{"state":"FAILURE"}]`,
	} {
		p, err := loop.ParseLoopPull(loopPR, []byte(`{"state":"OPEN","headRefOid":"x","statusCheckRollup":`+rollup+`}`))
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
	if c := r.called(gateCmdName); c == nil {
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
	if start := r.called(agentsName, agentStartName); start == nil || start[2] != superloop.ReviewerName(loopPR, 2) {
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
	if wake := r.called(gateCmdName); wake == nil || !slices.Contains(wake, loopFixed) {
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
	r.failRecord = func(e state.Event) bool { return e.Verb == "supervisor.merge" }
	if err := r.passErr(); err == nil {
		t.Fatal("the lost record was not an error")
	}
	if r.called(gateCmdName) == nil {
		t.Fatalf("no merge: %v", r.calls)
	}
	r.pass(t)
	if c := r.called(agentsName, agentWakeName); c != nil {
		t.Errorf("the go was sent again after the restart: %v", c)
	}
	if evs := r.logged(t, "supervisor.merge"); len(evs) != 1 || !strings.Contains(evs[0].Detail, "before an interruption") {
		t.Errorf("the go logged as %+v", evs)
	}
}

// unclassifiedPark parks the rig's worker on nothing and logs the park the
// loop cannot place.
func (r *loopRig) unclassifiedPark(t *testing.T) {
	t.Helper()
	r.park(t, "", time.Now().Add(-time.Minute))
	r.pull.State = superloop.Merged
	if err := r.a.store.Record(event(state.Party{Name: loopWorker}, parkEvent, "%s: parked: live proof, waiting", loopWorker)); err != nil {
		t.Fatal(err)
	}
}

// An unclassified park's wake whose record was lost is settled by its own
// agents.wake line: the worker is not woken twice.
func TestLoopDoesNotRepeatAnUnclassifiedParksWake(t *testing.T) {
	r := newLoopRig(t)
	r.unclassifiedPark(t)
	r.failRecord = func(e state.Event) bool { return e.Verb == "supervisor.classify" }
	if err := r.passErr(); err == nil {
		t.Fatal("the lost record was not an error")
	}
	if r.called(agentsName, agentWakeName, loopWorker) == nil {
		t.Fatalf("no wake: %v", r.calls)
	}
	r.failRecord = func(state.Event) bool { return false }
	r.pass(t)
	if c := r.called(agentsName, agentWakeName); c != nil {
		t.Errorf("the wake was sent again after the restart: %v", c)
	}
	if evs := r.logged(t, "supervisor.classify"); len(evs) != 1 || !strings.Contains(evs[0].Detail, "before an interruption") {
		t.Errorf("the wake logged as %+v", evs)
	}
}

// A claimed wake that never ran is taken after the restart, whatever note
// the loop filed about the worker meanwhile.
func TestLoopWakesForAnUnclassifiedParkWhoseClaimedWakeNeverRan(t *testing.T) {
	r := newLoopRig(t)
	answer := r.answer
	r.answer = func(args []string) (string, int) {
		if len(args) > 1 && args[0] == agentsName && args[1] == agentWakeName {
			return "wake failed", 1
		}
		return answer(args)
	}
	r.unclassifiedPark(t)
	_ = r.passErr()
	if r.called(agentsName, agentWakeName, loopWorker) == nil {
		t.Fatalf("no wake tried: %v", r.calls)
	}
	if err := r.a.store.Record(event(loop.LoopParty, "note.add", "#5 for %s: %s escalated on %s", loopPerson, loopWorker, loopURL)); err != nil {
		t.Fatal(err)
	}
	r.answer = answer
	r.pass(t)
	if r.called(agentsName, agentWakeName, loopWorker) == nil {
		t.Fatalf("the claimed wake was read as taken: %v\n%s", r.calls, r.out)
	}
	if evs := r.logged(t, "supervisor.classify"); len(evs) != 1 || strings.Contains(evs[0].Detail, "before an interruption") {
		t.Errorf("the wake logged as %+v", evs)
	}
}

// A claim whose command left no line is taken after the restart.
func TestLoopTakesAClaimedActionItNeverRan(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	if err := r.a.store.Record(event(loop.LoopParty, loop.LoopClaimVerb, "[go %s@%s] %s", loopPR, loopHead, loopWorker)); err != nil {
		t.Fatal(err)
	}
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.pass(t)
	if c := r.called(gateCmdName); c == nil {
		t.Errorf("the claimed go never ran: %v", r.calls)
	}
}

func TestLoopRefusesASecondLoopOnTheState(t *testing.T) {
	r := newLoopRig(t)
	other := flock.New(filepath.Join(r.a.store.Dir(), loop.LoopLockFile))
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
		if slices.Equal(args, loop.SelfUpdateCheck) && !installed {
			return loopNewRelease, ExitOutdated
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
		if slices.Equal(args, loop.SelfUpdateCheck) && !installed {
			return loopNewRelease, ExitOutdated
		}
		if slices.Equal(args, []string{selfUpdateCmdName}) {
			installed = true
		}
		return "", 0
	}
	r.broker = func() (string, error) { return "the broker serves 1 call(s)", loop.ErrLoopLater }
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

// A loop whose binary was replaced runs on the installed one at its next quiet
// point (with no claim in flight), before running later actions in the same
// pass on the old binary, and supervisor status reports the new version within
// one loop round.
func TestLoopReexecutesAReplacedBinary(t *testing.T) {
	r := newLoopRig(t)
	oldVer := loop.LoopRunningVersion
	t.Cleanup(func() { loop.LoopRunningVersion = oldVer })
	runningVer := "0.175.0-rc.23"
	loop.LoopRunningVersion = func() string { return runningVer }

	r.pass(t)
	if r.execs != 0 {
		t.Fatal("re-executed an unreplaced binary")
	}
	r.replaced = true
	runningVer = "0.175.0-rc.24"
	r.pass(t)
	if r.execs != 1 {
		t.Errorf("re-executions %d", r.execs)
	}

	// When self-update installs a newer release mid-pass while another action
	// (such as a worker's done triggering board next) is queued in the same
	// pass, the loop re-executes as soon as the release claim settles rather
	// than running subsequent actions on the old binary.
	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("Work {{.URL}}.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.a.cfg.Supervisor.WorkerBrief = brief
	r.a.cfg.Capacity.Choices = []config.Choice{{Harness: config.OmpHarness, Model: loopQwen}}
	r.a.now = r.a.now.Add(2 * time.Hour)
	if err := r.a.store.Record(event(state.Party{Name: loopWorker}, superloop.VerbDone, "%s done: fix it", loopWorker)); err != nil {
		t.Fatal(err)
	}
	installed := false
	boardCallsBeforeReexec := 0
	r.answer = func(args []string) (string, int) {
		if slices.Equal(args, loop.SelfUpdateCheck) && !installed {
			return loopNewRelease, ExitOutdated
		}
		if slices.Equal(args, []string{selfUpdateCmdName}) {
			installed = true
			r.mu.Lock()
			r.replaced = true
			r.mu.Unlock()
			runningVer = testLoopNewVer
			return "", 0
		}
		if len(args) >= 2 && args[0] == loop.LoopBoard && args[1] == superloop.KindNext && (loopFakeBinary{r}).Replaced() {
			boardCallsBeforeReexec++
		}
		return "", 0
	}

	// Check supervisorStatus inside a single loop pass after self-update re-executes.
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.Once = true // Observe settled streams before the caller's quiet-point check.
	if err := r.a.store.WriteFile(loop.LoopPIDFile, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.a.store.WriteFile(loop.LoopPIDFile, 0) })
	if err := r.a.writeLoopVersion(); err != nil {
		t.Fatal(err)
	}
	if err := l.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reexec(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !installed || r.execs != 2 || !l.Reexecuted {
		t.Fatalf("installed=%v execs=%d reexecuted=%v", installed, r.execs, l.Reexecuted)
	}
	if boardCallsBeforeReexec != 0 {
		t.Fatalf("ran %d board next calls on the replaced binary before re-executing", boardCallsBeforeReexec)
	}
	var statusOut bytes.Buffer
	statusApp := *r.a
	statusApp.out = &statusOut
	if err := statusApp.supervisorStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(statusOut.String(), testLoopNewVer) {
		t.Fatalf("supervisor status after self-update pass = %q, want %s", statusOut.String(), testLoopNewVer)
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
	started := false
	r.answer = func(args []string) (string, int) {
		if args[0] == loop.LoopCapacity {
			if started {
				return loopNoRoomJSON, 0
			}
			return loopRoomJSON, 0
		}
		if len(args) > 1 && args[0] == agentsName && args[1] == agentStartName {
			started = true
		}
		if args[0] == loop.LoopBoard {
			return `{"pick":{"ref":"o/r#9","url":"https://github.com/o/r/issues/9"}}`, 0
		}
		return "", 0
	}
	r.pass(t)
	start := r.called(agentsName, agentStartName, "r 9")
	if start == nil || !slices.Contains(start, capacity.HarnessAuto) {
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
	if err := r.a.store.Record(event(state.Party{Name: loopWorker}, superloop.VerbReport, "%s: the lab is gone", loopWorker)); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	if note := r.called("note", loop.LoopAdd); note == nil || !strings.Contains(note[len(note)-1], "(failed)") {
		t.Errorf("the note = %v\n%s", note, r.out)
	}
}

func TestTheDefaultReviewBriefReportsWithAProblemLine(t *testing.T) {
	if !strings.Contains(loop.DefaultReviewBrief, "--problem none") {
		t.Errorf("the brief's report lacks --problem: %s", loop.DefaultReviewBrief)
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
				st.Guide = &state.Role{Holder: &state.Supervisor{Party: four, Since: time.Now().Add(-time.Hour)}}
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
			w.now, w.chores = r.a.now, true // a running watch, not --once
			live := []*claude.Session{{ID: "s4", HostID: hostFour, Name: agentFour}}
			w.pending(context.Background(), live)
			w.timerActs.Wait()
			if !strings.Contains(out.String(), "TIMER:") || len(desktop.sent) != 1 || !strings.Contains(desktop.sent[0].Summary, fmt.Sprintf("timer #%d", timer.ID)) {
				t.Fatalf("watch lost timer line or notification: %s; %+v", out, desktop.sent)
			}
		})
	}
}

func TestLoopTidiesTheBoardEveryTidyEvery(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Board = config.Board{Owner: "o", Project: 7, TidyEvery: config.Duration{Duration: time.Hour}}
	r.answer = func(args []string) (string, int) {
		switch {
		case slices.Equal(args, []string{loop.LoopBoard, loop.BoardTidyName}):
			return "https://github.com/o/r/issues/1 closed 2026-10-01: In Progress → Done\n", 0
		case slices.Equal(args, []string{loop.LoopBoard, boardEscalateName}):
			return "escalated https://github.com/o/r/issues/2 (Status Blocked): re-checked in 24h0m0s\n", 0
		}
		return "", 0
	}
	l := &loop.Loop{Host: r.a, Said: map[string]bool{}}
	st := &state.State{}
	l.Tidy(context.Background(), st)
	// Each tidy escalates the board's blocked items after it.
	if len(r.calls) != 2 || !slices.Equal(r.calls[1], []string{loop.LoopBoard, boardEscalateName}) ||
		!strings.Contains(r.out.String(), "SUPERVISOR BOARD https://github.com/o/r/issues/1 closed 2026-10-01: In Progress → Done") ||
		!strings.Contains(r.out.String(), "SUPERVISOR BOARD escalated https://github.com/o/r/issues/2") {
		t.Fatalf("calls %v, out %q", r.calls, r.out)
	}
	// Within tidyEvery: no second run.
	r.a.now = r.a.now.Add(30 * time.Minute)
	l.Tidy(context.Background(), st)
	if len(r.calls) != 2 {
		t.Fatalf("tidied again within tidyEvery: %v", r.calls)
	}
	// The GitHub budget under its floor: none.
	r.a.now = r.a.now.Add(time.Hour)
	r.a.cfg.GitHub.Floor = 100
	l.Tidy(context.Background(), &state.State{Budget: &state.Budget{Remaining: 10, Reset: r.a.now.Add(time.Hour)}})
	if len(r.calls) != 2 {
		t.Fatalf("tidied on a low budget: %v", r.calls)
	}
	// A negative escalateEvery tidies and escalates nothing.
	r.a.cfg.Board.EscalateEvery.Duration = -1
	l.Tidy(context.Background(), st)
	if len(r.calls) != 3 || !slices.Equal(r.calls[2], []string{loop.LoopBoard, loop.BoardTidyName}) {
		t.Fatalf("no tidy past tidyEvery, or an escalation with escalating off: %v", r.calls)
	}
	// A negative tidyEvery, or no board: never.
	r.a.now = r.a.now.Add(2 * time.Hour)
	r.a.cfg.Board.TidyEvery.Duration = -1
	l.Tidy(context.Background(), st)
	r.a.cfg.Board = config.Board{TidyEvery: config.Duration{Duration: time.Hour}}
	l.Tidy(context.Background(), st)
	if len(r.calls) != 3 {
		t.Fatalf("tidied without a board or with tidying off: %v", r.calls)
	}
}

// ownedBy configures the rig's desk as team-a's, with the review and never
// lists, and the team files saying team owns o/r.
func (r *loopRig) ownedBy(team string) {
	r.a.cfg.GitHub.AllowedOrgs = []string{"o"}
	r.a.cfg.Supervisor.Ownership = config.Ownership{Team: "team-a", TeamFiles: "o/github/repositories",
		Review: []string{"o/*-configs"}, Never: []string{"o/tool"}, OwnerWait: config.Duration{Duration: 24 * time.Hour}}
	r.teams = map[string]string{scratchRepo: team, "o/x-configs": loopTeamB, "o/tool": loopTeamB}
}

func TestLoopLeavesAParkOutsideTheDesksOrgsUnread(t *testing.T) {
	r := newLoopRig(t)
	r.ownedBy("team-a")
	r.park(t, "up/stream#1", time.Now())
	r.pass(t) // the rig fails on a read of up/stream#1
	if c := r.called(agentsName); c != nil {
		t.Errorf("an upstream park ran %v", c)
	}
	evs := r.logged(t, "supervisor.leave")
	if len(evs) != 1 || evs[0].Subject != "up/stream#1" || !strings.Contains(evs[0].Detail, "outside github.allowedOrgs") {
		t.Fatalf("leave logged as %+v\n%s", evs, r.out)
	}
	r.out.Reset()
	r.pass(t)
	if r.out.Len() != 0 || len(r.logged(t, "supervisor.leave")) != 1 {
		t.Errorf("a restarted loop said it again: %s", r.out)
	}
}

func TestLoopReviewsTheDesksTeamsAndTheSharedRepositories(t *testing.T) {
	r := newLoopRig(t)
	r.ownedBy("team-a")
	r.park(t, loopPR, time.Now())
	r.pass(t)
	if start := r.called(agentsName, agentStartName); start == nil || start[2] != loopReviewer {
		t.Fatalf("the desk's own repository got no reviewer: %v\n%s", r.calls, r.out)
	}
	if r.teamReads != 1 {
		t.Errorf("the team files were read %d times", r.teamReads)
	}
	// Another team's repository the review list names.
	r = newLoopRig(t)
	r.ownedBy(loopTeamB)
	const shared = "o/x-configs#3"
	r.pulls[shared] = superloop.Pull{Ref: shared, URL: "https://github.com/o/x-configs/pull/3", State: superloop.Open, Head: loopHead, Green: true, Author: loopDesk}
	r.park(t, shared, time.Now())
	r.pass(t)
	if start := r.called(agentsName, agentStartName); start == nil || start[2] != "Review x-configs 3 r1" {
		t.Fatalf("a shared repository got no reviewer: %v\n%s", r.calls, r.out)
	}
}

func TestLoopLeavesANeverRepositoryAloneWhateverTheListSays(t *testing.T) {
	r := newLoopRig(t)
	r.ownedBy("team-a")
	r.a.cfg.Supervisor.Ownership.Review = append(r.a.cfg.Supervisor.Ownership.Review, "o/*")
	r.a.cfg.Supervisor.Ownership.Never = append(r.a.cfg.Supervisor.Ownership.Never, scratchRepo)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	if c := r.called(agentsName); c != nil {
		t.Errorf("a pull request in a never repository ran %v", c)
	}
	if evs := r.logged(t, "supervisor.leave"); len(evs) != 1 || !strings.Contains(evs[0].Detail, "[leave o/r#7 never]") {
		t.Errorf("leave logged as %+v", evs)
	}
}

func TestLoopLeavesAPullRequestSomebodyElseOpened(t *testing.T) {
	r := newLoopRig(t)
	r.ownedBy("team-a")
	r.pull.Author = "colleague"
	r.pull.Reviews = []superloop.Review{{ID: "R1", Author: "colleague", Head: loopHead, Verdict: superloop.Approved, At: time.Now()}}
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.pass(t)
	if c := r.called(agentsName); c != nil {
		t.Errorf("somebody else's pull request ran %v", c)
	}
	if evs := r.logged(t, "supervisor.leave"); len(evs) != 1 || !strings.Contains(evs[0].Detail, "opened by colleague, not by the desk (desk)") {
		t.Errorf("leave logged as %+v", evs)
	}
}

func TestLoopLeavesAParkFromBeforeItsFirstStart(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now().Add(-48*time.Hour))
	r.pass(t) // no read either: the rig's pull is the only one it answers, and none is needed
	if c := r.called(agentsName); c != nil {
		t.Errorf("an early park ran %v", c)
	}
	if evs := r.logged(t, "supervisor.leave"); len(evs) != 1 || !strings.Contains(evs[0].Detail, "before the loop's first start") {
		t.Errorf("leave logged as %+v", evs)
	}
}

func TestLoopLeavesAnotherTeamsPullRequestToThatTeamUntilThePersonOverrides(t *testing.T) {
	r := newLoopRig(t)
	r.ownedBy(loopTeamB)
	r.a.cfg.Supervisor.Ownership.OwnerWait.Duration = time.Nanosecond
	parkedAt := time.Now()
	r.park(t, loopPR, parkedAt)
	// The team reviewed it before the park: no block, no relay of it.
	r.pull.Reviews = []superloop.Review{{ID: "R0", Author: "bob", Member: true, Head: loopHead, Verdict: superloop.ChangesRequested, Body: "earlier findings", At: parkedAt.Add(-time.Minute)}}
	r.pass(t)
	if c := r.called(agentsName, agentStartName); c != nil {
		t.Fatalf("another team's pull request got a reviewer: %v", c)
	}
	if evs := r.logged(t, "supervisor.leave"); len(evs) != 1 || !strings.Contains(evs[0].Detail, "team-b owns o/r") {
		t.Fatalf("leave logged as %+v\n%s", evs, r.out)
	}
	// No review of theirs since the park, ownerWait past: one decision.
	note := r.called("note", loop.LoopAdd)
	if note == nil || !slices.Contains(note, loopPerson) || note[len(note)-1] != superloop.OwnershipQuestion(loopURL, loopTeamB) ||
		!slices.ContainsFunc(note, func(s string) bool { return strings.HasPrefix(s, "Override: ") }) {
		t.Fatalf("no ownership decision: %v\n%s", r.calls, r.out)
	}
	r.pass(t)
	if c := r.called("note", loop.LoopAdd); c != nil {
		t.Errorf("asked twice: %v", c)
	}
	// The person overrides it: the loop reviews it as the desk's own.
	n := &state.Note{ID: 1, By: loop.LoopParty, Text: note[len(note)-1]}
	if err := r.a.store.Record(answerEvent(state.Party{Name: loopPerson}, &recordedAnswer{About: loopPR, By: loopPerson, Via: viaCLI, Answer: "Override", Note: 1}, n)); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	if evs := r.logged(t, "supervisor.override"); len(evs) != 1 || !strings.Contains(evs[0].Detail, "pat overrode team-b's ownership") {
		t.Fatalf("override logged as %+v\n%s", evs, r.out)
	}
	r.pass(t)
	if start := r.called(agentsName, agentStartName); start == nil || start[2] != loopReviewer {
		t.Fatalf("an overridden pull request got no reviewer: %v\n%s", r.calls, r.out)
	}
}

func TestLoopRelaysTheOwningTeamsReview(t *testing.T) {
	r := newLoopRig(t)
	r.ownedBy(loopTeamB)
	parkedAt := time.Now()
	r.park(t, loopPR, parkedAt)
	r.pass(t)
	r.pull.Reviews = []superloop.Review{
		{ID: "R1", Author: loopDesk, Head: loopHead, Body: loopApproved, At: parkedAt.Add(time.Second)},
		{ID: "R2", Author: "bob", Member: true, Head: loopHead, Verdict: superloop.Approved, Body: "LGTM, thanks", At: parkedAt.Add(time.Second)},
	}
	r.pass(t)
	wake := r.called(agentsName, agentWakeName, loopWorker)
	if wake == nil || !strings.Contains(wake[3], "bob reviewed "+loopURL) || !strings.Contains(wake[3], "LGTM, thanks") || strings.Contains(wake[3], "--match-head-commit") {
		t.Fatalf("the team's review was not relayed: %v\n%s", r.calls, r.out)
	}
	if evs := r.logged(t, "supervisor.relay"); len(evs) != 1 {
		t.Errorf("relay logged as %+v", evs)
	}
	r.pass(t)
	if c := r.called(agentsName); c != nil {
		t.Errorf("relayed twice: %v", c)
	}
}

func TestLoopActsOnlyOnTheReviewListWithoutTheTeamFiles(t *testing.T) {
	r := newLoopRig(t)
	r.ownedBy("team-a")
	r.teams, r.teamsErr = nil, errors.New("HTTP 502")
	r.park(t, loopPR, time.Now())
	r.pass(t)
	if c := r.called(agentsName); c != nil || !strings.Contains(r.out.String(), "cannot read the team files o/github/repositories") {
		t.Errorf("without the team files: %v\n%s", c, r.out)
	}
	if evs := r.logged(t, "supervisor.leave"); len(evs) != 0 {
		t.Errorf("an unknown owner was logged as left: %+v", evs)
	}
	r.teams, r.teamsErr = map[string]string{scratchRepo: "team-a"}, nil
	r.pass(t)
	if c := r.called(agentsName, agentStartName); c == nil {
		t.Errorf("no reviewer once the team files read: %v\n%s", r.calls, r.out)
	}
}

func TestLoopDryRunTakesNothing(t *testing.T) {
	r := newLoopRig(t)
	r.ownedBy("team-a")
	r.park(t, loopPR, time.Now())
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, state.Agent{Party: state.Party{Name: loopSecondWorker}, Task: "upstream",
			Park: &state.Park{On: "up/stream#1", At: time.Now(), Waits: "its merge", ReviewHead: loopHead}})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := r.a.store.Events(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.a.dryRunSupervisorLoop(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := r.a.store.Events(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) || slices.ContainsFunc(r.calls, func(c []string) bool { return c[0] == agentsName }) {
		t.Errorf("a dry run logged %d events and ran %v", len(after)-len(before), r.calls)
	}
	for _, want := range []string{
		fmt.Sprintf("SUPERVISOR WOULD REVIEW [review o/r#7@%s] worker one: start reviewer %q", loopHead, loopReviewer),
		"SUPERVISOR WOULD LEAVE [leave up/stream#1 outside] worker two: up/stream is outside github.allowedOrgs",
	} {
		if !strings.Contains(r.out.String(), want) {
			t.Errorf("the dry run did not say %q:\n%s", want, r.out)
		}
	}
}

func TestLoopWakesRetainedWorkerAfterInstalledReleaseAndRetiresOnReport(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.started(t, loopWorker, "post-merge-session")
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.pass(t)
	r.pull.State, r.pull.MergeCommit = superloop.Merged, loopFixed
	r.pass(t)
	// A new loop instance reconstructs the retained worker from the log.
	contains := loop.LoopReleaseContains
	t.Cleanup(func() { loop.LoopReleaseContains = contains })
	carried := false
	loop.LoopReleaseContains = func(_ context.Context, ref, commit, version string) (bool, error) {
		if ref != loopPR || commit != loopFixed || version != "v2.0.0" {
			t.Fatalf("containment: %s %s %s", ref, commit, version)
		}
		return carried, nil
	}
	installed := false
	r.answer = func(args []string) (string, int) {
		if slices.Equal(args, loop.SelfUpdateCheck) && !installed {
			return loopNewRelease, ExitOutdated
		}
		if slices.Equal(args, []string{selfUpdateCmdName}) {
			installed = true
		}
		return "", 0
	}
	// Failed post-install blocks the wake. A failed containment lookup retries.
	r.a.cfg.Supervisor.PostInstall = []string{loopPostInstall}
	r.command = func([]string) (string, int) { return "", 1 }
	r.pass(t)
	if c := r.called(agentsName, agentWakeName, loopWorker); c != nil {
		t.Fatalf("wake before ready: %v", c)
	}
	r.command = func([]string) (string, int) { return "", 0 }
	loop.LoopReleaseContains = func(context.Context, string, string, string) (bool, error) { return false, errors.New("offline") }
	r.pass(t)
	if len(r.logged(t, "supervisor.release")) != 0 {
		t.Fatal("lost failed containment retry")
	}
	loop.LoopReleaseContains = func(context.Context, string, string, string) (bool, error) { return carried, nil }
	carried = true
	r.pass(t)
	c := r.called(agentsName, agentWakeName, loopWorker)
	if c == nil || !strings.Contains(c[3], "v2.0.0 is installed") || !strings.Contains(c[3], loopFixed) {
		t.Fatalf("missing installed wake: %v", r.calls)
	}
	r.pass(t)
	if c := r.called(agentsName, agentWakeName, loopWorker); c != nil {
		t.Fatalf("duplicate wake: %v", c)
	}
	if err := r.a.store.Record(event(state.Party{Name: loopWorker}, superloop.VerbDone, "%s done: release proven", loopWorker)); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	if c := r.called(agentsName, loop.LoopRemove, loopWorker); c == nil {
		t.Fatalf("reported worker retained: %v", r.calls)
	}
}

func TestLoopReleaseWakeSkipsReleasesWithoutTheMergeAndSurvivesRestart(t *testing.T) {
	r := newLoopRig(t)
	old := loop.LoopReleaseContains
	t.Cleanup(func() { loop.LoopReleaseContains = old })
	checks := 0
	loop.LoopReleaseContains = func(_ context.Context, _, _, version string) (bool, error) { checks++; return version == "v2.0.1", nil }
	l := &loop.Loop{Host: r.a, Stages: map[string]bool{}, PostMerge: []superloop.Action{{Ref: loopPR, URL: loopURL, Agent: loopWorker, Head: loopFixed}}}
	ctx := context.Background()
	v := superloop.Action{Key: "release v2.0.0", Version: "v2.0.0"}
	if err := l.WakeReleased(ctx, v); err != nil {
		t.Fatal(err)
	}
	if err := l.WakeReleased(ctx, v); err != nil {
		t.Fatal(err)
	}
	if checks != 1 || r.called(agentsName, agentWakeName) != nil {
		t.Fatalf("absent merge: %d %v", checks, r.calls)
	}
	v = superloop.Action{Key: "release v2.0.1", Version: "v2.0.1"}
	if err := l.WakeReleased(ctx, v); err != nil {
		t.Fatal(err)
	}
	if r.called(agentsName, agentWakeName, loopWorker) == nil {
		t.Fatal("no wake for containing release")
	}
	// Stages are reconstructed from the durable event log after re-exec.
	l.Stages = map[string]bool{}
	for _, e := range r.logged(t, loop.LoopStageVerb) {
		key, _, ok := loop.Keyed(e.Detail)
		if ok {
			l.Stages[key] = true
		}
	}
	r.calls = nil
	if err := l.WakeReleased(ctx, superloop.Action{Key: "release v2.0.2", Version: "v2.0.2"}); err != nil {
		t.Fatal(err)
	}
	if r.called(agentsName, agentWakeName) != nil || checks != 2 {
		t.Fatalf("wake repeated after restart: %d %v", checks, r.calls)
	}
}

// An agent that logs agents.done is still in that turn when the event hits
// the log: the loop waits for the turn to end before running agents remove,
// so stopBackgroundStart does not leave its background session running.
func TestLoopWaitsForAnIdleTurnBeforeRemovingADoneAgent(t *testing.T) {
	const sess = "w-in-turn"
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.started(t, loopWorker, sess)
	r.pull.State = superloop.Merged
	r.pass(t)
	done := event(state.Party{Name: loopWorker, Session: sess}, superloop.VerbDone, "%s done: fix %s", loopWorker, loopURL)
	report := event(state.Party{Name: loopWorker, Session: sess}, superloop.VerbReport, "%s: merged %s", loopWorker, loopURL)
	report.At = done.At.Add(10 * time.Microsecond)
	if err := r.a.store.Record(done, report); err != nil {
		t.Fatal(err)
	}
	r.inTurn = func(p state.Party) bool { return p.Session == sess }
	r.pass(t)
	if c := r.called(agentsName, loop.LoopRemove); c != nil {
		t.Fatalf("removed while its turn still ran: %v", c)
	}
	if evs := r.logged(t, "supervisor.done"); len(evs) != 0 {
		t.Fatalf("logged supervisor.done while its turn still ran: %+v", evs)
	}
	r.inTurn = func(state.Party) bool { return false }
	r.pass(t)
	if c := r.called(agentsName, loop.LoopRemove, loopWorker); c == nil {
		t.Fatalf("not removed once its turn ended: %v\n%s", r.calls, r.out)
	}
}

// loop.AgentRef finds the pull request ref from the worker's report after agents
// idle --done cleared its task and park, and KindFile settles without
// blocking KindDone on the following pass.
func TestLoopFileNotePassesNoteAddCheck(t *testing.T) {
	const task = "fix the gate"
	r := newLoopRig(t)
	r.a.as = loop.LoopParty.Name
	r.park(t, loopPR, time.Now())
	r.pull.State = superloop.Merged
	r.pass(t)
	// agents idle --done clears Task and Park, leaving LastTask without a URL
	// while the report carries the URL.
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Task, st.Agents[0].LastTask, st.Agents[0].Park, st.Agents[0].Done = "", task, nil, true
		st.WorkerReports = []state.WorkerReport{{By: st.Agents[0].Party, At: time.Now(), Task: task, Text: "merged " + loopURL}}
		if got := loop.AgentRef(st, loopWorker); got != loopURL {
			t.Fatalf("agentRef = %q, want %q", got, loopURL)
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	done := event(state.Party{Name: loopWorker}, superloop.VerbDone, "%s done: fix the gate", loopWorker)
	problem := event(state.Party{Name: loopWorker}, superloop.VerbProblem, "%s: o/r: the gate refuses a green head", loopWorker)
	problem.At = done.At.Add(10 * time.Microsecond)
	if err := r.a.store.Record(done, problem); err != nil {
		t.Fatal(err)
	}
	r.answer = func(args []string) (string, int) {
		if len(args) >= 2 && args[0] == loop.LoopNote && args[1] == loop.LoopAdd {
			var out bytes.Buffer
			cmd := r.a.noteCmd()
			cmd.SetArgs(args[1:])
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			if err := cmd.Execute(); err != nil {
				return out.String() + err.Error(), Code(err)
			}
			return out.String(), 0
		}
		return "", 0
	}
	r.pass(t)
	if evs := r.logged(t, loop.LoopFileVerb); len(evs) != 1 {
		t.Fatalf("KindFile was refused by note add: %+v\n%s", evs, r.out)
	}
	r.pass(t)
	if c := r.called(agentsName, loop.LoopRemove, loopWorker); c == nil {
		t.Fatalf("not taken off the roster after the file: %v\n%s", r.calls, r.out)
	}
}

// Each pass of the supervisor loop runs the doctor's roster and archive step:
// done workers, settled reviewers, relieved role runs and stale idle entries
// without a CLI leave the roster; their background sessions are stopped but
// a relieved role run's, which stays the person's to close, while active
// role holders, in-turn sessions and sessions the person started stay.
func TestLoopRunsTheDoctorsRosterAndArchiveStepEveryPass(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Agents.Mode = config.ModeBackground
	r.a.cfg.Agents.StaleAfter.Duration = 24 * time.Hour
	now := r.a.now

	doneWorker := state.Party{Session: "11111111-0000-4000-8000-000000000001", Name: loopWorker}
	reviewer := state.Party{Session: "22222222-0000-4000-8000-000000000002", Name: loopReviewer}
	guide3 := state.Party{Session: "33333333-0000-4000-8000-000000000003", Name: guideRole.runName(3)}
	guide4 := state.Party{Session: "44444444-0000-4000-8000-000000000004", Name: guideRole.runName(4)}
	guide5 := state.Party{Session: "55555555-0000-4000-8000-000000000005", Name: guideRole.runName(5)}
	staleIdle := state.Party{Session: "66666666-0000-4000-8000-000000000006", Name: "stale worker"}
	personOwn := state.Party{Session: "77777777-0000-4000-8000-000000000007", Name: "person session"}

	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		for _, p := range []state.Party{doneWorker, reviewer, guide3, guide4, guide5, staleIdle} {
			st.Starts = append(st.Starts, state.Start{Party: p, Mode: state.ModeBypass, Dir: t.TempDir(), By: loop.LoopParty, At: now.Add(-2 * time.Hour), Background: true})
		}
		st.Agents = []state.Agent{
			{Party: doneWorker, Done: true, IdleSince: now.Add(-time.Hour)},
			// guide3 is still on the roster with its role task although relieved.
			{Party: guide3, Task: guide3.Name},
			{Party: guide5, Task: guide5.Name},
			{Party: staleIdle, IdleSince: now.Add(-48 * time.Hour)},
			{Party: personOwn, IdleSince: now.Add(-48 * time.Hour)},
		}
		st.SetGuideRole(state.Role{
			Holder: &state.Supervisor{Party: guide5, Since: now.Add(-time.Hour)},
			Run:    5,
			Relieved: []state.Relief{
				{Party: guide3, By: guide4, At: now.Add(-2 * time.Hour), Taken: now.Add(-2 * time.Hour)},
				{Party: guide4, By: guide5, At: now.Add(-time.Hour), Taken: now.Add(-time.Hour)},
			},
		})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	calls := stubBackground(t, "",
		bgSession{ID: doneWorker.Session[:8], SessionID: doneWorker.Session, Kind: bgKind},
		bgSession{ID: reviewer.Session[:8], SessionID: reviewer.Session, Kind: bgKind},
		bgSession{ID: guide3.Session[:8], SessionID: guide3.Session, Kind: bgKind},
		bgSession{ID: guide4.Session[:8], SessionID: guide4.Session, Kind: bgKind},
		bgSession{ID: guide5.Session[:8], SessionID: guide5.Session, Kind: bgKind},
		bgSession{ID: personOwn.Session[:8], SessionID: personOwn.Session, Kind: bgKind},
	)
	// personOwn has a live CLI; staleIdle has no CLI.
	r.sessions = []*claude.Session{
		{PID: 101, ID: doneWorker.Session, Name: doneWorker.Name, Background: true},
		{PID: 102, ID: reviewer.Session, Name: reviewer.Name, Background: true},
		{PID: 103, ID: guide3.Session, Name: guide3.Name, Background: true},
		{PID: 104, ID: guide4.Session, Name: guide4.Name, Background: true},
		{PID: 105, ID: guide5.Session, Name: guide5.Name, Background: true},
		{PID: 107, ID: personOwn.Session, Name: personOwn.Name},
	}
	// While doneWorker and guide4 still run a turn, neither is stopped yet.
	r.inTurn = func(p state.Party) bool {
		return p.Session == doneWorker.Session || p.Session == guide4.Session
	}
	r.pass(t)

	stopped := func() []string {
		var ids []string
		for _, c := range *calls {
			if len(c.argv) == 3 && c.argv[1] == bgStopCmd {
				ids = append(ids, c.argv[2])
			}
		}
		slices.Sort(ids)
		return ids
	}
	if got, want := stopped(), []string{reviewer.Session[:8], staleIdle.Session[:8]}; !slices.Equal(got, want) {
		t.Fatalf("stopped on first pass = %v, want %v", got, want)
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	var roster []string
	for _, ag := range st.Agents {
		roster = append(roster, ag.Name)
	}
	if want := []string{doneWorker.Name, guide5.Name, personOwn.Name}; !slices.Equal(roster, want) {
		t.Fatalf("roster after first pass = %v, want %v", roster, want)
	}

	// Their turns end: doneWorker leaves the roster and stops; the relieved
	// guide4 stays.
	*calls = nil
	r.sessions = []*claude.Session{
		{PID: 101, ID: doneWorker.Session, Name: doneWorker.Name, Background: true},
		{PID: 104, ID: guide4.Session, Name: guide4.Name, Background: true},
		{PID: 105, ID: guide5.Session, Name: guide5.Name, Background: true},
		{PID: 107, ID: personOwn.Session, Name: personOwn.Name},
	}
	r.inTurn = func(state.Party) bool { return false }
	r.pass(t)
	if got, want := stopped(), []string{doneWorker.Session[:8]}; !slices.Equal(got, want) {
		t.Fatalf("stopped on second pass = %v, want %v", got, want)
	}
	st, err = r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	roster = nil
	for _, ag := range st.Agents {
		roster = append(roster, ag.Name)
	}
	if want := []string{guide5.Name, personOwn.Name}; !slices.Equal(roster, want) {
		t.Fatalf("roster after second pass = %v, want %v", roster, want)
	}
}

func TestSupervisorLoopPlainWaitReadsNoPullAndStartsNoReview(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Park.ReviewHead = ""
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	loop.ReadLoopPull = func(context.Context, string) (superloop.Pull, error) {
		t.Fatal("plain wait read for review")
		return superloop.Pull{}, nil
	}
	r.pass(t)
	if slices.ContainsFunc(r.calls, func(args []string) bool { return len(args) > 0 && args[0] == agentsName }) || strings.Contains(r.out.String(), "SUPERVISOR WOULD") {
		t.Fatalf("plain wait acted: %s %v", r.out, r.calls)
	}
}

// A PR URL makes a legacy wait operational, but only publication evidence
// authorizes review, whether the PR is explicit or inferred from the wait.
func TestLoopOwnsPullRequestWaitsWithoutOn(t *testing.T) {
	for _, green := range []bool{false, true} {
		for _, verified := range []bool{false, true} {
			for _, on := range []string{"", loopPR} {
				t.Run(fmt.Sprintf("green=%t/verified=%t/on=%s", green, verified, on), func(t *testing.T) {
					r := newLoopRig(t)
					r.park(t, on, time.Now())
					r.pull.Green = green
					err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
						ag := &st.Agents[0]
						ag.Park.Waits = "CI pending for " + loopURL
						if !verified {
							ag.Park.ReviewHead = ""
						}
						return []state.Event{event(ag.Party, "agents.park", "%s: %s", ag.Name, parkedText(ag.Park, r.a.now))}, nil
					})
					if err != nil {
						t.Fatal(err)
					}
					if !verified {
						loop.ReadLoopPull = func(context.Context, string) (superloop.Pull, error) {
							t.Fatal("unverified wait read for review")
							return superloop.Pull{}, nil
						}
					}
					r.pass(t)
					if note := r.called("note", loop.LoopAdd); note != nil {
						t.Fatalf("operational park got a note: %v", note)
					}
					if got := r.called(agentsName, "start"); (got != nil) != (green && verified) {
						t.Fatalf("review start = %v, green=%t verified=%t\n%s", got, green, verified, r.out)
					}
				})
			}
		}
	}
}

func TestLoopParkOnKeepsExplicitAndAmbiguousWaits(t *testing.T) {
	const noteOn = "#1234"
	for _, tc := range []struct{ on, waits, want string }{
		{noteOn, "CI pending " + loopURL, noteOn},
		{"", "desk review " + loopURL, loopPR},
		{"", "CI pending " + loopURL + " and " + loopURL, loopPR},
		{"", "waiting https://github.com/o/r/issues/7", ""},
		{"", "CI pending " + loopURL + " https://github.com/o/r/pull/8", ""},
		{"", "waiting for the person's rollout decision", ""},
	} {
		if got := loop.LoopParkOn(&state.Park{On: tc.on, Waits: tc.waits}); got != tc.want {
			t.Errorf("%+v: got %q", tc, got)
		}
	}
}

// hostsFirstRig is a loop under supervisor.hostsFirst whose next item, on
// the host's mapped repository, is due: choices are capacity.choices, and
// answer the capacity verdicts, local (with the --exclude flags it names)
// and the host's.
func hostsFirstRig(t *testing.T, choices []config.Choice, answer func(host bool, exclude []string) string) *loopRig {
	t.Helper()
	r := newLoopRig(t)
	nextBrief(t, r)
	r.a.cfg.Supervisor.HostsFirst = true
	r.sshLoopHost(t, true)
	r.a.cfg.Capacity.Choices = choices
	r.park(t, loopPR, time.Now())
	r.pull.State = superloop.Merged
	r.pass(t)
	loopNextDone(t, r)
	branch := loop.GhDefaultBranch
	t.Cleanup(func() { loop.GhDefaultBranch = branch })
	loop.GhDefaultBranch = func(context.Context, string) (string, error) { return mainBranch, nil }
	r.answer = func(args []string) (string, int) {
		switch args[0] {
		case loop.LoopCapacity:
			var exclude []string
			for i, a := range args {
				if a == "--"+excludeFlag && i+1 < len(args) {
					exclude = append(exclude, args[i+1])
				}
			}
			return answer(slices.Contains(args, loop.LoopHostFlag), exclude), 0
		case loop.LoopBoard:
			return loopNextPickJSON, 0
		}
		return "", 0
	}
	return r
}

// mixedChoices is a desk with a Claude choice a host runs and a codex one
// no host has an adapter for.
var mixedChoices = []config.Choice{{Harness: claudeComm, Model: bgModel}, {Harness: codex.Harness, Model: "gpt-fixture"}}

// When no host has room, local fallback retains every eligible choice.
func TestLoopHostsFirstStartsLocalOnlyKindsLocally(t *testing.T) {
	r := hostsFirstRig(t, mixedChoices, func(host bool, exclude []string) string {
		if host {
			return loopNoRoomJSON
		}
		return loopRoomJSON
	})
	r.pass(t)
	start := r.called(agentsName, agentStartName, loop.RepoName(sshTestRepository)+" 9")
	if start == nil {
		t.Fatalf("the codex-eligible start waited for a Claude-only host: %v\n%s", r.calls, r.out)
	}
	if slices.Contains(start, loop.LoopHostFlag) {
		t.Errorf("a full host took the start: %v", start)
	}
	if slices.Contains(start, "--"+excludeFlag) {
		t.Errorf("local fallback must preserve capacity choices: %v", start)
	}
	if !slices.Contains(start, capacity.HarnessAuto) {
		t.Errorf("the local start loses capacity's pick: %v", start)
	}
}

// Local fallback must not exclude an eligible subscription choice.
func TestLoopHostsFirstNeverDowngradesToASelfHostedChoice(t *testing.T) {
	const selfHosted = "local"
	choices := []config.Choice{{Harness: claudeComm, Model: bgModel}, {Harness: config.OmpHarness, Model: "local/qwen-fixture", Provider: selfHosted, Kinds: []string{workerRole}}}
	verdict := func(provider string) string {
		harness := claudeComm
		if provider == selfHosted {
			harness = config.OmpHarness
		}
		return `{"ceiling": 1, "room": 1, "choice": {"harness": "` + harness + `", "provider": "` + provider + `"}}`
	}
	for _, eligible := range []bool{true, false} {
		r := hostsFirstRig(t, choices, func(host bool, exclude []string) string {
			switch {
			case host:
				return loopNoRoomJSON
			case len(exclude) > 0 || !eligible:
				return verdict(selfHosted)
			}
			return verdict(claudeComm)
		})
		r.a.cfg.Capacity.Providers = map[string]config.Provider{claudeComm: {Ceiling: 2}, selfHosted: {Kind: config.KindSelfHosted, Ceiling: 2}}
		r.pass(t)
		start := r.called(agentsName, agentStartName, loop.RepoName(sshTestRepository)+" 9")
		if start != nil && slices.Contains(start, "--"+excludeFlag) {
			t.Errorf("fallback changed the subscription pick: %v", start)
		}
		if start == nil {
			t.Fatalf("no subscription eligible: the self-hosted start waited: %v\n%s", r.calls, r.out)
		}
		if slices.Contains(start, loop.LoopHostFlag) {
			t.Errorf("a full host took the start: %v", start)
		}
	}
}

// The same mixed queue with a host that has room: the start goes there, as
// hostsFirst prefers, and names no exclusion.
func TestLoopHostsFirstPrefersAHostWithRoomForAMixedQueue(t *testing.T) {
	r := hostsFirstRig(t, mixedChoices, func(host bool, exclude []string) string { return loopRoomJSON })
	r.pass(t)
	start := r.called(agentsName, agentStartName, loop.RepoName(sshTestRepository)+" 9")
	if start == nil || !slices.Contains(start, loop.LoopHostFlag) {
		t.Fatalf("a host with room did not take the start: %v\n%s", start, r.out)
	}
	if slices.Contains(start, "--"+excludeFlag) {
		t.Errorf("a remote start excludes choices: %v", start)
	}
}

// With the local machine full for the local-only kinds (capacity.local.
// ceiling, or no eligible codex choice), the mixed queue waits like a
// Claude-only one.
func TestLoopHostsFirstWaitsWithoutLocalRoomForLocalOnlyKinds(t *testing.T) {
	r := hostsFirstRig(t, mixedChoices, func(host bool, exclude []string) string {
		return loopNoRoomJSON
	})
	r.pass(t)
	r.pass(t)
	if c := r.called(agentsName, agentStartName); c != nil {
		t.Errorf("a start with no local room for codex and no host room started %v", c)
	}
}

// Full hosts leave the ordinary local capacity available as fallback.
func TestLoopHostsFirstFallsBackWhenEveryKindRunsThere(t *testing.T) {
	r := hostsFirstRig(t, []config.Choice{{Harness: claudeComm, Model: bgModel}}, func(host bool, exclude []string) string {
		if host {
			return loopNoRoomJSON
		}
		return loopRoomJSON
	})
	r.pass(t)
	if c := r.called(agentsName, agentStartName); c == nil || slices.Contains(c, loop.LoopHostFlag) {
		t.Errorf("full hosts did not leave a local fallback: %v", c)
	}
}

// A repository no host maps: no host can run it, so hostsFirst starts it
// locally within the local room, a Claude choice included, and waits
// only while the local machine has none.
func TestLoopHostsFirstStartsAnUnmappedRepositoryLocally(t *testing.T) {
	for _, room := range []bool{true, false} {
		r := hostsFirstRig(t, []config.Choice{{Harness: claudeComm, Model: bgModel}}, func(host bool, exclude []string) string {
			if host {
				return loopRoomJSON
			}
			if room {
				return loopRoomJSON
			}
			return loopNoRoomJSON
		})
		h := r.a.cfg.Agents.Hosts[sshTestSelector]
		h.Repositories = map[string]string{"example/elsewhere": "/srv/elsewhere"}
		r.a.cfg.Agents.Hosts[sshTestSelector] = h
		r.pass(t)
		start := r.called(agentsName, agentStartName, loop.RepoName(sshTestRepository)+" 9")
		if !room {
			if start != nil {
				t.Errorf("no local room: started %v", start)
			}
			continue
		}
		if start == nil {
			t.Fatalf("the unmapped repository waited for a host: %v\n%s", r.calls, r.out)
		}
		if slices.Contains(start, loop.LoopHostFlag) || slices.Contains(start, "--"+excludeFlag) {
			t.Errorf("the unmapped repository's start = %v, want a plain local start", start)
		}
	}
}

// Reviewers fall back with unchanged choices and review capacity flags.
func TestLoopHostsFirstReviewerPreservesChoices(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Supervisor.HostsFirst = true
	r.sshLoopHost(t, true)
	r.a.cfg.Capacity.Choices = mixedChoices
	pr := sshTestRepository + "#7"
	r.park(t, pr, time.Now())
	r.pulls[pr] = superloop.Pull{Ref: pr, URL: "https://github.com/" + sshTestRepository + "/pull/7", State: superloop.Open, Head: loopHead, Green: true, Author: loopDesk}
	r.answer = func(args []string) (string, int) {
		if args[0] == loop.LoopCapacity && slices.Contains(args, loop.LoopHostFlag) {
			return loopNoRoomJSON, 0
		}
		if args[0] == loop.LoopCapacity {
			return loopRoomJSON, 0
		}
		return "", 0
	}
	r.pass(t)
	var start []string
	for _, c := range r.calls {
		if len(c) > 2 && c[0] == agentsName && c[1] == agentStartName && slices.Contains(c, "--"+reviewFlag) {
			start = c
		}
	}
	if start == nil {
		t.Fatalf("no reviewer started: %v\n%s", r.calls, r.out)
	}
	if slices.Contains(start, loop.LoopHostFlag) || slices.Contains(start, "--"+excludeFlag) {
		t.Errorf("the reviewer start = %v, want local with unchanged choices", start)
	}
}

// https://github.com/giantswarm/beekeeper/issues/1927: a worker on an SSH
// execution host parks on the pull request it published through the
// coordinator; its park at the published head is a review request, and a
// park after a new publication asks for that head's review.
func TestLoopReviewsARemoteWorkersPublishedPark(t *testing.T) {
	r := newLoopRig(t)
	ref := execution.Ref{Host: "host-a", Execution: "remote-one", Generation: 1}
	party := state.Party{Name: loopWorker, Session: "remote-session", Host: string(ref.Host)}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, state.Agent{Party: party, Task: "fix " + loopURL, Execution: &ref})
		st.Starts = append(st.Starts, state.Start{Party: party, Execution: &state.ExecutionRecord{Handle: execution.Handle{Ref: ref, Repository: scratchRepo, Commit: loopFixed}, Request: execution.StartRequest{Ref: ref, Repository: scratchRepo}}})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	was := executionReviewGH
	t.Cleanup(func() { executionReviewGH = was })
	executionReviewGH = func(context.Context, ...string) ([]byte, error) {
		return fmt.Appendf(nil, `{"head":{"ref":%q,"sha":%q,"repo":{"full_name":"o/r"}}}`, executionBranch(ref), r.pull.Head), nil
	}
	// A remote worker's round binds the reviewer the loop started.
	r.answer = func(args []string) (string, int) {
		if len(args) > 2 && args[0] == agentsName && args[1] == agentStartName {
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Starts = append(st.Starts, state.Start{Party: state.Party{Name: args[2], Session: "reviewer-" + args[2]}, By: loop.LoopParty, At: time.Now()})
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		return "", 0
	}
	park := func(head string) {
		t.Helper()
		if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
			if !slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(party) }) {
				st.Agents = append(st.Agents, state.Agent{Party: party, Task: "fix " + loopURL, Execution: &ref})
			}
			_, evs, err := r.a.applyExecutionControl(context.Background(), st, party, execution.ControlRequest{Operation: execution.ControlPark, Data: mustExecutionJSON(t, executionPark{On: loopPR, Waits: deskReviewWait, Head: head})})
			return evs, err
		}); err != nil {
			t.Fatal(err)
		}
	}
	park(loopHead)
	r.pass(t)
	start := r.called(agentsName, agentStartName)
	if start == nil || start[2] != loopReviewer {
		t.Fatalf("no review of the remote worker's published head: %v\n%s", r.calls, r.out)
	}
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.pass(t)
	// The approval reaches the remote go (agents merge --authorization);
	// its grant needs the reviewer's bound report, which
	// executionmerge_review_test.go covers, and the rig has none.
	if !strings.Contains(r.out.String(), "go "+loopPR+"@"+loopHead) {
		t.Fatalf("the approval took no go: %v\n%s", r.calls, r.out)
	}
	// The worker publishes one more commit and parks again at it.
	r.pull.Head = loopFixed
	park(loopFixed)
	r.pass(t)
	start = r.called(agentsName, agentStartName)
	if start == nil || start[2] != superloop.ReviewerName(loopPR, 2) {
		t.Fatalf("no review of the remote worker's new head: %v\n%s", r.calls, r.out)
	}
	if evs := r.logged(t, "supervisor.review"); len(evs) != 2 || !strings.Contains(evs[1].Detail, loopFixed) {
		t.Errorf("review logged as %+v", evs)
	}
	r.pass(t)
	if c := r.called(agentsName, agentStartName); c != nil {
		t.Errorf("one head got a second review: %v", c)
	}
}

// TestLoopKeepsAMultiPullRemoteWorkerUntilItsWorkCloses: a worker, here a
// managed SSH execution, whose first pull request merged stays on the
// roster while it serves an open issue and parks on its next open pull
// request; once both closed it is stopped and removed.
func TestLoopKeepsAMultiPullRemoteWorkerUntilItsWorkCloses(t *testing.T) {
	const issue, next = "o/r#5", "o/r#8"
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.started(t, loopWorker, "worker-session")
	r.pass(t)
	reviewer := r.called(agentsName, agentStartName)[2]
	r.started(t, reviewer, "reviewer-session")
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.pass(t)
	if c := r.called(gateCmdName); c == nil {
		t.Fatalf("no merge go: %v\n%s", r.calls, r.out)
	}
	ref := execution.Ref{Host: execution.HostID(sshTestSelector), Execution: execution.ExecutionID("11112222-3333-4444-5555-666677778888"), Generation: 1}
	err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == loopWorker })
		st.Agents[i].Execution = &ref
		st.Agents[i].Park = &state.Park{On: next, At: time.Now(), Waits: "desk review of the next pull request"}
		st.Records = append(st.Records, state.Record{Session: st.Agents[i].Party, Issue: "https://github.com/o/r/issues/5", By: st.Agents[i].Party, At: time.Now()})
		st.Starts = append(st.Starts, state.Start{Party: st.Agents[i].Party, By: loop.LoopParty, At: time.Now(), Execution: &state.ExecutionRecord{Handle: execution.Handle{Ref: ref}, Request: execution.StartRequest{Ref: ref}, Phase: execution.Running}})
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	open := map[string]bool{issue: true, next: true}
	refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
		out := map[github.PR]github.RefState{}
		for _, p := range refs {
			s := github.Closed
			if open[refName(p)] {
				s = github.Open
			}
			out[p] = github.RefState{Kind: github.RefPullRequest, State: s}
		}
		return out, nil
	}
	r.pull.State = superloop.Merged
	r.a.cfg.Supervisor.PostMergeWindow.Duration = time.Nanosecond
	for range 2 {
		r.pass(t)
		for _, verb := range []string{loop.LoopStop, loop.LoopRemove} {
			if c := r.called(agentsName, verb, loopWorker); c != nil {
				t.Fatalf("retired while it serves open work: %v\n%s", c, r.out)
			}
		}
	}
	if evs := r.logged(t, "supervisor.leave"); len(evs) != 1 || !strings.Contains(evs[0].Detail, "stays on the roster") || !strings.Contains(evs[0].Detail, next) {
		t.Fatalf("logged %+v", evs)
	}
	if evs := r.logged(t, "supervisor.done"); slices.ContainsFunc(evs, func(e state.Event) bool { return strings.Contains(e.Detail, loopWorker) }) {
		t.Fatalf("done logged: %+v", evs)
	}
	// Its served issue and its next pull request close: it goes.
	open[issue], open[next] = false, false
	r.pass(t)
	if c := r.called(agentsName, loop.LoopStop, loopWorker); c == nil {
		t.Fatalf("the remote worker was not stopped: %v\n%s", r.calls, r.out)
	}
	if c := r.called(agentsName, loop.LoopRemove, loopWorker); c == nil {
		t.Fatalf("the remote worker stays: %v\n%s", r.calls, r.out)
	}
}

// https://github.com/giantswarm/beekeeper/issues/1921: a remote (SSH
// execution) worker has no local session or process, so post-merge
// retirement and KindDone must consult its execution record and host
// observation rather than retiring it mid-turn once postMergeWindow passes.
func TestLoopRetainsActiveOrInTurnRemoteWorkerAfterPostMergeWindow(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, r.a.now)
	r.started(t, loopWorker, "worker-session")
	r.pass(t)
	reviewer := r.called(agentsName, agentStartName)[2]
	r.started(t, reviewer, "reviewer-session")
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.pass(t)
	if c := r.called(gateCmdName); c == nil {
		t.Fatalf("no merge go: %v\n%s", r.calls, r.out)
	}

	ref := execution.Ref{Host: execution.HostID(sshTestSelector), Execution: execution.ExecutionID("22223333-4444-5555-6666-777788889999"), Generation: 1}
	old := time.Now().Add(-10 * time.Minute)
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == loopWorker })
		st.Agents[i].Execution = &ref
		st.Agents[i].Park = nil
		for j := range st.Starts {
			if st.Starts[j].Name == loopWorker {
				st.Starts[j].Execution = &state.ExecutionRecord{
					Handle:      execution.Handle{Ref: ref},
					Request:     execution.StartRequest{Ref: ref},
					Phase:       execution.Running,
					LastActive:  old,
					Observation: execution.Observation{Handle: execution.Handle{Ref: ref}, ProcessState: execution.Running, Reachability: execution.Reachable, ObservedAt: old, LastActive: old, Ready: true},
				}
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	r.pull.State = superloop.Merged
	r.a.cfg.Supervisor.PostMergeWindow.Duration = time.Nanosecond
	r.pass(t) // records [end o/r#7]

	assertRetained := func(step string) {
		t.Helper()
		for _, verb := range []string{loop.LoopStop, loop.LoopRemove} {
			if c := r.called(agentsName, verb, loopWorker); c != nil {
				t.Fatalf("%s: remote worker retired early (%s): %v\n%s", step, verb, c, r.out)
			}
		}
	}

	// 1. Past end.At + postMergeWindow, but the execution recorded activity
	// within postMergeWindow: Plan does not schedule retirement.
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		rec, _ := executionRecord(st, ref)
		rec.LastActive = time.Now().Add(time.Minute)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	assertRetained("recent execution activity")

	// 2. Past lastActive + postMergeWindow, but the host is unreachable:
	// a failed observation never retires a post-merge worker.
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		rec, _ := executionRecord(st, ref)
		rec.LastActive = old
		rec.Observation.Reachability = execution.Unreachable
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	assertRetained("unreachable execution host")

	// 3. Host reachable again and past lastActive + postMergeWindow, but the
	// remote session is still in a turn (Observation.InTurn): KindDone waits.
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		rec, _ := executionRecord(st, ref)
		rec.Observation.Reachability = execution.Reachable
		rec.Observation.InTurn = true
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	assertRetained("host observation inTurn")
	if !strings.Contains(r.out.String(), "SUPERVISOR DONE WAITS "+loopWorker+": its session runs a turn") {
		t.Fatalf("missing in-turn wait message:\n%s", r.out)
	}

	// 4. Observation.InTurn is false, but an in-flight wake message is
	// uncertain: KindDone still waits for the remote turn.
	r.out.Reset()
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		rec, _ := executionRecord(st, ref)
		rec.Observation.InTurn = false
		rec.Messages = []state.ExecutionMessage{{Key: "wake-1", Stage: execution.Uncertain}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	assertRetained("uncertain wake message")

	// 5. No message is in flight, but the coordinator recorded turn activity
	// within stewardQuiet (while PostMergeWindow=1ns has elapsed): KindDone waits.
	r.out.Reset()
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		rec, _ := executionRecord(st, ref)
		rec.Messages = nil
		rec.LastActive = time.Now().Add(-time.Millisecond)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	assertRetained("recent coordinator callback activity within stewardQuiet")

	// 6. While in a turn (Observation.InTurn=true), the remote worker calls
	// agents idle --done --report through the coordinator. A supervisor pass
	// before the next host resync retains the worker while the harness
	// finishes its final response.
	r.out.Reset()
	reportData := mustExecutionJSON(t, executionReport{
		Finished: true,
		Text:     "https://github.com/o/r/pull/7 merged; Problems found: none",
		Problems: []string{ghNone},
	})
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == loopWorker })
		rec, _ := executionRecord(st, ref)
		rec.Observation.InTurn = true
		_, evs, err := r.a.applyExecutionControl(context.Background(), st, st.Agents[i].Party, execution.ControlRequest{
			Operation: execution.ControlReport,
			Data:      reportData,
		})
		return evs, err
	}); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	assertRetained("in-turn remote worker after reporting done before host resync")
	if !strings.Contains(r.out.String(), "SUPERVISOR DONE WAITS "+loopWorker+": its session runs a turn") {
		t.Fatalf("missing in-turn wait message after report:\n%s", r.out)
	}

	// 7. A subsequent idle/terminal host observation clears InTurn: the loop
	// stops the execution and removes the worker from the roster.
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		rec, _ := executionRecord(st, ref)
		rec.Observation.InTurn = false
		rec.LastActive = old
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	if c := r.called(agentsName, loop.LoopStop, loopWorker); c == nil {
		t.Fatalf("the remote worker was not stopped after its turn ended: %v\n%s", r.calls, r.out)
	}
	if c := r.called(agentsName, loop.LoopRemove, loopWorker); c == nil {
		t.Fatalf("the remote worker was not removed after its turn ended: %v\n%s", r.calls, r.out)
	}
}

// A claim waiting for review capacity must not disappear silently when its
// parked worker is removed. The warning survives loop restarts.
func TestLoopLogsAnOrphanedPendingReviewOnce(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, r.a.now)
	r.answer = func(args []string) (string, int) {
		if args[0] == agentsName && args[1] == agentStartName {
			return "capacity: no start", ExitRefused
		}
		return "", 0
	}
	for range 2 {
		r.pass(t)
	}
	if evs := r.logged(t, "supervisor.leave"); len(evs) != 0 {
		t.Fatalf("parked pending review reported as orphaned: %+v", evs)
	}
	if len(r.logged(t, loop.LoopClaimVerb)) == 0 || len(r.logged(t, "supervisor.review")) != 0 {
		t.Fatal("expected an uncompleted review claim while capacity refuses")
	}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = nil
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		r.pass(t)
	}
	evs := r.logged(t, "supervisor.leave")
	if len(evs) != 1 || !strings.Contains(evs[0].Detail, "orphaned") || !strings.Contains(evs[0].Detail, loopURL) || !strings.Contains(evs[0].Detail, loopHead) {
		t.Fatalf("pending review disappeared without one orphan warning: %+v", evs)
	}
	// Reporting an orphan must not consume the pending review: a new park
	// still starts the original review when capacity becomes available.
	r.answer = func([]string) (string, int) { return "", 0 }
	r.park(t, loopPR, time.Now())
	r.pass(t)
	if r.called(agentsName, agentStartName) == nil {
		t.Fatalf("review lost after re-park: %v", r.calls)
	}
}

// An unchanged refusal is retried less and less often, never every five
// minutes all day.
func TestLoopFailureDelayGrowsToAnHour(t *testing.T) {
	for count, want := range map[int]time.Duration{2: 5 * time.Minute, 3: 10 * time.Minute, 4: 20 * time.Minute, 5: 40 * time.Minute, 6: time.Hour, 50: time.Hour} {
		if got := loop.LoopFailureDelay(count); got != want {
			t.Fatalf("delay after %d failures: %s, want %s", count, got, want)
		}
	}
}

// A look for a newer release that fails is said, once until a look
// succeeds again, never taken for "this is the latest".
func TestLoopSaysAFailedReleaseLookup(t *testing.T) {
	r := newLoopRig(t)
	failing := true
	r.answer = func(args []string) (string, int) {
		if slices.Equal(args, loop.SelfUpdateCheck) && failing {
			return "beekeeper: looking up the latest release: no signed binary found\n", ExitError
		}
		return "", 0
	}
	l := &loop.Loop{Host: r.a, Said: map[string]bool{}}
	for range 2 {
		if v := l.NewerRelease(context.Background()); v != "" {
			t.Fatalf("a failed look found %q", v)
		}
	}
	const failed = "SUPERVISOR RELEASE LOOKUP FAILED: self-update --check exits 1: beekeeper: looking up the latest release: no signed binary found\n"
	if r.out.String() != failed {
		t.Fatalf("said %q, want %q once", r.out, failed)
	}
	// A look that succeeds clears it: the next failure is said again.
	failing = false
	l.NewerRelease(context.Background())
	failing = true
	l.NewerRelease(context.Background())
	if r.out.String() != failed+failed {
		t.Fatalf("said %q, want the failure again after a success", r.out)
	}
}

func TestLoopWorkerAParksOnPRBWhoseOwnerIsGoneStartsOwnerForB(t *testing.T) {
	r := newLoopRig(t)
	r.pull.Closes = []string{loopClosingIssue}
	r.a.cfg.Board.Owner = "o"
	r.a.cfg.Board.Project = 1
	oldRefStates, oldHead := refStates, parkReviewHead
	t.Cleanup(func() { refStates, parkReviewHead = oldRefStates, oldHead })
	refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
		out := map[github.PR]github.RefState{}
		for _, p := range refs {
			out[p] = github.RefState{Kind: github.RefPullRequest, State: github.Open}
		}
		return out, nil
	}
	parkReviewHead = func(string) (string, string) { return "", parkVerificationUnavailable }

	me := state.Party{Name: "worker A", Session: "session-a"}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.ReviewParks = []state.ReviewPark{
			{On: loopPR, Agent: "worker B", ReviewHead: loopHead, Serves: loopClosingIssue, At: r.a.now.Add(-5 * time.Minute)},
		}
		st.Agents = []state.Agent{{
			Party: me,
			Task:  loopIssueTask,
		}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	// Worker A plain-parks on PR B (loopPR) because PR B overlaps its task;
	// this must not delete PR B's ReviewParks entry or steal its Serves.
	if _, err := r.a.park(me, loopPR, "waiting on overlapping PR"); err != nil {
		t.Fatalf("park: %v", err)
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if rp, ok := st.ReviewParkFor(loopPR); !ok || rp.Agent != "worker B" {
		t.Fatalf("plain waiter overwrote ReviewParks[%s]: %+v", loopPR, st.ReviewParks)
	}
	// PR B's owner is now gone: next loop pass sees worker A waiting on PR B,
	// records o/r#10 blocked by closing issue o/r#70 and starts an owner for PR B.
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.ReviewParks = nil
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	if c := r.called("board", "block", "o/r#10", "--by", loopClosingIssue); c == nil {
		t.Fatalf("expected board block o/r#10 --by o/r#70, got calls %v\n%s", r.calls, r.out)
	}
	if r.called(loop.LoopBoard, loopUnblock, loopClosingIssue, "--by", "o/r#10") == nil {
		t.Fatalf("inverse dependency must use closing issue: %v", r.calls)
	}
	owner := superloop.PullWorkerName(loopPR)
	if start := r.called(agentsName, agentStartName, owner); start == nil {
		t.Fatalf("expected agents start %s, got calls %v\n%s", owner, r.calls, r.out)
	}
	if evs := r.logged(t, "supervisor.owner"); len(evs) != 1 || !strings.Contains(evs[0].Detail, loopURL) {
		t.Fatalf("expected supervisor.owner logged, got %+v", evs)
	}
}

// Issue-less PRs must not trap owner assignment in board unblock retries.
func TestLoopOverlapWithoutClosingIssueAssignsOwner(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Board.Owner, r.a.cfg.Board.Project = "o", 1
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: state.Party{Name: loopWaitingIssueWorker, Session: loopOverlapSession}, Task: loopIssueTask, Park: &state.Park{On: loopPR, At: r.a.now, Waits: loopOverlapWait}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.answer = func(args []string) (string, int) {
		if args[0] == loop.LoopBoard && (args[1] == loopUnblock || args[1] == "block") {
			return "is a pull request: a hold is recorded on an issue", 3
		}
		return "", 0
	}
	r.pass(t)
	if r.called(loop.LoopBoard, loopUnblock) != nil || r.called(loop.LoopBoard, "block") != nil {
		t.Fatalf("attempted issue dependency for issue-less PR: %v", r.calls)
	}
	if r.called(agentsName, agentStartName, superloop.PullWorkerName(loopPR)) == nil || len(r.logged(t, "supervisor.owner")) != 1 {
		t.Fatalf("owner not assigned: %v\n%s", r.calls, r.out)
	}
	if len(r.logged(t, loop.LoopFailureVerb)) != 0 {
		t.Fatalf("owner failed: %s", r.out)
	}
}

func TestParseLoopPullClosingIssueAliases(t *testing.T) {
	p, err := loop.ParseLoopPull(loopPR, []byte(`{"closingIssuesReferences":[{"url":"https://github.com/o/r/issues/101"},{"url":"https://github.com/o/other/issues/102"}]}`))
	if err != nil || !slices.Equal(p.Closes, []string{"o/r#101", "o/other#102"}) {
		t.Fatalf("closing issues = %v, %v", p.Closes, err)
	}
}

func TestLoopOverlapDependencyFailureRetriesBeforeAssigningOwner(t *testing.T) {
	for _, operation := range []string{loopUnblock, "block"} {
		t.Run(operation, func(t *testing.T) {
			r := newLoopRig(t)
			r.pull.Closes = []string{loopClosingIssue}
			r.a.cfg.Board.Owner, r.a.cfg.Board.Project = "o", 1
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents = []state.Agent{{Party: state.Party{Name: loopWaitingIssueWorker, Session: loopOverlapSession}, Task: loopIssueTask, Park: &state.Park{On: loopPR, At: r.a.now, Waits: loopOverlapWait}}}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			r.answer = func(args []string) (string, int) {
				if args[0] == loop.LoopBoard && args[1] == operation {
					return "dependency API unavailable", 1
				}
				return "", 0
			}
			r.pass(t)
			if got := r.logged(t, "supervisor.owner"); len(got) != 0 {
				t.Fatalf("dependency failure recorded as taken: %+v", got)
			}
			if got := r.logged(t, loop.LoopFailureVerb); len(got) != 1 || !strings.Contains(got[0].Detail, "dependency") {
				t.Fatalf("dependency failure not reported: %+v", got)
			}
			if r.called(agentsName, agentStartName) != nil || r.called(agentsName, agentWakeName) != nil {
				t.Fatalf("assigned owner before dependency confirmation: %v", r.calls)
			}
			if operation == loopUnblock && r.called(loop.LoopBoard, "block") != nil {
				t.Fatalf("continued after failed unblock: %v", r.calls)
			}
			r.answer = func([]string) (string, int) { return "", 0 }
			r.pass(t)
			if r.called(loop.LoopBoard, "block", "o/r#10", "--by", loopClosingIssue) == nil || r.called(agentsName, agentStartName, superloop.PullWorkerName(loopPR)) == nil {
				t.Fatalf("retry did not record dependency then assign owner: %v", r.calls)
			}
			if got := r.logged(t, "supervisor.owner"); len(got) != 1 {
				t.Fatalf("retry did not record success: %+v", got)
			}
		})
	}
}

// Scheduled exponential backoff must not expire a continuously failing step.
func TestLoopStuckEpisodeSurvivesScheduledBackoff(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	start := r.a.now
	failures, filed := map[string]loop.LoopFailure{}, map[string]time.Time{}
	act := superloop.Action{Kind: superloop.KindReview, Key: "review o/r#1", Ref: refOne}
	for count := 1; count <= 7; count++ {
		if err := l.Failed(act, failures, errors.New("unavailable")); err != nil {
			t.Fatal(err)
		}
		f := failures[act.Key]
		if f.Count != count || !f.Since.Equal(start) {
			t.Fatalf("episode lost at retry %d: %+v", count, f)
		}
		if err := l.Stuck(failures, filed); err != nil {
			t.Fatal(err)
		}
		r.a.now = f.RetryAt
		if r.a.now.IsZero() {
			r.a.now = start.Add(time.Minute)
		}
	}
	if evs := r.logged(t, loop.LoopFileVerb); len(evs) != 1 {
		t.Fatalf("filed %d times", len(evs))
	}
	r.a.now = failures[act.Key].RetryAt.Add(loop.LoopStuckRecent)
	if err := l.Failed(act, failures, errors.New("unavailable")); err != nil {
		t.Fatal(err)
	}
	if f := failures[act.Key]; f.Count != 1 || !f.Since.Equal(r.a.now) {
		t.Fatalf("quiet episode continued: %+v", f)
	}
}

// The service leaves app.now at startup and advances only loopNow.
func TestLoopStuckEpisodeUsesRunningClock(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	start := r.a.now
	r.a.loopNow = &atomic.Pointer[time.Time]{}
	failures, filed := map[string]loop.LoopFailure{}, map[string]time.Time{}
	act := superloop.Action{Kind: superloop.KindReview, Key: "review o/r#1", Ref: refOne}
	for i, minute := range []int{0, 1, 6, 16, 36, 76} {
		now := start.Add(time.Duration(minute) * time.Minute)
		r.a.loopNow.Store(&now)
		if err := l.Failed(act, failures, errors.New("unavailable")); err != nil {
			t.Fatal(err)
		}
		f := failures[act.Key]
		if f.Count != i+1 || !f.Since.Equal(start) || !f.At.Equal(now) {
			t.Fatalf("episode at minute %d: %+v", minute, f)
		}
		if err := l.Stuck(failures, filed); err != nil {
			t.Fatal(err)
		}
		want := 0
		if minute >= 36 {
			want = 1
		}
		if got := len(r.logged(t, loop.LoopFileVerb)); got != want {
			t.Fatalf("running loop filed %d problems after %d minutes, want %d", got, minute, want)
		}
		for _, at := range filed {
			if !at.Equal(start.Add(36 * time.Minute)) {
				t.Fatalf("filed at %v", at)
			}
		}
	}
	// Missing the scheduled retry expires the group, even with startup fixed.
	expired := failures[act.Key].RetryAt.Add(loop.LoopStuckRecent)
	r.a.loopNow.Store(&expired)
	if group := l.StuckGroup(failures, act.Kind, "unavailable"); len(group) != 0 {
		t.Fatalf("quiet group did not expire: %+v", group)
	}
	if err := l.Failed(act, failures, errors.New("unavailable")); err != nil {
		t.Fatal(err)
	}
	if f := failures[act.Key]; f.Count != 1 || !f.Since.Equal(expired) || !f.At.Equal(expired) {
		t.Fatalf("quiet episode continued: %+v", f)
	}
	if err := l.Stuck(failures, filed); err != nil {
		t.Fatal(err)
	}
	if got := len(r.logged(t, loop.LoopFileVerb)); got != 1 {
		t.Fatalf("new episode filed immediately: %d", got)
	}
}

// Regression test for after-the-fact review of https://github.com/giantswarm/beekeeper/pull/2164:
// when ownOverlap records the board dependency and wakeOrStartFixer defers on
// capacity (loop.ErrLoopLater), the next pass must not repeat board unblock/block.
func TestLoopOverlapDeferredStartDoesNotRepeatBoardBlock(t *testing.T) {
	r := newLoopRig(t)
	r.pull.Closes = []string{loopClosingIssue}
	r.a.cfg.Board.Owner, r.a.cfg.Board.Project = "o", 1
	r.sshLoopHost(t, true)
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: state.Party{Name: loopWaitingIssueWorker, Session: loopOverlapSession}, Task: loopIssueTask, Park: &state.Park{On: loopPR, At: r.a.now, Waits: loopOverlapWait}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	capOut, capCode := "capacity full", 1
	boardCalls := 0
	r.answer = func(args []string) (string, int) {
		if len(args) > 0 && args[0] == loop.LoopBoard {
			boardCalls++
			return "", 0
		}
		if len(args) > 0 && args[0] == loop.LoopCapacity {
			return capOut, capCode
		}
		return "", 0
	}
	r.pass(t)
	if boardCalls != 2 {
		t.Fatalf("pass 1 board calls = %d, want 2 (unblock + block)", boardCalls)
	}
	if len(r.logged(t, "supervisor.owner")) != 0 {
		t.Fatal("owner recorded before fixer started")
	}
	capOut, capCode = `{"room":1}`, 0
	r.pass(t)
	if boardCalls != 2 {
		t.Fatalf("pass 2 repeated board calls: total = %d, want 2", boardCalls)
	}
	if len(r.logged(t, "supervisor.owner")) != 1 {
		t.Fatalf("pass 2 did not finish owner assignment: %+v", r.logged(t, "supervisor.owner"))
	}
}

// Reviewers without a worker GitHub App follow the placement preferences of workers,
// including Codex, and retain review and self-hosted capacity accounting.
func TestLoopPlacesRelayedReviewersOnExecutionHosts(t *testing.T) {
	branch := loop.GhDefaultBranch
	t.Cleanup(func() { loop.GhDefaultBranch = branch })
	loop.GhDefaultBranch = func(context.Context, string) (string, error) { return mainBranch, nil }
	for _, harness := range []string{claudeComm, execution.HarnessCodex} {
		for _, first := range []bool{false, true} {
			for _, localRoom := range []bool{false, true} {
				for _, selfHosted := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/first=%t/local=%t/selfHosted=%t", harness, first, localRoom, selfHosted), func(t *testing.T) {
						r := newLoopRig(t)
						r.sshLoopHost(t, true)
						r.a.cfg.GitHub.WorkerApp = nil
						r.a.cfg.Supervisor.HostsFirst = first
						r.a.cfg.Capacity.Choices[0].Harness = harness
						h := r.a.cfg.Agents.Hosts[sshTestSelector]
						h.Harnesses = []string{harness}
						r.a.cfg.Agents.Hosts[sshTestSelector] = h
						r.answer = func(args []string) (string, int) {
							if args[0] == loop.LoopCapacity {
								if !slices.Contains(args, "--review") || slices.Contains(args, "--"+reviewOfSelfHostedFlag) != selfHosted {
									t.Fatalf("lost review capacity flags: %v", args)
								}
								if localRoom || slices.Contains(args, loop.LoopHostFlag) {
									return loopRoomJSON, 0
								}
								return loopNoRoomJSON, 0
							}
							return "", 0
						}
						act := superloop.Action{Ref: sshTestRepository + "#7", URL: "https://github.com/" + sshTestRepository + "/pull/7", Head: loopHead, Round: 1, Key: "remote-review", SelfHosted: selfHosted}
						p := r.pull
						p.Ref, p.URL, p.Branch = act.Ref, act.URL, "feat/pr-7"
						r.pulls[act.Ref] = p
						if _, err := (&loop.Loop{Host: r.a, Said: map[string]bool{}}).StartReviewer(context.Background(), act); err != nil {
							t.Fatal(err)
						}
						start := r.called(agentsName, agentStartName)
						if start == nil {
							t.Fatal("no reviewer started")
						}
						if slices.Contains(start, loop.LoopHostFlag) != (first || !localRoom) {
							t.Fatalf("wrong placement: %v", start)
						}
						if first || !localRoom {
							for _, want := range []string{sshTestSelector, loop.LoopRepoFlag, sshTestRepository, loop.LoopRefFlag, "feat/pr-7"} {
								if !slices.Contains(start, want) {
									t.Fatalf("missing %s: %v", want, start)
								}
							}
						}
					})
				}
			}
		}
	}
}

func TestCombinedPullInheritsClosingKeywordsAndMergeClosesNamedIssues(t *testing.T) {
	r := newLoopRig(t)
	const (
		origA    = "o/r#401"
		origB    = "o/r#402"
		issueA   = "o/r#50"
		issueB   = "o/r#51"
		issueC   = "o/r#52"
		issueURL = "https://github.com/o/r/issues/"
	)
	r.pulls[origA] = superloop.Pull{
		Ref:    origA,
		URL:    "https://github.com/o/r/pull/401",
		State:  superloop.Open,
		Head:   loopHead,
		Author: loopDesk,
		Closes: []string{issueA},
	}
	r.pulls[origB] = superloop.Pull{
		Ref:    origB,
		URL:    "https://github.com/o/r/pull/402",
		State:  superloop.Open,
		Head:   loopHead,
		Author: loopDesk,
	}
	noPost := ""
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.UpsertReviewPark(state.ReviewPark{
			On:         origB,
			At:         r.a.timeNow(),
			ReviewHead: loopHead,
			Agent:      loopSecondWorker,
			Party:      state.Party{Name: loopSecondWorker, Session: "session-b"},
			Serves:     issueB,
			PostMerge:  &noPost,
		})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pull.Body = "Combines https://github.com/o/r/pull/401 and https://github.com/o/r/pull/402.\n\nCloses " + issueURL + "52"
	r.pull.Absorbs = []string{origA, origB}
	r.pull.Closes = []string{issueC}

	var editedBody string
	loop.EditPullBody = func(_ context.Context, pr github.PR, body string) error {
		if pr.Repo != repo || pr.N != 7 {
			t.Fatalf("unexpected editPullBody target: %+v", pr)
		}
		editedBody = body
		return nil
	}
	closedIssues := map[string]string{}
	loop.ClosePullIssue = func(_ context.Context, iss github.PR, pullURL string) error {
		closedIssues[refName(iss)] = pullURL
		return nil
	}
	refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
		out := map[github.PR]github.RefState{}
		for _, ref := range refs {
			if ref.N == 7 || ref.N == 401 || ref.N == 402 {
				out[ref] = github.RefState{Kind: github.RefPullRequest, State: github.Open}
				continue
			}
			state := github.Open
			if closedIssues[refName(ref)] != "" {
				state = github.Closed
			}
			out[ref] = github.RefState{Kind: github.RefIssue, State: state}
		}
		return out, nil
	}

	r.park(t, loopPR, time.Now())
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		for i := range st.Agents {
			if st.Agents[i].Name == loopWorker && st.Agents[i].Park != nil {
				st.Agents[i].Park.PostMerge = &noPost
			}
		}
		for i := range st.ReviewParks {
			if st.ReviewParks[i].On == loopPR {
				st.ReviewParks[i].PostMerge = &noPost
				st.ReviewParks[i].Serves = issueC
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.pass(t)

	for _, want := range []string{
		"Closes " + issueURL + "50",
		"Closes " + issueURL + "51",
		"Closes " + issueURL + "52",
	} {
		if !strings.Contains(editedBody, want) {
			t.Fatalf("combining PR body missing %q:\n%s", want, editedBody)
		}
	}
	for _, iss := range []string{issueA, issueB, issueC} {
		if closedIssues[iss] != loopURL {
			t.Fatalf("issue %s was not closed by %s (closedIssues=%v)", iss, loopURL, closedIssues)
		}
	}

	parsed, err := loop.ParseLoopPull(loopPR, []byte(`{"url":"`+loopURL+`","state":"OPEN","headRefOid":"`+loopHead+`","baseRefName":"release-1.0","title":"fix: closes #53","body":"This does not fix https://github.com/o/r/issues/99\nCloses https://github.com/o/r/issues/54","closingIssuesReferences":[]}`))
	if err != nil {
		t.Fatalf("parseLoopPull: %v", err)
	}
	if want := []string{"o/r#53", "o/r#54"}; !slices.Equal(parsed.Closes, want) {
		t.Fatalf("parseLoopPull Closes = %v, want %v", parsed.Closes, want)
	}
}

func TestReview3325ClosureFailureRetriesDurably(t *testing.T) {
	r := newLoopRig(t)
	const (
		origA    = "o/r#401"
		issueA   = "o/r#50"
		issueURL = "https://github.com/o/r/issues/50"
	)
	r.pulls[origA] = superloop.Pull{
		Ref:    origA,
		URL:    "https://github.com/o/r/pull/401",
		State:  superloop.Open,
		Head:   loopHead,
		Author: loopDesk,
		Closes: []string{issueA},
	}
	noPost := ""
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.UpsertReviewPark(state.ReviewPark{
			On:         origA,
			At:         r.a.timeNow(),
			ReviewHead: loopHead,
			Agent:      loopSecondWorker,
			Party:      state.Party{Name: loopSecondWorker, Session: "session-a"},
			Serves:     issueA,
			PostMerge:  &noPost,
		})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pull.Body = "Combines https://github.com/o/r/pull/401"
	r.pull.Absorbs = []string{origA}

	failEditOnce := true
	loop.EditPullBody = func(_ context.Context, _ github.PR, _ string) error {
		if failEditOnce {
			failEditOnce = false
			return errors.New("transient editPullBody failure")
		}
		return nil
	}
	failClose := true
	closedIssues := map[string]string{}
	loop.ClosePullIssue = func(_ context.Context, iss github.PR, pullURL string) error {
		if failClose {
			return errors.New("transient closePullIssue failure")
		}
		closedIssues[refName(iss)] = pullURL
		return nil
	}
	refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
		out := map[github.PR]github.RefState{}
		for _, ref := range refs {
			if ref.N == 7 || ref.N == 401 {
				out[ref] = github.RefState{Kind: github.RefPullRequest, State: github.Open}
				continue
			}
			st := github.Open
			if closedIssues[refName(ref)] != "" {
				st = github.Closed
			}
			out[ref] = github.RefState{Kind: github.RefIssue, State: st}
		}
		return out, nil
	}

	r.park(t, loopPR, time.Now())
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		for i := range st.Agents {
			if st.Agents[i].Name == loopWorker && st.Agents[i].Park != nil {
				st.Agents[i].Park.PostMerge = &noPost
			}
		}
		for i := range st.ReviewParks {
			if st.ReviewParks[i].On == loopPR {
				st.ReviewParks[i].PostMerge = &noPost
				st.ReviewParks[i].Serves = issueA
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")

	// Pass 1: loop.EditPullBody fails transiently, so the combining PR is not merged yet.
	r.pass(t)
	if r.pull.State == superloop.Merged {
		t.Fatal("combining PR should not merge when ensureCombinedPullCloses fails")
	}

	// Pass 2: loop.EditPullBody succeeds and PR merges, but loop.ClosePullIssue fails once,
	// so ReviewParks remain present for retry on the next pass.
	r.pass(t)
	if r.pull.State != superloop.Merged {
		t.Fatal("expected combining PR to merge once editPullBody succeeded")
	}
	if closedIssues[issueA] != "" {
		t.Fatalf("expected first closePullIssue attempt to fail, got %v", closedIssues)
	}
	stMid, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stMid.ReviewParkFor(origA); !ok {
		t.Fatal("absorbed PR ReviewPark was deleted before closeMergedPullIssues succeeded")
	}

	// Pass 3: endMerged / endAbsorbed retries loop.CloseMergedPullIssues and closes issueA.
	failClose = false
	r.pass(t)
	if closedIssues[issueA] != loopURL {
		t.Fatalf("issue %s was not closed on retry (closedIssues=%v, url=%s)", issueA, closedIssues, issueURL)
	}
}

func TestReview3325MissingClosureStatesSurviveReplay(t *testing.T) {
	for _, absorbed := range []bool{false, true} {
		for _, partial := range []bool{false, true} {
			t.Run(fmt.Sprintf("absorbed=%t/partial=%t", absorbed, partial), func(t *testing.T) {
				r := newLoopRig(t)
				const original = "o/r#401"
				const inherited = "o/r#51"
				noPost := ""
				target := loopPR
				if absorbed {
					target = original
				}
				if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
					st.UpsertReviewPark(state.ReviewPark{On: target, Agent: loopWorker, Party: state.Party{Session: "retained-session"}, At: r.a.timeNow(), ReviewHead: loopHead, Serves: "o/r#50", PostMerge: &noPost})
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
				r.pull.State = superloop.Merged
				r.pull.Closes = []string{"o/r#50", inherited}
				r.pulls[original] = superloop.Pull{Ref: original, State: superloop.Closed, Closes: []string{inherited}}
				confirmed := false
				refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
					states := map[github.PR]github.RefState{}
					for _, ref := range refs {
						if confirmed || (partial && ref.N == 50) {
							states[ref] = github.RefState{Kind: github.RefIssue, State: github.Closed}
						}
					}
					return states, nil
				}
				act := superloop.Action{Ref: target, URL: loop.PullRefURL(target), Message: strings.ToLower(superloop.Merged)}
				if absorbed {
					act.Message = "absorbed by " + loopPR
				}
				// Re-open the durable store and construct a new loop before each replay;
				// the merged-pull cache and the old roster cannot carry the obligation.
				for attempt := 0; attempt < 2; attempt++ {
					reopened, err := state.Open(r.a.store.Dir())
					if err != nil {
						t.Fatal(err)
					}
					r.a.store = reopened
					l, err := r.a.newSupervisorLoop(false)
					if err != nil {
						t.Fatal(err)
					}
					st, err := reopened.Read()
					if err != nil {
						t.Fatal(err)
					}
					if _, err := l.EndMerged(context.Background(), st, act); err == nil {
						t.Fatal("missing closure state completed reconciliation")
					}
					st, err = reopened.Read()
					if err != nil {
						t.Fatal(err)
					}
					if _, ok := st.ReviewParkFor(target); !ok {
						t.Fatal("lost durable retry ownership")
					}
				}
				confirmed = true
				l, err := r.a.newSupervisorLoop(false)
				if err != nil {
					t.Fatal(err)
				}
				st, err := r.a.store.Read()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := l.EndMerged(context.Background(), st, act); err != nil {
					t.Fatalf("confirmed closure failed: %v", err)
				}
				st, err = r.a.store.Read()
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := st.ReviewParkFor(target); ok {
					t.Fatal("confirmed closure still retained ownership")
				}
			})
		}
	}
}

// A step the stopping loop cancels is no failure of the step: recorded, it
// would back off for up to an hour, across the restart
// (https://github.com/giantswarm/beekeeper/issues/3390).
func TestLoopStepCanceledByTheLoopIsNoFailure(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	run, inTurn := loop.BeekeeperRun, loopInTurn
	t.Cleanup(func() { loop.BeekeeperRun, loopInTurn = run, inTurn })
	loop.BeekeeperRun = func(ctx context.Context, _ ...string) (string, int, error) {
		return "", 0, cmp.Or(ctx.Err(), errors.New("unavailable"))
	}
	loopInTurn = func(*app, state.Party) bool { return false }
	st := &state.State{Agents: []state.Agent{{Party: state.Party{Name: "w1"}}}}
	act := superloop.Action{Kind: superloop.KindDone, Key: "done w1", Agent: "w1"}
	failures := map[string]loop.LoopFailure{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.ExecAction(ctx, st, nil, failures, act); err != nil {
		t.Fatal(err)
	}
	if len(failures) != 0 {
		t.Fatalf("a canceled step was recorded as failed: %+v", failures)
	}
	if evs := r.logged(t, loop.LoopFailureVerb); len(evs) != 0 {
		t.Fatalf("a canceled step logged a failure: %+v", evs)
	}
	// The same error while the loop runs is the step's own failure.
	if err := l.ExecAction(context.Background(), st, nil, failures, act); err != nil {
		t.Fatal(err)
	}
	if failures[act.Key].Count != 1 {
		t.Fatalf("a failing step was not recorded: %+v", failures)
	}
}

// A busy state lock is told apart from other pass failures, wrapped or not:
// the loop skips the pass instead of exiting on it.
func TestLoopTellsABusyStateLock(t *testing.T) {
	busy := &state.LockBusyError{PID: 1, Command: "beekeeper guide watch", Since: time.Now(), Waited: 2 * time.Minute}
	if !loop.LockBusy(busy) || !loop.LockBusy(fmt.Errorf("reading the state: %w", busy)) {
		t.Fatal("a busy state lock was not told")
	}
	if loop.LockBusy(errors.New("disk full")) || loop.LockBusy(nil) {
		t.Fatal("another failure was told a busy lock")
	}
}

func TestLoopDoesNotReexecDuringShutdown(t *testing.T) {
	for _, duringGate := range []bool{false, true} {
		t.Run(fmt.Sprintf("duringGate=%v", duringGate), func(t *testing.T) {
			r := newLoopRig(t)
			r.replaced = true
			l, err := r.a.newSupervisorLoop(false)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if duringGate {
				r.answer = func(args []string) (string, int) {
					if slices.Equal(args, budgetGateArgs) {
						cancel()
					}
					return "", 0
				}
			} else {
				cancel()
			}
			if reexec, err := l.Reexec(ctx); err != nil || reexec {
				t.Fatalf("shutdown reexec = %v, %v", reexec, err)
			}
			if r.execs != 0 {
				t.Fatal("re-executed during shutdown")
			}
			if ctx.Err() == nil {
				t.Fatal("shutdown was not exercised")
			}
		})
	}
}

func TestLoopDoesNotReexecWhenShutdownBeginsWhileWatchDrains(t *testing.T) {
	r := newLoopRig(t)
	r.replaced = true
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := false
	l.StopWatch = func() {
		stopped = true
		cancel()
	}
	if reexec, err := l.Reexec(ctx); err != nil || reexec {
		t.Fatalf("shutdown reexec = %v, %v", reexec, err)
	}
	if !stopped || ctx.Err() == nil {
		t.Fatal("shutdown while stopping watch was not exercised")
	}
	if r.execs != 0 {
		t.Fatal("re-executed during shutdown")
	}
}

func TestLoopSkipsPassObservationsWhileDrainingForReexec(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.replaced = true
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	if !l.BeginInFlight("stream:workers") {
		t.Fatal("failed to mark worker stream in flight")
	}
	alertReads := 0
	oldAlerts := readLoopAlerts
	t.Cleanup(func() { readLoopAlerts = oldAlerts })
	readLoopAlerts = func(context.Context, *app) ([]alerts.Firing, error) {
		alertReads++
		return nil, nil
	}
	pullReads := 0
	oldReadPull := loop.ReadLoopPull
	t.Cleanup(func() { loop.ReadLoopPull = oldReadPull })
	loop.ReadLoopPull = func(context.Context, string) (superloop.Pull, error) {
		pullReads++
		return r.pull, nil
	}
	if err := l.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.execs != 0 {
		t.Fatalf("re-executed while a claim was in flight: %d", r.execs)
	}
	if alertReads != 0 || pullReads != 0 {
		t.Fatalf("ran pass observations on a replaced binary (alerts=%d pulls=%d)", alertReads, pullReads)
	}
	if !strings.Contains(r.out.String(), "SUPERVISOR LOOP restart held: claims are in flight") {
		t.Fatalf("missing restart-held log: %s", r.out.String())
	}
	if err := l.SettleStaleNext([]superloop.Action{{Key: "next 1", Kind: superloop.KindNext}}); err != nil {
		t.Fatal(err)
	}
	if evs := r.logged(t, "supervisor."+superloop.KindNext); len(evs) != 0 {
		t.Fatalf("settled stale next while draining for re-exec: %+v", evs)
	}
	l.EndInFlight("stream:workers")
	if reexec, err := l.Reexec(context.Background()); err != nil || !reexec || r.execs != 1 {
		t.Fatalf("reexec after draining in-flight claim = %v, %v (execs=%d)", reexec, err, r.execs)
	}
}

func TestLoopReexecCancelsInFlightActionsWhenRecoveryWriterFenced(t *testing.T) {
	r := newLoopRig(t)
	r.replaced = true
	candidateStore, err := state.OpenVersion(r.a.store.Dir(), recoveryBadVersion)
	if err != nil {
		t.Fatal(err)
	}
	r.a.store = candidateStore
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := candidateStore.RecoverVersion(recoveryGoodVersion, recoveryBadVersion, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !state.WriterFenced(r.a.store) {
		t.Fatal("expected candidate store to be fenced after recovery")
	}
	bgCtx := l.BackgroundContext(context.Background())
	if !l.BeginInFlight("stream:workers") {
		t.Fatal("failed to mark worker stream in flight")
	}
	l.PassActions.Go(func() {
		defer l.EndInFlight("stream:workers")
		<-bgCtx.Done()
	})
	reexec, err := l.Reexec(context.Background())
	if err != nil || !reexec || r.execs != 1 {
		t.Fatalf("fenced reexec with in-flight action = %v, %v (execs=%d)", reexec, err, r.execs)
	}
	if strings.Contains(r.out.String(), "SUPERVISOR LOOP restart held: claims are in flight") {
		t.Fatalf("fenced candidate held re-exec on in-flight claims: %s", r.out.String())
	}
}

func TestLoopReviewWorkersStopOnRecoveryWriterFence(t *testing.T) {
	r := newLoopRig(t)
	candidateStore, err := state.OpenVersion(r.a.store.Dir(), recoveryBadVersion)
	if err != nil {
		t.Fatal(err)
	}
	r.a.store = candidateStore
	now := time.Now().UTC()
	if err := candidateStore.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{
			{
				Party: state.Party{Name: "w1", HostSession: "s1"},
				Task:  "Work https://github.com/org/repo/issues/1",
				Park:  &state.Park{At: now, On: loopPR, Waits: deskReviewWait, ReviewHead: loopHead, Worktree: "/tmp/wt1"},
			},
			{
				Party: state.Party{Name: "w2", HostSession: "s2"},
				Task:  "Work https://github.com/org/repo/issues/2",
				Park:  &state.Park{At: now, On: "org/repo#13", Waits: deskReviewWait, ReviewHead: loopHead, Worktree: "/tmp/wt2"},
			},
		}
		for _, ag := range st.Agents {
			st.Starts = append(st.Starts, state.Start{Party: ag.Party, Dir: ag.Park.Worktree})
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.Lifecycle = &loop.LoopLifecycle{Version: recoveryBadVersion, Started: now}
	prevInTurn := loopInTurn
	t.Cleanup(func() { loopInTurn = prevInTurn })
	loopInTurn = func(*app, state.Party) bool { return false }

	st, err := candidateStore.Read()
	if err != nil {
		t.Fatal(err)
	}
	runCalls := 0
	r.answer = func(args []string) (string, int) {
		runCalls++
		if err := candidateStore.RecoverVersion(recoveryGoodVersion, recoveryBadVersion, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		return "cannot write the state: " + state.ErrRecoveryWriterFenced.Error(), 1
	}
	err = l.EndReviewWorkers(context.Background(), st)
	if !errors.Is(err, state.ErrRecoveryWriterFenced) || runCalls != 1 {
		t.Fatalf("endReviewWorkers error = %v (runCalls=%d), want %v and 1 call", err, runCalls, state.ErrRecoveryWriterFenced)
	}
	// Once fenced, a subsequent pass stops before running any further worker commands.
	if err := l.EndReviewWorkers(context.Background(), st); err != nil || runCalls != 1 {
		t.Fatalf("fenced endReviewWorkers = %v (runCalls=%d), want nil and no extra calls", err, runCalls)
	}
}

func TestLoopReviewsReferencesToNeverRepositories(t *testing.T) {
	r := newLoopRig(t)
	r.ownedBy(loopTeamB)
	r.a.cfg.Supervisor.Ownership.Review = []string{"o/*"}
	r.pull.Title = "fix: update o/tool integration"
	r.pull.Body = "Context: https://github.com/o/tool/issues/1"
	r.pull.Branch = "update-tool"
	r.pull.Files = []string{"tool/config.yaml"}
	r.park(t, loopPR, time.Now())
	r.pass(t)
	if start := r.called(agentsName, agentStartName); start == nil || start[2] != loopReviewer {
		t.Fatalf("a reviewed repository naming a never repository got no reviewer: %v", r.calls)
	}
	if evs := r.logged(t, "supervisor.leave"); len(evs) != 0 {
		t.Errorf("references caused a leave: %+v", evs)
	}
}

// A planned classification must re-read the park before sending a wake or
// asking the classifier: a worker can change state during loop observations.
func TestLoopClassifyRechecksParkState(t *testing.T) {
	const (
		activeTimer    = "active timer"
		workerDone     = "worker done"
		workerRemoved  = "worker removed"
		parkReplaced   = "park replaced"
		parkResumable  = "park resumable"
		firedTimer     = "fired timer"
		oldParkTimer   = "old park timer"
		stillUnwatched = "still unwatched"
	)
	for _, change := range []string{activeTimer, workerDone, workerRemoved, parkReplaced, parkResumable, firedTimer, oldParkTimer, stillUnwatched} {
		t.Run(change, func(t *testing.T) {
			r := newLoopRig(t)
			at := time.Now().UTC().Add(-time.Minute)
			r.park(t, "", at)
			st, err := r.a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			seed(t, r.a, func(current *state.State) {
				i := slices.IndexFunc(current.Agents, func(ag state.Agent) bool { return ag.Name == loopWorker })
				ag := &current.Agents[i]
				switch change {
				case activeTimer, firedTimer, oldParkTimer:
					timer := state.Timer{Wake: loopWorker, At: at.Add(-time.Second), Due: at.Add(time.Hour)}
					if change == firedTimer {
						timer.Fired = at
					}
					if change == oldParkTimer {
						timer.WakeParkAt = at.Add(-time.Hour)
					}
					current.Timers = append(current.Timers, timer)
				case workerDone:
					ag.Done, ag.Task, ag.Park = true, "", nil
				case workerRemoved:
					current.Agents = slices.Delete(current.Agents, i, i+1)
				case parkReplaced:
					ag.Park.At = at.Add(time.Second)
				case parkResumable:
					ag.Park.Resumable = at.Add(time.Second)
				}
			})
			l := &loop.Loop{Host: r.a}
			_, err = l.Classify(t.Context(), st, superloop.Action{Agent: loopWorker, Event: &superloop.Event{At: at, Agent: loopWorker, Verb: parkEvent}})
			if err != nil {
				t.Fatal(err)
			}
			wantWake := change == firedTimer || change == oldParkTimer || change == stillUnwatched
			if wake := r.called(agentsName, agentWakeName); (wake != nil) != wantWake {
				t.Fatalf("wake %v, want wake %v", wake, wantWake)
			}
			if note := r.called(loop.LoopNote, loop.LoopAdd); note != nil {
				t.Fatalf("unexpected person note: %v", note)
			}
		})
	}
}
