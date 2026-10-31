package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// loopParty is the party the supervisor loop acts and logs as.
var loopParty = state.Party{Name: "supervisor loop"}

// loopStartVerb logs each start of the loop; the first one bounds the events
// it acts on.
const loopStartVerb = "supervisor.loop"

// loopClaimVerb logs an action the loop is about to take, before its side
// effect; loopStageVerb a finished stage of a release.
const (
	loopClaimVerb = "supervisor.claim"
	loopStageVerb = "supervisor.stage"
)

// loopEventVerbs are the events the loop's rules act on.
var loopEventVerbs = []string{superloop.VerbDone, superloop.VerbReport, superloop.VerbProblem, superloop.VerbPark}

// beekeeperRun runs this beekeeper binary with args for the loop: its
// output (stdout, then stderr) and exit code. An error is a run that did
// not exit.
var beekeeperRun = func(ctx context.Context, args ...string) (string, int, error) {
	bin, err := os.Executable()
	if err != nil {
		return "", 0, err
	}
	out, errOut, code, err := runArgv(ctx, "", append([]string{bin}, args...))
	return out + errOut, code, err
}

// loopCommand runs a configured command (the classifier, the post-install)
// with stdin: its stdout, its stderr and its exit code.
var loopCommand = runArgv

// runArgv runs argv with stdin: its stdout, its stderr and its exit code.
func runArgv(ctx context.Context, stdin string, argv []string) (string, string, int, error) {
	if len(argv) == 0 {
		return "", "", 0, errors.New("no command")
	}
	c := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // the loop's own binary or a configured command
	c.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	c.Stdout, c.Stderr = &out, &errOut
	err := c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.String(), errOut.String(), ee.ExitCode(), nil
	}
	return out.String(), errOut.String(), 0, err
}

// replaceable is the running binary as the loop sees it: whether an
// install renamed another file over its path, and its re-execution.
type replaceable interface {
	Replaced() bool
	Exec(env ...string) error
}

// loopBinary is the binary the loop runs; nil where it cannot tell.
var loopBinary = func() replaceable {
	if b := platform.RunningBinary(); b != nil {
		return b
	}
	return nil
}

// loopBroker puts the sandbox's broker onto the binary installed at the
// loop's path and says so: errLoopLater while it serves calls a restart
// would end (as self-update leaves it, its restart due).
var loopBroker = func(ctx context.Context) (string, error) {
	pid, bin, err := brokerProcess()
	if err != nil {
		return "no broker runs", nil //nolint:nilerr // no broker unit: nothing to put on the new binary
	}
	exe, err := os.Executable()
	if b := platform.RunningBinary(); b != nil {
		exe, err = b.Path, nil
	}
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return "", err
	}
	t, err := proc.Read()
	if err != nil {
		return "", err
	}
	switch step, calls := brokerDue(pid, bin, exe, fi.ModTime(), t); step {
	case brokerBusy:
		return fmt.Sprintf("the broker serves %d call(s)", calls), errLoopLater
	case brokerRestart:
		if err := restartBroker(ctx); err != nil {
			return "", fmt.Errorf("restarting the broker: %w", err)
		}
		return "the broker restarted on " + exe, nil
	}
	return "the broker runs the installed binary", nil
}

// readLoopPull reads a tracked pull request (owner/repo#n): its state, head,
// the checks on the head and its reviews.
var readLoopPull = func(ctx context.Context, ref string) (superloop.Pull, error) {
	r, ok := parseRef(ref)
	if !ok {
		return superloop.Pull{}, fmt.Errorf("%s names no pull request", ref)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := github.RunGH(ctx, "pr", "view", strconv.Itoa(r.N), "--repo", r.Repo, "--json", "state,url,headRefOid,statusCheckRollup,reviews")
	if err != nil {
		return superloop.Pull{}, fmt.Errorf("gh pr view %d --repo %s: %w", r.N, r.Repo, err)
	}
	return parseLoopPull(ref, out)
}

// loopPullJSON is gh pr view's answer.
type loopPullJSON struct {
	State  string `json:"state"`
	URL    string `json:"url"`
	Head   string `json:"headRefOid"`
	Rollup []struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		State      string `json:"state"`
	} `json:"statusCheckRollup"`
	Reviews []struct {
		ID     string    `json:"id"`
		Body   string    `json:"body"`
		State  string    `json:"state"`
		At     time.Time `json:"submittedAt"`
		Commit struct {
			OID string `json:"oid"`
		} `json:"commit"`
	} `json:"reviews"`
}

// parseLoopPull reads gh pr view's JSON of ref.
func parseLoopPull(ref string, out []byte) (superloop.Pull, error) {
	var j loopPullJSON
	if err := json.Unmarshal(out, &j); err != nil {
		return superloop.Pull{}, fmt.Errorf("gh pr view of %s: %w", ref, err)
	}
	p := superloop.Pull{Ref: ref, URL: j.URL, State: j.State, Head: j.Head, Green: len(j.Rollup) > 0}
	for _, c := range j.Rollup {
		// A check run reports a status and a conclusion, a commit status
		// a state.
		ok := c.State == "SUCCESS" || c.Status == "COMPLETED" && slices.Contains([]string{"SUCCESS", "NEUTRAL", "SKIPPED"}, c.Conclusion)
		if !ok {
			p.Green = false
		}
	}
	for _, r := range j.Reviews {
		p.Reviews = append(p.Reviews, superloop.Review{ID: r.ID, Head: superloop.ReviewedHead(r.Body, r.Commit.OID), Verdict: superloop.Verdict(r.State, r.Body), Body: r.Body, At: r.At})
	}
	slices.SortStableFunc(p.Reviews, func(x, y superloop.Review) int { return x.At.Compare(y.At) })
	return p, nil
}

func (a *app) supervisorLoopCmd() *cobra.Command {
	var once bool
	c := &cobra.Command{
		Use:   "loop [--once]",
		Short: "Run the supervisor's fixed procedure as a deterministic loop over beekeeper's events: no supervisor session needed",
		Long: `loop takes the supervisor's routine transitions without a supervisor session,
every supervisor.loopInterval (30s; --once: one pass), as the party
"supervisor loop". It is silent while nothing happens and prints and logs
one line per transition (supervisor.<kind>, the pull request its subject,
its key first in the detail):

  timers    due wall-clock and conditional timers send their wakes or run
            their commands, as in watch; line-only reminders stay with watch
  review    an agent parked on its pull request (agents park --on
            owner/repo#n) with every check on the head green, for the
            first round, or after the last round's findings were forwarded
            and the worker parked again on a new head while a round is
            left (supervisor.reviewRounds, default 1): a reviewer starts
            (agents start --review) with supervisor.reviewBrief or the
            built-in brief, on capacity's pick (--harness auto with
            capacity.choices), decided like any start; a worker on a
            self-hosted provider's model gets one on no self-hosted provider
            (--review-of-self-hosted)
  go        every check on the head green, and either the last round's
            verdict approves that head, or no round is left, its verdict
            asked for changes, they were forwarded and the worker parked
            again on a new head (its fixes): the merge go for that head,
            told to the worker (agents wake) with --match-head-commit
            <head>. Only the verdict of the loop's own last round counts,
            on the head it reviewed and posted after it started: an older
            verdict or a round still running authorizes nothing
  forward   the last round's verdict asks for changes: the review, word
            for word, to the worker, whatever its head is now
  escalate  no round left and the worker parked again after the forward
            on the head the findings were about, or on a new head after
            the last approval: a decision for guide.person (note add); the
            loop leaves that pull request alone from then
  end       a reviewed pull request merged or closed: its tracking ends
  done      agents.done: the agent leaves the roster (agents remove);
            the loop retires a session beekeeper started the same way,
            once its turn ends: a reviewer once the loop acted on its
            round's verdict (a go, forward, escalation, next round or
            end), and the worker of a pull request the loop gave the go
            for once it merged, reported or not. A session its person
            started is never retired
  next      after a done, while capacity has room: the next item of board
            next starts (agents start, --harness auto with
            capacity.choices) with supervisor.workerBrief; none
            configured, none starts
  release   a newer release, in stages a later pass resumes until the
            last holds: self-update (verified, installed) while --check
            finds it newer, the sandbox's broker on the installed binary
            (restarted, or waited for while it serves calls),
            supervisor.postInstall once, then self-update --check must
            find it the latest. A loop whose binary was replaced
            re-executes the installed one at its next pass
  classify  a report or park the rules cannot place: agents.report without
            agents.done; agents.problem neither logged with an agents.done
            (the done's log line names it, the worker files it in the
            owning repository) nor of a reviewer whose changes request
            the loop forwarded; a park on nothing of an agent still parked
            on nothing, with no timer that wakes it (timer add --wake), no
            note its wait names (#12) and no open note of its own. The
            supervisor.classifier command, given the event as JSON on
            stdin, answers {"class":"needs-person|blocked|failed|question",
            "confidence":0.9}; the event goes to the guide (note add for
            guide.person) with its class, or unclassified below
            supervisor.classifierConfidence (0.8) or without a classifier
  question  a worker's open note for someone other than guide.person,
            asked before the loop started or after: the note is
            re-addressed to guide.person, into the guide's queue

The briefs are Go templates over .URL, .Repo, .Number, .Head, .Round and
.Worker. A pull request is read (one gh pr view) only while an agent is
parked on it or the loop reviews it. Every transition happens once: a
restarted loop reads its own log lines back and takes no key twice; a
failed one is tried again next pass, its failure printed once. An action
with a side effect is claimed in the log first (supervisor.claim): one
claimed and not logged taken, after an interruption, is taken only when
the log shows no line of its command (agents start, agents wake, agents
remove, note add) since the claim. One loop runs per state: a second is
refused. The loop acts on the events logged since its first start.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.runSupervisorLoop(cmd.Context(), once) },
	}
	c.Flags().BoolVar(&once, "once", false, "one pass, then exit")
	return c
}

// supervisorLoop is one run of the loop: what it said once.
type supervisorLoop struct {
	a     *app
	since time.Time
	// said are the failures already printed, by key.
	said map[string]bool
	// releaseAt is when the loop last looked for a release.
	releaseAt time.Time
	// stages are the release stages finished, by key.
	stages map[string]bool
	// timerActs are timer side effects; shutdown and re-exec await them.
	timerActs sync.WaitGroup
}

// loopOutput serializes loop transitions with asynchronous timer output.
type loopOutput struct {
	mu  sync.Mutex
	out io.Writer
}

func (w *loopOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(p)
}

func (a *app) runSupervisorLoop(ctx context.Context, once bool) error {
	// One loop per state: a second would take each action again.
	lock := flock.New(filepath.Join(a.store.Dir(), loopLockFile))
	held, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !held {
		return refused("another supervisor loop runs on this state (%s is locked)", lock.Path())
	}
	defer func() { _ = lock.Unlock() }()
	out := a.out
	a.out = &loopOutput{out: out}
	defer func() { a.out = out }()
	l, err := a.newSupervisorLoop()
	if err != nil {
		return err
	}
	defer l.timerActs.Wait()
	for {
		a.now = time.Now()
		if err := l.pass(ctx); err != nil {
			return err
		}
		if once {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(a.cfg.Supervisor.LoopInterval.Duration):
		}
	}
}

// loopLockFile is the lock one running loop holds, beside the state.
const loopLockFile = "supervisor-loop.lock"

// newSupervisorLoop logs the loop's start and bounds its events by its
// first.
func (a *app) newSupervisorLoop() (*supervisorLoop, error) {
	starts, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == loopStartVerb })
	if err != nil {
		return nil, err
	}
	l := &supervisorLoop{a: a, since: a.now.UTC(), said: map[string]bool{}}
	if len(starts) > 0 {
		l.since = starts[0].At
	}
	if err := a.store.Record(event(loopParty, loopStartVerb, "started; acting on the events since %s", l.since.Format(time.RFC3339))); err != nil {
		return nil, err
	}
	return l, nil
}

// pass is one look: gather, plan, act.
func (l *supervisorLoop) pass(ctx context.Context) error {
	a := l.a
	// A release installed over the loop's path runs from here on: an old
	// binary's saves are refused once the new one wrote the state.
	if b := loopBinary(); b != nil && b.Replaced() {
		if _, err := fmt.Fprintln(a.out, "SUPERVISOR LOOP its binary was replaced: re-executing it"); err != nil {
			return err
		}
		l.timerActs.Wait()
		if err := b.Exec(); err != nil {
			l.sayOnce("reexec", "SUPERVISOR LOOP the new binary does not start (%v): carrying on", err)
		}
	}
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	if err := l.fireTimers(ctx, st); err != nil {
		return err
	}
	in := superloop.Input{Rounds: a.cfg.Supervisor.ReviewRounds, Person: a.cfg.Guide.Person}
	for _, ag := range st.Agents {
		w := superloop.Worker{Name: ag.Name, SelfHosted: selfHostedModel(a.cfg.Capacity, ag.Model), Started: st.StartIndex(ag.Party) >= 0}
		if ag.Park != nil {
			w.ParkedOn, w.ParkedAt = ag.Park.On, ag.Park.At
		}
		w.Waking = slices.ContainsFunc(st.Timers, func(t state.Timer) bool { return t.Wake != "" && strings.EqualFold(t.Wake, ag.Name) })
		in.Workers = append(in.Workers, w)
	}
	for _, n := range st.Notes {
		if n.Kind != noteLogin {
			by := n.By.Name
			if i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(n.By) }); i >= 0 {
				by = st.Agents[i].Name
			}
			in.Notes = append(in.Notes, superloop.Note{ID: n.ID, For: n.For, By: by, Text: n.Text})
		}
	}
	taken, err := a.store.Follow("supervisor-loop-taken", func(e state.Event) bool { return strings.HasPrefix(e.Verb, "supervisor.") })
	if err != nil {
		return err
	}
	claims, stages := map[string]loopClaim{}, map[string]bool{}
	for _, e := range taken {
		if t, ok := superloop.ParseTaken(e.At, e.Verb, e.Subject, e.Detail); ok {
			in.Taken = append(in.Taken, t)
			continue
		}
		key, rest, ok := keyed(e.Detail)
		switch {
		case !ok:
		case e.Verb == loopClaimVerb:
			claims[key] = loopClaim{at: e.At, target: rest}
		case e.Verb == loopStageVerb:
			stages[key] = true
		}
	}
	for _, t := range in.Taken {
		delete(claims, t.Key)
	}
	l.stages = stages
	since := l.since
	evs, err := a.store.Follow("supervisor-loop", func(e state.Event) bool {
		return slices.Contains(loopEventVerbs, e.Verb) && !e.At.Before(since)
	})
	if err != nil {
		return err
	}
	for _, e := range evs {
		in.Events = append(in.Events, superloop.Event{At: e.At, Verb: e.Verb, Agent: loopEventAgent(st, e), Detail: e.Detail})
	}
	in.Pulls = map[string]superloop.Pull{}
	for _, ref := range superloop.Tracked(in.Workers, in.Taken) {
		p, err := readLoopPull(ctx, ref)
		if err != nil {
			l.sayOnce("read "+ref, "SUPERVISOR LOOP cannot read %s: %v", ref, err)
			continue
		}
		in.Pulls[ref] = p
	}
	// A release claimed and not finished goes on, whatever --check says
	// now that it is installed.
	for key := range claims {
		if v, ok := strings.CutPrefix(key, superloop.KindRelease+" "); ok {
			in.Release = v
		}
	}
	if in.Release == "" && a.now.Sub(l.releaseAt) >= a.cfg.Supervisor.ReleaseEvery.Duration {
		l.releaseAt = a.now
		in.Release = l.newerRelease(ctx)
	}
	for _, act := range superloop.Plan(in) {
		if c, ok := claims[act.Key]; ok && act.Kind != superloop.KindRelease {
			// Claimed before an interruption: what the log shows of its
			// side effect decides, never a second one.
			what, done, err := l.reconcile(st, act, c)
			if err != nil {
				return err
			}
			if done {
				if err := l.record(act, what); err != nil {
					return err
				}
				continue
			}
		}
		if act.Kind == superloop.KindQuestion {
			if err := l.readdress(act); err != nil {
				l.sayOnce(act.Key, "SUPERVISOR QUESTION FAILED %s: %v", act.Key, err)
			}
			continue
		}
		what, err := l.act(ctx, st, act)
		switch {
		case errors.Is(err, errLoopLater):
			continue
		case err != nil:
			l.sayOnce(act.Key, "SUPERVISOR %s FAILED %s: %v", strings.ToUpper(act.Kind), act.Key, err)
			continue
		}
		if err := l.record(act, what); err != nil {
			return err
		}
	}
	return nil
}

// record logs act taken, which no pass takes again, and says it.
func (l *supervisorLoop) record(act superloop.Action, what string) error {
	detail := superloop.Detail(act, what)
	if err := l.a.store.Record(about(event(loopParty, "supervisor."+act.Kind, "%s", detail), act.Ref)); err != nil {
		return err
	}
	_, err := fmt.Fprintf(l.a.out, "SUPERVISOR %s %s\n", strings.ToUpper(act.Kind), detail)
	return err
}

// loopClaim is a claim read back: when, and what its side effect names
// (the session started, the agent woken or removed, the note's question).
type loopClaim struct {
	at     time.Time
	target string
}

// keyed reads a loop line's detail: "[<key>] <rest>".
func keyed(detail string) (key, rest string, ok bool) {
	m := keyedLine.FindStringSubmatch(detail)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// keyedLine is a loop line's detail.
var keyedLine = regexp.MustCompile(`^\[([^\]]+)\] (.*)$`)

// claim logs act's side effect about to happen on target, before it does:
// a restart finds it and reconciles instead of acting twice.
func (l *supervisorLoop) claim(act superloop.Action, target string) error {
	return l.a.store.Record(about(event(loopParty, loopClaimVerb, "[%s] %s", act.Key, target), act.Ref))
}

// reconcile decides a claimed action from the log since its claim: done
// when the command it ran left its line (agents start, agents wake, agents
// remove, note add, all logged as the loop), else not, and it runs.
func (l *supervisorLoop) reconcile(st *state.State, act superloop.Action, c loopClaim) (string, bool, error) {
	evs, err := l.a.store.Events(0, func(e state.Event) bool { return e.By.Name == loopParty.Name && !e.At.Before(c.at) })
	if err != nil {
		return "", false, err
	}
	logged := func(verb string, match func(string) bool) bool {
		return slices.ContainsFunc(evs, func(e state.Event) bool { return e.Verb == verb && match(e.Detail) })
	}
	opens := func(d string) bool { return strings.HasPrefix(d, c.target+":") }
	var done bool
	switch act.Kind {
	case superloop.KindReview, superloop.KindNext:
		done = logged("agents.start", opens)
	case superloop.KindGo, superloop.KindForward:
		done = logged("agents.wake", opens)
	case superloop.KindDone:
		done = !slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == c.target }) ||
			logged("agents.remove", func(d string) bool { return d == c.target })
	case superloop.KindEscalate, superloop.KindClassify:
		done = logged("note.add", func(d string) bool { return strings.Contains(d, c.target) })
	}
	if !done {
		return "", false, nil
	}
	return fmt.Sprintf("taken before an interruption: %s, as the log shows", c.target), true, nil
}

// readdress puts a worker's question into the guide's queue and logs it in
// one write.
func (l *supervisorLoop) readdress(act superloop.Action) error {
	person := l.a.cfg.Guide.Person
	detail := superloop.Detail(act, "re-addressed to the guide's person: "+act.Message)
	err := l.a.store.Update(func(st *state.State) ([]state.Event, error) {
		i := slices.IndexFunc(st.Notes, func(n state.Note) bool { return n.ID == act.Note })
		if i < 0 {
			return nil, fmt.Errorf("note #%d is closed", act.Note)
		}
		st.Notes[i].For = person
		return []state.Event{event(loopParty, "supervisor."+act.Kind, "%s", detail)}, nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(l.a.out, "SUPERVISOR %s %s\n", strings.ToUpper(act.Kind), detail)
	return err
}

// errLoopLater is an action that waits for a later pass, quietly: no room
// for a start yet.
var errLoopLater = errors.New("later")

// sayOnce prints a failure once per key and run.
func (l *supervisorLoop) sayOnce(key, format string, args ...any) {
	if l.said[key] {
		return
	}
	l.said[key] = true
	_, _ = fmt.Fprintf(l.a.out, format+"\n", args...)
}

// newerRelease is a newer release's version, "" when this is the latest or
// the look failed.
func (l *supervisorLoop) newerRelease(ctx context.Context) string {
	out, code, err := beekeeperRun(ctx, selfUpdateCheck...)
	if err != nil || code != ExitOutdated {
		return ""
	}
	if m := newerLine.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

// selfUpdateCheck is the look for a newer release.
var selfUpdateCheck = []string{selfUpdateCmdName, "--check"}

// newerLine is self-update --check's line for a newer release.
var newerLine = regexp.MustCompile(`(\S+) is newer than`)

// The subcommands the loop runs that no other constant names.
const (
	loopRemove = "remove"
	loopBoard  = "board"
	loopAdd    = "add"
)

// asFlag is the global flag that names the party a command acts as.
const asFlag = "as"

// as is a beekeeper command line the loop runs as itself.
func (l *supervisorLoop) as(args ...string) []string {
	out := []string{"--" + asFlag, loopParty.Name}
	if l.a.cfgPath != "" {
		out = append([]string{"--" + configFlag, l.a.cfgPath}, out...)
	}
	return append(out, args...)
}

// run runs a beekeeper command as the loop; a non-zero exit is an error
// with its output.
func (l *supervisorLoop) run(ctx context.Context, args ...string) (string, error) {
	out, code, err := beekeeperRun(ctx, l.as(args...)...)
	if err != nil {
		return out, err
	}
	if code != 0 {
		return out, &exitError{code: code, msg: fmt.Sprintf("beekeeper %s: exit %d: %s", strings.Join(args[:min(2, len(args))], " "), code, oneLine(out))}
	}
	return out, nil
}

// act takes one transition and says what it did.
func (l *supervisorLoop) act(ctx context.Context, st *state.State, act superloop.Action) (string, error) {
	a := l.a
	k := a.cfg.Supervisor
	switch act.Kind {
	case superloop.KindReview:
		return l.startReviewer(ctx, act)
	case superloop.KindGo:
		if err := l.wake(ctx, act); err != nil {
			return "", err
		}
		return fmt.Sprintf("merge go for %s at %s, told", act.URL, act.Head), nil
	case superloop.KindForward:
		if err := l.wake(ctx, act); err != nil {
			return "", err
		}
		return fmt.Sprintf("the review of %s asking for changes on %s forwarded", act.URL, act.Head), nil
	case superloop.KindEscalate:
		if err := l.toGuide(ctx, act, act.URL,
			fmt.Sprintf("Merge %s with its review's blocking findings open, or have it reworked?", act.URL),
			act.Message,
			fmt.Sprintf("supervisor.reviewRounds (%d) is used up and the findings stand: only a person can let it merge", k.ReviewRounds),
			fmt.Sprintf("leave %s unmerged and its worker parked", act.URL)); err != nil {
			return "", err
		}
		return "to the guide: " + act.Message, nil
	case superloop.KindEnd:
		return fmt.Sprintf("%s %s: tracking ends", act.URL, act.Message), nil
	case superloop.KindDone:
		i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == act.Agent })
		if i < 0 {
			return "already off the roster", nil
		}
		// A retirement waits for the session's turn to end: its last
		// report is not cut off.
		if act.Event == nil && loopInTurn(a, st.Agents[i].Party) {
			l.sayOnce("turn "+act.Key, "SUPERVISOR DONE WAITS %s: its session runs a turn", act.Agent)
			return "", errLoopLater
		}
		if err := l.claim(act, act.Agent); err != nil {
			return "", err
		}
		if _, err := l.run(ctx, agentsName, loopRemove, act.Agent); err != nil {
			return "", err
		}
		if act.Message != "" {
			return "off the roster: " + act.Message, nil
		}
		return "off the roster", nil
	case superloop.KindNext:
		return l.startNext(ctx, act)
	case superloop.KindRelease:
		return l.install(ctx, act)
	case superloop.KindClassify:
		return l.classify(ctx, st, act)
	}
	return "", fmt.Errorf("no transition for %s", act.Kind)
}

// loopInTurn reports whether p's session runs a turn.
var loopInTurn = func(a *app, p state.Party) bool {
	_, ok := a.runningTurn(p)
	return ok
}

// wake tells act's worker its message (agents wake), claimed first.
func (l *supervisorLoop) wake(ctx context.Context, act superloop.Action) error {
	if err := l.claim(act, act.Agent); err != nil {
		return err
	}
	_, err := l.run(ctx, agentsName, agentWakeName, act.Agent, act.Message)
	return err
}

// briefData is what a brief template renders.
type briefData struct {
	URL, Repo, Head, Worker string
	Number, Round           int
}

// defaultReviewBrief is the review brief without supervisor.reviewBrief.
const defaultReviewBrief = `Review {{.URL}} at head {{.Head}}, review round {{.Round}}, the work of {{.Worker}}.

Read the pull request, the issue it closes and the diff. Judge whether it does what the issue asks, and find its bugs. Post one review on the pull request (gh pr review {{.Number}} --repo {{.Repo}} --comment --body-file <file>): its first line "Review: approved" or "Review: changes requested", its second "Head reviewed: ` + "`{{.Head}}`" + `", then every blocking finding with its file and line. Then report: beekeeper agents idle --done --report "<the verdict and the review's URL>" --problem none (or --problem "<finding, evidence, owning repo>" for each problem the review met).
`

// brief renders the template in file (def when file is empty) with d into
// a file under the state's directory, named after key.
func (l *supervisorLoop) brief(file, def, key string, d briefData) (string, error) {
	text := def
	if file != "" {
		b, err := os.ReadFile(file) //nolint:gosec // the configured brief template
		if err != nil {
			return "", err
		}
		text = string(b)
	}
	t, err := template.New(filepath.Base(file)).Option("missingkey=error").Parse(text)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, d); err != nil {
		return "", err
	}
	dir := filepath.Join(l.a.store.Dir(), "supervisor-loop")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, briefName.ReplaceAllString(key, "-")+".md")
	return path, os.WriteFile(path, b.Bytes(), 0o600)
}

// briefName is what a key may not keep in a file name.
var briefName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// startReviewer starts the reviewer of act's head.
func (l *supervisorLoop) startReviewer(ctx context.Context, act superloop.Action) (string, error) {
	r, _ := parseRef(act.Ref)
	brief, err := l.brief(l.a.cfg.Supervisor.ReviewBrief, defaultReviewBrief, act.Key, briefData{URL: act.URL, Repo: r.Repo, Number: r.N, Head: act.Head, Round: act.Round, Worker: act.Agent})
	if err != nil {
		return "", err
	}
	name := superloop.ReviewerName(act.Ref, act.Round)
	args := []string{agentsName, agentStartName, name, brief, "--review"}
	if len(l.a.cfg.Capacity.Choices) > 0 {
		// The reviewer takes capacity's pick, decided as a worker's start
		// is; a self-hosted model's work gets one on no self-hosted provider.
		args = append(args, "--harness", harnessAuto)
		if act.SelfHosted {
			args = append(args, "--"+reviewOfSelfHostedFlag)
		}
	}
	if err := l.claim(act, name); err != nil {
		return "", err
	}
	if _, err := l.run(ctx, args...); err != nil {
		if Code(err) == ExitRefused {
			l.sayOnce("room "+act.Key, "SUPERVISOR REVIEW WAITS %s: no room for a start yet (%v)", act.URL, err)
			return "", errLoopLater
		}
		return "", err
	}
	return fmt.Sprintf("reviewer %q started for %s at %s, round %d", name, act.URL, act.Head, act.Round), nil
}

// repoName is owner/repo's repo.
func repoName(repo string) string {
	_, name, _ := strings.Cut(repo, "/")
	return name
}

// selfHostedModel reports whether model is a choice of a self-hosted
// provider.
func selfHostedModel(k config.Capacity, model string) bool {
	if model == "" {
		return false
	}
	for _, c := range k.Choices {
		if c.Model == model || c.Harness+"/"+c.Model == model {
			if p, ok := k.Providers[c.EffectiveProvider()]; ok && p.SelfHosted() {
				return true
			}
		}
	}
	return false
}

// startNext starts the next ranked item with the worker brief, when
// capacity has room.
func (l *supervisorLoop) startNext(ctx context.Context, act superloop.Action) (string, error) {
	k := l.a.cfg.Supervisor
	if k.WorkerBrief == "" {
		return "no supervisor.workerBrief: no worker starts", nil
	}
	if _, code, err := beekeeperRun(ctx, l.as("capacity")...); err != nil || code != 0 {
		return "", errLoopLater
	}
	out, err := l.run(ctx, loopBoard, "next", "--json")
	if err != nil {
		return "", err
	}
	var next struct {
		Pick *struct {
			Ref   string `json:"ref"`
			URL   string `json:"url"`
			Title string `json:"title"`
		} `json:"pick"`
	}
	if err := json.Unmarshal([]byte(out), &next); err != nil {
		return "", fmt.Errorf("board next --json: %w", err)
	}
	if next.Pick == nil {
		return "the board offers nothing: no worker starts", nil
	}
	r, _ := parseRef(next.Pick.Ref)
	brief, err := l.brief(k.WorkerBrief, "", "next "+next.Pick.Ref, briefData{URL: next.Pick.URL, Repo: r.Repo, Number: r.N})
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s %d", repoName(r.Repo), r.N)
	args := []string{agentsName, agentStartName, name, brief}
	if len(l.a.cfg.Capacity.Choices) > 0 {
		// The start takes capacity's choice of harness and model, as a
		// reviewer's does.
		args = append(args, "--harness", harnessAuto)
	}
	if err := l.claim(act, name); err != nil {
		return "", err
	}
	if _, err := l.run(ctx, args...); err != nil {
		if Code(err) == ExitRefused {
			return "", errLoopLater
		}
		return "", err
	}
	return fmt.Sprintf("%q started on %s", name, next.Pick.URL), nil
}

// install takes release act.Version through its stages, each resumed on a
// later pass while the release's claim stands: the install (self-update,
// while --check still finds it newer), the broker on the installed binary,
// supervisor.postInstall (once), then --check finding it the latest.
func (l *supervisorLoop) install(ctx context.Context, act superloop.Action) (string, error) {
	version := act.Version
	if err := l.claim(act, version); err != nil {
		return "", err
	}
	_, code, err := beekeeperRun(ctx, l.as(selfUpdateCheck...)...)
	if err != nil {
		return "", err
	}
	if code == ExitOutdated {
		if _, err := l.run(ctx, selfUpdateCmdName); err != nil {
			return "", err
		}
	}
	what := []string{version + " installed"}
	broker, err := loopBroker(ctx)
	if errors.Is(err, errLoopLater) {
		l.sayOnce("broker "+act.Key, "SUPERVISOR RELEASE WAITS %s: %s, its restart waits for them", version, broker)
		return "", errLoopLater
	}
	if err != nil {
		return "", err
	}
	what = append(what, broker)
	if argv := l.a.cfg.Supervisor.PostInstall; len(argv) > 0 {
		stage := act.Key + " post-install"
		if !l.stages[stage] {
			out, errOut, code, err := loopCommand(ctx, "", argv)
			if err != nil || code != 0 {
				return "", fmt.Errorf("%s installed, supervisor.postInstall failed (exit %d, %v): %s", version, code, err, oneLine(out+errOut))
			}
			if err := l.a.store.Record(event(loopParty, loopStageVerb, "[%s] ran", stage)); err != nil {
				return "", err
			}
			l.stages[stage] = true
		}
		what = append(what, "supervisor.postInstall ran")
	}
	out, code, err := beekeeperRun(ctx, l.as(selfUpdateCheck...)...)
	if err != nil || code != 0 {
		return "", fmt.Errorf("%s installed, but self-update --check exits %d: %s", version, code, oneLine(out))
	}
	return strings.Join(append(what, "the latest"), ", "), nil
}

// classification is the classifier's answer.
type classification struct {
	Class      string  `json:"class"`
	Confidence float64 `json:"confidence"`
}

// classifierInput is the event the classifier reads on stdin.
type classifierInput struct {
	Verb   string    `json:"verb"`
	Agent  string    `json:"agent"`
	Detail string    `json:"detail"`
	At     time.Time `json:"at"`
}

// loopClasses are the classes a classifier may answer.
var loopClasses = []string{"needs-person", "blocked", "failed", "question"}

// classify routes an event the rules cannot place to the guide, with the
// classifier's class when it is sure enough.
func (l *supervisorLoop) classify(ctx context.Context, st *state.State, act superloop.Action) (string, error) {
	k := l.a.cfg.Supervisor
	e := *act.Event
	label := "unclassified"
	why := "beekeeper's supervisor loop has no rule for it and no classifier (supervisor.classifier)"
	if len(k.Classifier) > 0 {
		in, _ := json.Marshal(classifierInput{Verb: e.Verb, Agent: e.Agent, Detail: e.Detail, At: e.At})
		// Its stdout is the answer; its stderr is for the failure.
		out, errOut, code, err := loopCommand(ctx, string(in), k.Classifier)
		var c classification
		switch {
		case err != nil || code != 0:
			why = fmt.Sprintf("its classifier failed (exit %d, %v): %s", code, err, oneLine(errOut))
		case json.Unmarshal([]byte(strings.TrimSpace(out)), &c) != nil || !slices.Contains(loopClasses, c.Class):
			why = "its classifier answered no class: " + oneLine(out)
		case c.Confidence < k.ClassifierConfidence:
			why = fmt.Sprintf("its classifier says %s at confidence %.2f, below %.2f", c.Class, c.Confidence, k.ClassifierConfidence)
		default:
			label = c.Class
			why = fmt.Sprintf("its classifier says %s (confidence %.2f), which only a person or the guide resolves", c.Class, c.Confidence)
		}
	}
	ref := agentRef(st, e.Agent)
	if ref == "" {
		return "", fmt.Errorf("%s's %s names no issue or pull request to file the guide's note on", e.Agent, e.Verb)
	}
	q := fmt.Sprintf("Decide what %s does next about its %s (%s)?", e.Agent, strings.TrimPrefix(e.Verb, "agents."), label)
	if err := l.toGuide(ctx, act, ref, q, fmt.Sprintf("%s logged %s at %s: %s", e.Agent, e.Verb, e.At.UTC().Format(time.RFC3339), e.Detail), why,
		fmt.Sprintf("leave %s as it is: it keeps its task", e.Agent)); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s to the guide (%s) on %s", e.Verb, label, ref), nil
}

// agentRef is the issue or pull request the agent works: what it is
// parked on, else its serve record, else the first one its task names.
func agentRef(st *state.State, name string) string {
	for _, ag := range st.Agents {
		if ag.Name != name {
			continue
		}
		if ag.Park != nil && superloop.IsPull(ag.Park.On) {
			return ag.Park.On
		}
		for _, r := range st.Records {
			if r.Session.Is(ag.Party) && r.Issue != "" {
				return r.Issue
			}
		}
		for _, task := range []string{ag.Task, ag.LastTask} {
			if m := refURL.FindString(task); m != "" {
				return m
			}
		}
	}
	return ""
}

// toGuide files a decision for the guide's person on ref (note add).
func (l *supervisorLoop) toGuide(ctx context.Context, act superloop.Action, ref, question, statusQuo, why, dflt string) error {
	person := l.a.cfg.Guide.Person
	if person == "" {
		return errors.New("guide.person is unset: no guide's queue to file it in")
	}
	question = truncate(question, 150)
	if err := l.claim(act, question); err != nil {
		return err
	}
	_, err := l.run(ctx, "note", loopAdd, "--for", person, "--ref", ref,
		"--due", l.a.cfg.Supervisor.NoteDue.String(),
		"--status-quo", truncate(statusQuo, 2900), "--why", why, "--default", dflt,
		"--checked", "beekeeper supervisor loop: gh pr view and the event log", question)
	return err
}

// loopEventAgent is the roster name of the agent e concerns: the name its
// detail opens with, else the roster entry of the party that logged it,
// else that party's name.
func loopEventAgent(st *state.State, e state.Event) string {
	by := e.By.Name
	if i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(e.By) }); i >= 0 {
		by = st.Agents[i].Name
	}
	return superloop.EventAgent(e.Verb, e.Detail, by)
}

// fireTimers takes only timers with wakes or commands; watch retains line-only
// reminders and their desktop notifications. Checks run outside the state
// lock; settling inside Update lets a concurrent watch take a timer first
// without either firing it twice. Side effects follow the successful save.
func (l *supervisorLoop) fireTimers(ctx context.Context, st *state.State) error {
	a := l.a
	var acting []state.Timer
	for _, t := range st.Timers {
		if timerActs(t) {
			acting = append(acting, t)
		}
	}
	found := checkTimers(ctx, acting, a.now, lowBudget(st.Budget, a.cfg.GitHub.Floor, a.now) || a.resetFirst(st))
	// Avoid a write on every idle loop pass.
	_, _, _, changed := settleTimersMatching(st, found, a.now, timerActs)
	if !changed {
		return nil
	}
	var lines []string
	var fires []timerFire
	if err := a.store.Update(func(current *state.State) ([]state.Event, error) {
		var evs []state.Event
		lines, evs, fires, _ = settleTimersMatching(current, found, a.now, timerActs)
		return evs, nil
	}); err != nil {
		return err
	}
	// Each batch keeps its own time: later passes update a.now while these
	// wakes and commands can still be running.
	snapshot := *a
	w := &watcher{app: &snapshot}
	for _, line := range lines {
		w.emitNow("timer", "%s", line)
	}
	l.timerActs.Go(func() {
		w.actTimers(ctx, fires)
		w.timerActs.Wait()
	})
	return nil
}
