package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// loopParty is the party the supervisor loop acts and logs as.
var loopParty = state.Party{Name: "supervisor loop"}

// loopStartVerb logs each start of the loop; the first one bounds the events
// it acts on.
const loopStartVerb = "supervisor.loop"

// loopEventVerbs are the events the loop's rules act on.
var loopEventVerbs = []string{superloop.VerbDone, superloop.VerbReport, superloop.VerbProblem, superloop.VerbPark, superloop.VerbNoteAdd}

// beekeeperRun runs this beekeeper binary with args for the loop: its
// output and exit code. An error is a run that did not exit.
var beekeeperRun = func(ctx context.Context, args ...string) (string, int, error) {
	bin, err := os.Executable()
	if err != nil {
		return "", 0, err
	}
	return runArgv(ctx, "", append([]string{bin}, args...))
}

// loopCommand runs a configured command (the classifier, the post-install)
// with stdin: its output and exit code.
var loopCommand = runArgv

// runArgv runs argv with stdin: its stdout (and stderr) and exit code.
func runArgv(ctx context.Context, stdin string, argv []string) (string, int, error) {
	if len(argv) == 0 {
		return "", 0, errors.New("no command")
	}
	c := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // the loop's own binary or a configured command
	c.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.String(), ee.ExitCode(), nil
	}
	return out.String(), 0, err
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

  review    an agent parked on its pull request (agents park --on
            owner/repo#n), every check on the head green, no verdict on
            the head and a review round left (supervisor.reviewRounds,
            default 1): a reviewer starts (agents start --review) with
            supervisor.reviewBrief or the built-in brief; a worker on a
            self-hosted provider's model gets one on no self-hosted
            provider (--harness auto --exclude <provider>)
  go        the latest verdict on the head approves, or every round is used
            and the worker parked on a green head past the last verdict
            (its fixes): the merge go for that head, told to the worker
            (agents wake) with --match-head-commit <head>
  forward   the latest verdict on the head asks for changes: the review,
            word for word, to the worker
  escalate  every round used, the last verdict asked for changes and the
            worker parked again on that head: a decision for guide.person
            (note add); the loop leaves that pull request alone from then
  end       a reviewed pull request merged or closed: its tracking ends
  done      agents.done: the agent leaves the roster (agents remove)
  next      after a done, while capacity has room: the next item of board
            next starts (agents start) with supervisor.workerBrief; none
            configured, none starts
  release   a newer release: self-update (verified, installed, the
            sandbox's broker restarted), supervisor.postInstall, then
            self-update --check must find it the latest
  classify  a report or park the rules cannot place (agents.report without
            agents.done, agents.problem, a park on nothing): the
            supervisor.classifier command, given the event as JSON on
            stdin, answers {"class":"needs-person|blocked|failed|question",
            "confidence":0.9}; the event goes to the guide (note add for
            guide.person) with its class, or unclassified below
            supervisor.classifierConfidence (0.8) or without a classifier
  question  a worker's note for someone other than guide.person: the note
            is re-addressed to guide.person, into the guide's queue

The briefs are Go templates over .URL, .Repo, .Number, .Head, .Round and
.Worker. A pull request is read (one gh pr view) only while an agent is
parked on it or the loop reviews it. Every transition happens once: a
restarted loop reads its own log lines back and takes no key twice; a
failed one is tried again next pass, its failure printed once. The loop
acts on the events logged since its first start.`,
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
}

func (a *app) runSupervisorLoop(ctx context.Context, once bool) error {
	l, err := a.newSupervisorLoop()
	if err != nil {
		return err
	}
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
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	in := superloop.Input{Rounds: a.cfg.Supervisor.ReviewRounds, Person: a.cfg.Guide.Person}
	for _, ag := range st.Agents {
		w := superloop.Worker{Name: ag.Name, SelfHosted: selfHostedModel(a.cfg.Capacity, ag.Model)}
		if ag.Park != nil {
			w.ParkedOn, w.ParkedAt = ag.Park.On, ag.Park.At
		}
		in.Workers = append(in.Workers, w)
	}
	for _, n := range st.Notes {
		if n.Kind != noteLogin {
			in.Notes = append(in.Notes, superloop.Note{ID: n.ID, For: n.For, By: n.By.Name, Text: n.Text})
		}
	}
	taken, err := a.store.Follow("supervisor-loop-taken", func(e state.Event) bool { return strings.HasPrefix(e.Verb, "supervisor.") })
	if err != nil {
		return err
	}
	for _, e := range taken {
		if t, ok := superloop.ParseTaken(e.At, e.Verb, e.Subject, e.Detail); ok {
			in.Taken = append(in.Taken, t)
		}
	}
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
	if a.now.Sub(l.releaseAt) >= a.cfg.Supervisor.ReleaseEvery.Duration {
		l.releaseAt = a.now
		in.Release = l.newerRelease(ctx)
	}
	for _, act := range superloop.Plan(in) {
		what, err := l.act(ctx, st, act)
		switch {
		case errors.Is(err, errLoopLater):
			continue
		case err != nil:
			l.sayOnce(act.Key, "SUPERVISOR %s FAILED %s: %v", strings.ToUpper(act.Kind), act.Key, err)
			continue
		}
		detail := superloop.Detail(act, what)
		if err := a.store.Record(about(event(loopParty, "supervisor."+act.Kind, "%s", detail), act.Ref)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(a.out, "SUPERVISOR %s %s\n", strings.ToUpper(act.Kind), detail); err != nil {
			return err
		}
	}
	return nil
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
var selfUpdateCheck = []string{"self-update", "--check"}

// newerLine is self-update --check's line for a newer release.
var newerLine = regexp.MustCompile(`(\S+) is newer than`)

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
		if _, err := l.run(ctx, agentsName, "wake", act.Agent, act.Message); err != nil {
			return "", err
		}
		return fmt.Sprintf("merge go for %s at %s, told", act.URL, act.Head), nil
	case superloop.KindForward:
		if _, err := l.run(ctx, agentsName, "wake", act.Agent, act.Message); err != nil {
			return "", err
		}
		return fmt.Sprintf("the review of %s asking for changes on %s forwarded", act.URL, act.Head), nil
	case superloop.KindEscalate:
		if err := l.toGuide(ctx, act.URL,
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
		if !slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == act.Agent }) {
			return "already off the roster", nil
		}
		if _, err := l.run(ctx, agentsName, "remove", act.Agent); err != nil {
			return "", err
		}
		return "off the roster", nil
	case superloop.KindNext:
		return l.startNext(ctx)
	case superloop.KindRelease:
		return l.install(ctx, act.Version)
	case superloop.KindClassify:
		return l.classify(ctx, st, act)
	case superloop.KindQuestion:
		if a.cfg.Guide.Person == "" {
			return "", errors.New("guide.person is unset: no guide's queue to put it in")
		}
		person := a.cfg.Guide.Person
		err := a.store.Update(func(st *state.State) ([]state.Event, error) {
			i := slices.IndexFunc(st.Notes, func(n state.Note) bool { return n.ID == act.Note })
			if i < 0 {
				return nil, fmt.Errorf("note #%d is closed", act.Note)
			}
			st.Notes[i].For = person
			return nil, nil
		})
		if err != nil {
			return "", err
		}
		return "re-addressed to the guide's person: " + act.Message, nil
	}
	return "", fmt.Errorf("no transition for %s", act.Kind)
}

// briefData is what a brief template renders.
type briefData struct {
	URL, Repo, Head, Worker string
	Number, Round           int
}

// defaultReviewBrief is the review brief without supervisor.reviewBrief.
const defaultReviewBrief = `Review {{.URL}} at head {{.Head}}, review round {{.Round}}, the work of {{.Worker}}.

Read the pull request, the issue it closes and the diff. Judge whether it does what the issue asks, and find its bugs. Post one review on the pull request (gh pr review {{.Number}} --repo {{.Repo}} --comment --body-file <file>): its first line "Review: approved" or "Review: changes requested", its second "Head reviewed: ` + "`{{.Head}}`" + `", then every blocking finding with its file and line. Then report: beekeeper agents idle --done --report "<the verdict and the review's URL>".
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
	name := fmt.Sprintf("Review %s %d r%d", repoName(r.Repo), r.N, act.Round)
	args := []string{agentsName, agentStartName, name, brief, "--review"}
	if k := l.a.cfg.Capacity; len(k.Choices) > 0 {
		args = append(args, "--harness", harnessAuto)
		if act.SelfHosted {
			for _, p := range selfHostedProviders(k) {
				args = append(args, "--exclude", p)
			}
		}
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

// selfHostedProviders are the self-hosted providers of k, sorted.
func selfHostedProviders(k config.Capacity) []string {
	var out []string
	for name, p := range k.Providers {
		if p.SelfHosted() {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
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
func (l *supervisorLoop) startNext(ctx context.Context) (string, error) {
	k := l.a.cfg.Supervisor
	if k.WorkerBrief == "" {
		return "no supervisor.workerBrief: no worker starts", nil
	}
	if _, code, err := beekeeperRun(ctx, l.as("capacity")...); err != nil || code != 0 {
		return "", errLoopLater
	}
	out, err := l.run(ctx, "board", "next", "--json")
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
	if _, err := l.run(ctx, agentsName, "start", name, brief); err != nil {
		if Code(err) == ExitRefused {
			return "", errLoopLater
		}
		return "", err
	}
	return fmt.Sprintf("%q started on %s", name, next.Pick.URL), nil
}

// install installs release version and checks it.
func (l *supervisorLoop) install(ctx context.Context, version string) (string, error) {
	if _, err := l.run(ctx, "self-update"); err != nil {
		return "", err
	}
	what := version + " installed"
	if argv := l.a.cfg.Supervisor.PostInstall; len(argv) > 0 {
		out, code, err := loopCommand(ctx, "", argv)
		if err != nil || code != 0 {
			return "", fmt.Errorf("%s installed, supervisor.postInstall failed (exit %d, %v): %s", version, code, err, oneLine(out))
		}
		what += ", supervisor.postInstall ran"
	}
	out, code, err := beekeeperRun(ctx, l.as(selfUpdateCheck...)...)
	if err != nil || code != 0 {
		return "", fmt.Errorf("%s installed, but self-update --check exits %d: %s", version, code, oneLine(out))
	}
	return what + ", the latest", nil
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
		out, code, err := loopCommand(ctx, string(in), k.Classifier)
		var c classification
		switch {
		case err != nil || code != 0:
			why = fmt.Sprintf("its classifier failed (exit %d, %v)", code, err)
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
	if err := l.toGuide(ctx, ref, q, fmt.Sprintf("%s logged %s at %s: %s", e.Agent, e.Verb, e.At.UTC().Format(time.RFC3339), e.Detail), why,
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
func (l *supervisorLoop) toGuide(ctx context.Context, ref, question, statusQuo, why, dflt string) error {
	person := l.a.cfg.Guide.Person
	if person == "" {
		return errors.New("guide.person is unset: no guide's queue to file it in")
	}
	_, err := l.run(ctx, "note", "add", "--for", person, "--ref", ref,
		"--due", l.a.cfg.Supervisor.NoteDue.String(),
		"--status-quo", truncate(statusQuo, 2900), "--why", why, "--default", dflt,
		"--checked", "beekeeper supervisor loop: gh pr view and the event log", truncate(question, 150))
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
