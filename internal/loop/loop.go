package loop

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"text/template"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/claude"
	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
	"github.com/giantswarm/beekeeper/internal/update"
)

// LoopParty is the party the supervisor loop acts and logs as.
var LoopParty = state.Party{Name: "supervisor loop"}

// LoopStartVerb logs each start of the loop; the first one bounds the events
// it acts on.
const LoopStartVerb = "supervisor.loop"

// LoopClaimVerb logs an action the loop is about to take, before its side
// effect; LoopStageVerb a finished stage of a release; LoopUnstageVerb a
// stage found obsolete, which no later pass resumes.
// LoopGoVerb logs a merge go given to a worker, LoopMergeVerb a merge the
// loop ran itself.
const (
	LoopClaimVerb    = "supervisor.claim"
	LoopGoVerb       = "supervisor." + superloop.KindGo
	LoopMergeVerb    = "supervisor." + superloop.KindMerge
	LoopEndVerb      = "supervisor." + superloop.KindEnd
	LoopFileVerb     = "supervisor." + superloop.KindFile
	LoopClaimEndVerb = "supervisor.unclaim"
	LoopStageVerb    = "supervisor.stage"
	LoopUnstageVerb  = "supervisor.unstage"
	LoopFailureVerb  = "supervisor.failure"
	// LoopOwnerlessVerb logs since when an open pull request has had no
	// live owner ("[<ref>] since <time>"), and its end ("[<ref>] owned"),
	// so a restarted loop's sweep keeps the stretch.
	LoopOwnerlessVerb = "supervisor.ownerless"
)

// loopEventVerbs are the events the loop's rules act on.
var loopEventVerbs = []string{superloop.VerbDone, superloop.VerbReport, superloop.VerbProblem, superloop.VerbPark}

// ExecutablePath returns the running binary's filesystem path, stripping the
// Linux " (deleted)" marker when an in-place update or rollback replaced it.
func ExecutablePath() (string, error) {
	if b := platform.RunningBinary(); b != nil && b.Path != "" {
		return b.Path, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(exe, " (deleted)"), nil
}

// BeekeeperRun runs this beekeeper binary with args for the loop: its
// output (stdout, then stderr) and exit code. An error is a run that did
// not exit.
var BeekeeperRun = func(ctx context.Context, args ...string) (string, int, error) {
	bin, err := ExecutablePath()
	if err != nil {
		return "", 0, err
	}
	out, errOut, code, err := RunArgv(ctx, "", append([]string{bin}, args...))
	return out + errOut, code, err
}

// LoopCommand runs a configured command (the classifier, the post-install)
// with stdin: its stdout, its stderr and its exit code.
var LoopCommand = RunArgv

// RunArgv runs argv with stdin: its stdout, its stderr and its exit code.
func RunArgv(ctx context.Context, stdin string, argv []string) (string, string, int, error) {
	if len(argv) == 0 {
		return "", "", 0, errors.New("no command")
	}
	c := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // the loop's own binary or a configured command
	c.Env = Cmd.DevctlCommandEnv("")
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

// Replaceable is the running binary as the loop sees it: whether an
// install renamed another file over its path, and its re-execution.
type Replaceable interface {
	Replaced() bool
	Exec(env ...string) error
}

// LoopBinary is the binary the loop runs; nil where it cannot tell.
var LoopBinary = func() Replaceable {
	if b := platform.RunningBinary(); b != nil {
		return b
	}
	return nil
}

// ReadLoopPull reads a tracked pull request (owner/repo#n): its state, head,
// the checks on the head and its reviews.
var ReadLoopPull = func(ctx context.Context, ref string) (superloop.Pull, error) {
	r, ok := Cmd.ParseRef(ref)
	if !ok {
		return superloop.Pull{}, fmt.Errorf("%s names no pull request", ref)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := github.RunGH(ctx, "pr", "view", strconv.Itoa(r.N), "--repo", r.Repo, "--json", "state,url,headRefOid,baseRefName,statusCheckRollup,reviews,author,title,body,headRefName,files,changedFiles,additions,deletions,mergeCommit,isDraft,mergeable,mergeStateStatus,reviewDecision,closingIssuesReferences,updatedAt")
	if err != nil {
		return superloop.Pull{}, fmt.Errorf("gh pr view %d --repo %s: %w", r.N, r.Repo, err)
	}
	p, err := ParseLoopPull(ref, out)
	if err != nil || p.State != "OPEN" {
		return p, err
	}
	// A newer suite may fail before creating a required job. The rollup
	// still contains that job's old success, so also read workflow runs.
	runs, err := github.RunGH(ctx, "api", "--paginate", fmt.Sprintf("repos/%s/actions/runs?head_sha=%s&per_page=100", r.Repo, p.Head))
	if err != nil {
		return superloop.Pull{}, fmt.Errorf("read workflow runs for %s: %w", ref, err)
	}
	var snapshot LoopPullJSON
	if err := json.Unmarshal(out, &snapshot); err != nil {
		return superloop.Pull{}, err
	}
	if err := ApplyLoopWorkflowRuns(&p, runs, snapshot.Rollup); err != nil {
		return superloop.Pull{}, err
	}
	return p, nil
}

// LoopWorkflowRun identifies a workflow independently of its display name.
// Run IDs order new suites; run attempts order reruns of the same suite.
type LoopWorkflowRun struct {
	ID         uint64 `json:"id"`
	WorkflowID uint64 `json:"workflow_id"`
	Attempt    int    `json:"run_attempt"`
	Head       string `json:"head_sha"`
	Name       string `json:"name"`
	URL        string `json:"html_url"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

func ApplyLoopWorkflowRuns(p *superloop.Pull, raw []byte, rollup []LoopCheck) error {
	latest := map[uint64]LoopWorkflowRun{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	pages := 0
	for {
		var page struct {
			Runs []LoopWorkflowRun `json:"workflow_runs"`
		}
		if err := dec.Decode(&page); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("read workflow runs: %w", err)
		}
		if page.Runs == nil {
			return errors.New("read workflow runs: missing workflow_runs")
		}
		pages++
		for _, run := range page.Runs {
			if run.Head != p.Head {
				continue
			}
			if run.ID == 0 || run.WorkflowID == 0 {
				return errors.New("read workflow runs: missing run or workflow ID")
			}
			old, seen := latest[run.WorkflowID]
			if !seen || run.ID > old.ID || run.ID == old.ID && run.Attempt > old.Attempt {
				latest[run.WorkflowID] = run
			}
		}
	}
	if pages == 0 {
		return errors.New("read workflow runs: empty response")
	}
	for _, run := range latest {
		// An old job absent from the newest suite is not evidence of a
		// passing check, even when the jobs that did run all succeeded.
		for _, check := range rollup {
			id := loopActionsRun(check.DetailsURL)
			if check.WorkflowName == run.Name && id != 0 && id < run.ID && !check.superseded(rollup) {
				p.Green = false
			}
		}
		c := LoopCheck{Status: strings.ToUpper(run.Status), Conclusion: strings.ToUpper(run.Conclusion)}
		if !c.green() {
			p.Green = false
		}
		if c.red() {
			p.Red = append(p.Red, superloop.Check{Name: run.Name, URL: run.URL})
		}
	}
	slices.SortStableFunc(p.Red, func(x, y superloop.Check) int { return strings.Compare(x.Name, y.Name) })
	return nil
}

// ReadBaseMerge reads whether the approval of approved covers p's head: it
// only brings its base into approved (github.ApprovalCovers), by a merge, a
// merge whose conflicts it resolves, or a rebase, with no change of its
// own. A pull request gets one desk review
// (https://github.com/giantswarm/beekeeper/issues/3861): the merge-result
// check against the current base is such a head's gate, never a new review.
var ReadBaseMerge = func(ctx context.Context, p superloop.Pull, approved string) (bool, error) {
	r, ok := Cmd.ParseRef(p.Ref)
	if !ok || p.Base == "" {
		return false, fmt.Errorf("%s names no pull request with a base", p.Ref)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	covered, _, err := github.ApprovalCovers(ctx, github.RunGH, r.Repo, p.Base, approved, p.Head)
	return covered, err
}

// ReadDeskLogin is the login gh acts as: the one the desk's agents open
// their pull requests as, unless supervisor.ownership.authors names them.
var ReadDeskLogin = func(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return github.ViewerLogin(ctx, github.RunGH)
}

// ReadTeams reads supervisor.ownership.teamFiles: the team that owns each
// repository (owner/name, lower-cased).
var ReadTeams = func(ctx context.Context, own config.Ownership) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	repo, dir := own.TeamFilesRepo()
	return github.Teams(ctx, github.RunGH, repo, dir)
}

// teamsEvery is how often the loop reads the team files again.
const teamsEvery = time.Hour

// LoopPullJSON is gh pr view's answer.
type LoopPullJSON struct {
	Additions, Deletions int
	Closes               []struct {
		URL string `json:"url"`
	} `json:"closingIssuesReferences"`
	Updated        time.Time `json:"updatedAt"`
	Draft          bool      `json:"isDraft"`
	Mergeable      string    `json:"mergeable"`
	MergeState     string    `json:"mergeStateStatus"`
	ReviewDecision string    `json:"reviewDecision"`
	MergeCommit    struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
	State  string      `json:"state"`
	URL    string      `json:"url"`
	Head   string      `json:"headRefOid"`
	Base   string      `json:"baseRefName"`
	Rollup []LoopCheck `json:"statusCheckRollup"`
	Author loopLogin   `json:"author"`
	Title  string      `json:"title"`
	Body   string      `json:"body"`
	Branch string      `json:"headRefName"`
	Files  []struct {
		Path string `json:"path"`
	} `json:"files"`
	ChangedFiles int `json:"changedFiles"`
	Reviews      []struct {
		ID          string    `json:"id"`
		Author      loopLogin `json:"author"`
		Association string    `json:"authorAssociation"`
		Body        string    `json:"body"`
		State       string    `json:"state"`
		At          time.Time `json:"submittedAt"`
		Commit      struct {
			OID string `json:"oid"`
		} `json:"commit"`
	} `json:"reviews"`
}

// loopLogin is a GitHub account as gh pr view names it.
type loopLogin struct {
	Login string `json:"login"`
}

// LoopCheck is one entry of a pull request's check rollup: a check run
// reports a status and a conclusion, a commit status a state.
type LoopCheck struct {
	StartedAt    time.Time `json:"startedAt"`
	WorkflowName string    `json:"workflowName"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	State        string    `json:"state"`
	// A check run's name and details URL, a commit status's context and
	// target URL.
	Name       string `json:"name"`
	DetailsURL string `json:"detailsUrl"`
	Context    string `json:"context"`
	TargetURL  string `json:"targetUrl"`
}

// The states of a check run and a commit status the loop reads.
const (
	CheckCompleted = "COMPLETED"
	CheckSuccess   = "SUCCESS"
	CheckFailure   = "FAILURE"
	checkError     = "ERROR"
)

// loopRedConclusions are the conclusions of a check run that are red: a
// terminal state that never turns green by itself.
var loopRedConclusions = []string{CheckFailure, "CANCELLED", "TIMED_OUT", "STARTUP_FAILURE", "ACTION_REQUIRED"}

// green reports whether the check concluded successfully.
func (c LoopCheck) green() bool {
	return c.State == CheckSuccess || c.Status == CheckCompleted && slices.Contains([]string{CheckSuccess, "NEUTRAL", "SKIPPED"}, c.Conclusion)
}

// red reports whether the check concluded red: a run that failed, or a
// status that failed or errored. One queued, running or pending is not.
func (c LoopCheck) red() bool {
	return c.Status == CheckCompleted && slices.Contains(loopRedConclusions, c.Conclusion) || slices.Contains([]string{CheckFailure, checkError}, c.State)
}

// Concluded reports whether the check reached a terminal state.
func (c LoopCheck) Concluded() bool {
	if c.Status != "" {
		return c.Status == CheckCompleted
	}
	return slices.Contains([]string{CheckSuccess, CheckFailure, checkError}, c.State)
}

// superseded reports check runs replaced on this head's rollup. Only the
// newest run of a named check counts, regardless of its outcome. A cancelled
// job can also be replaced by a newer workflow run with renamed jobs. Actions
// run IDs order queued replacements whose startedAt has not been set yet.
func (c LoopCheck) superseded(checks []LoopCheck) bool {
	if c.Status == "" {
		return false
	}
	oldRun := loopActionsRun(c.DetailsURL)
	oldRepo, _, _ := strings.Cut(c.DetailsURL, "/actions/runs/")
	return slices.ContainsFunc(checks, func(next LoopCheck) bool {
		if next.Status == "" {
			return false // A commit status is not a replacement check run.
		}
		newRun := loopActionsRun(next.DetailsURL)
		newRepo, _, _ := strings.Cut(next.DetailsURL, "/actions/runs/")
		newerActionsRun := oldRun != 0 && newRun > oldRun && oldRepo == newRepo
		// Job names are only unique within a workflow. Independent workflows
		// may both have a job named test and must not replace each other.
		sameCheck := c.Name != "" && c.Name == next.Name && c.WorkflowName == next.WorkflowName
		sameWorkflow := c.Conclusion == "CANCELLED" && c.Status == CheckCompleted && c.WorkflowName != "" && c.WorkflowName == next.WorkflowName && newerActionsRun
		if oldRun != 0 && newRun != 0 && oldRun != newRun && oldRepo == newRepo {
			return (sameCheck || sameWorkflow) && newerActionsRun
		}
		return sameCheck && !c.StartedAt.IsZero() && next.StartedAt.After(c.StartedAt)
	})
}

func loopActionsRun(detailsURL string) uint64 {
	// The rollup exposes the Actions run ID through its details URL.
	_, tail, ok := strings.Cut(detailsURL, "/actions/runs/")
	if !ok {
		return 0
	}
	id, _, _ := strings.Cut(tail, "/")
	n, _ := strconv.ParseUint(id, 10, 64)
	return n
}

// ParseLoopPull reads gh pr view's JSON of ref.
func ParseLoopPull(ref string, out []byte) (superloop.Pull, error) {
	var j LoopPullJSON
	if err := json.Unmarshal(out, &j); err != nil {
		return superloop.Pull{}, fmt.Errorf("gh pr view of %s: %w", ref, err)
	}
	p := superloop.Pull{Draft: j.Draft, Mergeable: j.Mergeable, MergeState: j.MergeState, ReviewDecision: j.ReviewDecision, MergeCommit: j.MergeCommit.OID, Ref: ref, URL: j.URL, State: j.State, Head: j.Head, Base: j.Base, Green: len(j.Rollup) > 0, Author: j.Author.Login, Title: j.Title, Body: j.Body, Branch: j.Branch, ChangedFiles: j.ChangedFiles}
	p.Additions, p.Deletions = j.Additions, j.Deletions
	p.Updated = j.Updated
	p.Absorbs = superloop.ParseAbsorbedPulls(ref, j.Title, j.Body)
	for _, issue := range j.Closes {
		if r, ok := Cmd.ParseRef(issue.URL); ok {
			p.Closes = append(p.Closes, Cmd.RefName(r))
		}
	}
	for _, issue := range superloop.ParseClosingIssues(ref, j.Title+"\n"+j.Body) {
		if r, ok := Cmd.ParseRef(issue); ok {
			name := Cmd.RefName(r)
			if !slices.ContainsFunc(p.Closes, func(existing string) bool { return strings.EqualFold(existing, name) }) {
				p.Closes = append(p.Closes, name)
			}
		}
	}
	for _, f := range j.Files {
		p.Files = append(p.Files, f.Path)
	}
	for _, c := range j.Rollup {
		if c.superseded(j.Rollup) {
			continue
		}
		p.Checks = append(p.Checks, cmp.Or(c.Name, c.Context))
		if !c.green() {
			p.Green = false
		}
		if c.red() {
			p.Red = append(p.Red, superloop.Check{Name: cmp.Or(c.Name, c.Context), URL: cmp.Or(c.DetailsURL, c.TargetURL)})
		}
	}
	slices.SortStableFunc(p.Red, func(x, y superloop.Check) int { return strings.Compare(x.Name, y.Name) })
	for _, r := range j.Reviews {
		member := slices.Contains([]string{"MEMBER", "OWNER", "COLLABORATOR"}, r.Association)
		p.Reviews = append(p.Reviews, superloop.Review{ID: r.ID, Author: r.Author.Login, Member: member, Commit: r.Commit.OID, Head: superloop.ReviewedHead(r.Body, r.Commit.OID), Verdict: superloop.Verdict(r.State, r.Body), Body: r.Body, At: r.At})
	}
	slices.SortStableFunc(p.Reviews, func(x, y superloop.Review) int { return x.At.Compare(y.At) })
	return p, nil
}

// Loop is one run of the loop: what it said once.
type Loop struct {
	recordDir string
	Recording *LoopRecording
	Lifecycle *LoopLifecycle
	ReviewDir string // explicit agents review checkout
	Mu        sync.Mutex
	Host      Host
	Since     time.Time
	// startedAt is when this process's loop started: a next action for a
	// done event before it is settled by the refill (staleNext).
	StartedAt time.Time
	Once      bool
	// said are the failures already printed, by key.
	Said map[string]bool
	// releaseAt is when the loop last looked for a release.
	releaseAt      time.Time
	StartsHeld     bool
	StopWatch      func()
	stopPullWatch  func()
	StopRefill     func()
	RefillConfigMu sync.RWMutex
	// tidyAt is when the loop last ran board tidy.
	TidyAt time.Time
	// stages are the release or intake stages finished, by key; stageDetail
	// is the recorded detail of each stage.
	Stages      map[string]bool
	StageDetail map[string]string
	PostMerge   []superloop.Action
	// FillPause defers an unproductive board read until capacity changes,
	// an agent is done or removed, or the configured retry interval passes.
	FillPause FillPause
	// fillEmpty counts the refill's unproductive board reads since its last
	// start or a change that ended its pause; each one doubles the next
	// pause, up to fillBackoff times.
	FillEmpty       int
	FillRetries     map[string]fillRetry
	refillFold      TakenFold
	IndependentFill bool
	fillPhase       string
	// TakenFold is the loop's own history folded so far (looptakenfold.go).
	takenFold TakenFold
	// reviewRecoveries are the desk-review parks whose published HEAD did
	// not verify, by park: when recoverReviewRequests looks again.
	reviewRecoveries map[string]reviewRecovery
	// boardNone is this pass's empty or failed board next answer: every
	// later start of the pass takes it instead of walking the board again.
	BoardNone *loopBoardNone
	// stall is the refill's flow: when it last ran and since when a start
	// waits with room (checkStall alarms on it).
	Stall     startStall
	FlowWaits map[string]flowWait
	// mergeNotes are the holds the pending merge decisions' last runs read,
	// mergeRuns the decisions dispatched to their lane's stream, both by key:
	// the stall alarm names them (checkFlowWaits).
	MergeNotes map[string]mergeNote
	mergeRuns  map[string]mergeRun
	// reviewWaits are the review requests this loop planned and has not
	// started, by pull request (checkStall alarms on the oldest).
	reviewWaits map[string]reviewWait
	// timerActs are timer side effects; shutdown and re-exec await them.
	TimerActs sync.WaitGroup
	// inFlight tracks actions and ordered streams across passes.
	// Both wait groups are drained on exit and before re-exec.
	inFlight     map[string]bool
	MergeActions sync.WaitGroup
	PassActions  sync.WaitGroup
	actionCtx    context.Context
	actionCancel context.CancelFunc
	actionErr    error
	completed    map[string]bool
	// login is the login gh acts as, once read; teams the team files'
	// owners, read at teamsAt.
	Login   string
	Teams   map[string]string
	TeamsAt time.Time
	// dryRun prints a pass's transitions and takes none.
	DryRun bool
	// bin is the loop's running executable, checked at quiet points so an
	// install replaces this process as soon as no claim is in flight.
	Bin          Replaceable
	Reexecuted   bool
	reexecFailed bool
	// baseMerges are the answers of ReadBaseMerge, by pull request,
	// approved head and head: two commits' relation does not change.
	baseMerges map[string]bool
	// opens are the answers of refStates, by issue or pull request,
	// each kept for loopOpenEvery.
	opens map[string]loopOpen
	// merged are the merged pull requests read, by reference.
	Merged map[string]superloop.Pull
	// pulls holds the dated observations consumed by the pass. pullWatch
	// owns their refresh independently of the pass and desk events.
	Pulls     map[string]LoopPullObservation
	pullWatch *loopPullWatcher
	// fillPreview is the dry-run projection, used only by synchronous previews.
	FillPreview *state.State
	// openPulls are the desk's open pull requests the sweep last listed,
	// at openPullsAt.
	OpenPulls       []string
	OpenPullsAt     time.Time
	SweepIncomplete bool
	Unmerged        map[string]LoopUnmergedObservation
	UnmergedCursor  string
	DefaultBranches map[string]string
	cfgSum          [32]byte
}

// loopOpen is whether an issue or pull request was open when read at at.
type loopOpen struct {
	open bool
	at   time.Time
}

// loopOpenEvery is how long the loop keeps a read of whether an issue or
// pull request is open: a kept worker's served issue is not read every pass.
const loopOpenEvery = 10 * time.Minute

// open reports whether the issue or pull request ref is open, read at most
// every loopOpenEvery; one it cannot read counts as open, said once, so no
// worker is retired on a failed read.
func (l *Loop) open(ctx context.Context, ref string) bool {
	if l.PullWatcher().knows(ref) {
		p, err := l.readPull(ctx, ref)
		return err != nil || p.State == superloop.Open
	}
	l.Mu.Lock()
	if o, ok := l.opens[ref]; ok && l.Host.TimeNow().Sub(o.at) < loopOpenEvery {
		l.Mu.Unlock()
		return o.open
	}
	l.Mu.Unlock()
	r, ok := Cmd.ParseRef(ref)
	if !ok {
		return true
	}
	states, err := Cmd.RefStates(ctx, []github.PR{r})
	st, read := states[r]
	if err != nil || !read {
		l.SayOnce("open "+ref, "SUPERVISOR LOOP cannot tell whether %s is open (%v): its worker stays", ref, err)
		return true
	}
	open := st.State == github.Open
	l.Mu.Lock()
	if l.opens == nil {
		l.opens = map[string]loopOpen{}
	}
	l.opens[ref] = loopOpen{open: open, at: l.Host.TimeNow()}
	l.Mu.Unlock()
	return open
}

// liveOpen reports whether the issue or pull request ref is confirmed open by a
// fresh live read. Unlike open, it never uses the 10-minute cache and fails
// closed when the live state cannot be read.
func (l *Loop) liveOpen(ctx context.Context, ref string) bool {
	if l.PullWatcher().knows(ref) {
		p, err := l.readPull(ctx, ref)
		return err == nil && p.State == superloop.Open
	}
	r, ok := Cmd.ParseRef(ref)
	if !ok {
		return false
	}
	states, err := Cmd.RefStates(ctx, []github.PR{r})
	st, read := states[r]
	if err != nil || !read {
		return false
	}
	open := st.State == github.Open
	l.Mu.Lock()
	if l.opens == nil {
		l.opens = map[string]loopOpen{}
	}
	l.opens[Cmd.RefName(r)] = loopOpen{open: open, at: l.Host.TimeNow()}
	l.Mu.Unlock()
	return open
}

// LoopServes is the issue ag's active sessions serve record names, as
// owner/repo#n: "" without one, once its session ended or once it waits
// for "done".
func LoopServes(st *state.State, ag state.Agent) string {
	for i := range st.Records {
		r := &st.Records[i]
		if !r.Session.Is(ag.Party) {
			continue
		}
		if !r.Ended.IsZero() || strings.HasPrefix(strings.ToLower(r.Waits), "done") {
			return ""
		}
		if ref, ok := Cmd.ParseRef(r.Issue); ok {
			return Cmd.RefName(ref)
		}
		return ""
	}
	return ""
}

func LoopTaskServes(ag state.Agent, parkedOn string) string {
	for _, task := range []string{ag.Task, ag.LastTask} {
		if m := Cmd.RefURL().FindString(task); m != "" && !strings.Contains(strings.ToLower(m), "/pull/") {
			if r, ok := Cmd.ParseRef(m); ok && !strings.EqualFold(Cmd.RefName(r), parkedOn) {
				return Cmd.RefName(r)
			}
		}
	}
	return ""
}

// loopOutput serializes parallel loop actions and asynchronous timer output.
type loopOutput struct {
	mu  sync.Mutex
	out io.Writer
}

func (w *loopOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(p)
}

// RunLoop runs the loop: every pass until ctx ends, or one with once.
func RunLoop(ctx context.Context, a Host, once bool) (runErr error) {
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// One loop per state: a second would take each action again.
	lock := flock.New(filepath.Join(a.Store().Dir(), LoopLockFile))
	held, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !held {
		return Cmd.Refused("another supervisor loop runs on this state (%s is locked)", lock.Path())
	}
	defer func() { _ = lock.Unlock() }()
	life := &LoopLifecycle{Version: NormalizeLoopVersion(LoopRunningVersion()), Started: clk.Now().UTC(), Phase: LoopPhaseStartup}
	life.At = life.Started
	_ = a.Store().WriteFile(LoopLifecycleFile, life)
	defer func() {
		crash := recover()
		life.At, life.Exited, life.Failure = clk.Now().UTC(), true, LoopFailureKind(runErr)
		if runErr != nil {
			life.Code = Cmd.Code(runErr)
		}
		if crash != nil {
			life.Code, life.Failure = ExitError, "panic (see candidate service journal)"
		}
		_ = a.Store().WriteFile(LoopLifecycleFile, life)
		if crash != nil {
			panic(crash)
		}
	}()
	// The stop signal bounds the exit, however the pass is stuck.
	shutdownDone := make(chan struct{})
	defer close(shutdownDone)
	go BoundLoopShutdown(ctx, shutdownDone, a.Store(), a.Out())
	// Observers read a PID marker instead of briefly contending for this lock.
	if err := a.Store().WriteFile(LoopPIDFile, os.Getpid()); err != nil {
		return err
	}
	defer func() { _ = a.Store().WriteFile(LoopPIDFile, 0) }()
	if err := a.WriteLoopVersion(); err != nil {
		return err
	}
	defer a.ClearLoopVersion()
	out := a.Out()
	defer func() { a.SetOut(out) }()
	if a.LoopNow() == nil {
		a.SetLoopNow(&atomic.Pointer[time.Time]{})
	}
	defer a.LoopNow().Store(nil)
	if err := a.PrepareEventSegmentation(); err != nil {
		return err
	}
	startup := &Loop{Host: a, Lifecycle: life}
	startup.LifecyclePhase(LoopPhaseStartupHistory)
	l, err := NewLoop(a, false, func() { startup.LifecyclePhase(LoopPhaseStartupHistory) })
	if err != nil {
		return err
	}
	l.Once = once
	if !once {
		l.stopPullWatch = StartLoopPullWatcher(ctx, l)
	}
	defer func() {
		if l.stopPullWatch != nil {
			l.stopPullWatch()
		}
	}()
	l.Lifecycle = life
	if !once && !l.DryRun {
		l.LoadCache()
	}
	defer func() {
		l.CancelBackgroundActions()
		cancel()
		l.PassActions.Wait()
		l.MergeActions.Wait()
		l.TimerActs.Wait()
		if !once && !l.DryRun {
			l.SaveCache()
		}
	}()
	if !once {
		l.StopWatch = a.StartLoopWatch(ctx)
		defer func() {
			if l.StopWatch != nil {
				l.StopWatch()
			}
		}()
	}
	if !once {
		l.IndependentFill = true
		l.HoldStarts(true) // the first watch observation must admit starts
		l.StopRefill = l.startRefillCadence(ctx)
		defer func() {
			if l.StopRefill != nil {
				l.StopRefill()
			}
		}()
	}
	for {
		now := clk.Now()
		if once {
			a.SetNow(now)
		}
		a.LoopNow().Store(&now)
		l.Reexecuted = false
		l.Mu.Lock()
		l.reexecFailed = false
		l.Mu.Unlock()
		l.LifecyclePhase("begin pass")
		if err := l.Pass(ctx); err != nil {
			if reexec, rerr := l.Reexec(ctx); rerr != nil {
				return rerr
			} else if reexec {
				if once {
					return nil
				}
				continue
			}
			fenced := errors.Is(err, state.ErrRecoveryWriterFenced) || state.WriterFenced(a.Store())
			if (!LockBusy(err) && !fenced) || once {
				return err
			}
			// A busy state lock or recovery writer fence fails this pass, not
			// the loop: exiting on a busy lock would cancel every action still
			// running, and a fenced candidate must stay alive long enough to
			// re-execute the restored binary on the next tick.
			if fenced {
				l.SayOnce("recovery writer fenced", "SUPERVISOR LOOP pass skipped while binary recovery finishes: %v", err)
			} else {
				l.SayOnce("state lock busy", "SUPERVISOR LOOP pass skipped, retrying next pass: %v", err)
			}
			if !l.WaitNextPass(ctx) {
				return nil
			}
			continue
		}
		l.ClearSaid("state lock busy")
		l.ClearSaid("recovery writer fenced")
		if !once && !l.DryRun {
			l.SaveCache()
		}
		l.LifecyclePhase(LoopPhaseBetween)
		if _, rerr := l.Reexec(ctx); rerr != nil {
			return rerr
		}
		if once {
			return nil
		}
		if l.Reexecuted {
			continue
		}
		if !l.WaitNextPass(ctx) {
			return nil
		}
	}
}

// LockBusy says err is a wait for the state lock that ended before its
// holder released it.
func LockBusy(err error) bool {
	var busy *state.LockBusyError
	return errors.As(err, &busy) || errors.Is(err, execution.ErrCoordinatorBusy)
}

// WaitNextPass waits for LoopInterval, ctx.Done(), or a replaced binary so
// an external self-update during the idle sleep triggers re-execution
// without waiting the full LoopInterval on the old binary.
func (l *Loop) WaitNextPass(ctx context.Context) bool {
	d := l.Host.Cfg().Supervisor.LoopInterval.Duration
	if d <= 0 {
		return ctx.Err() == nil
	}
	changes, closeChanges := state.Changes(ctx, filepath.Join(l.Host.Store().Dir(), "state.json"), filepath.Join(l.Host.Store().Dir(), "events.jsonl"))
	defer closeChanges()
	ended := clk.Now()
	// A change wakes the next pass early, but not sooner than PassChangeGap
	// after this one ended, nor later than the interval.
	changed := func() bool { return Cmd.PauseChangeFor(ctx, min(PassChangeGap, d)-clk.Since(ended)) }
	timer := clk.NewTimer(d)
	defer timer.Stop()
	const checkEvery = time.Second
	if d <= checkEvery {
		select {
		case <-ctx.Done():
			return false
		case <-l.PullWatcher().ready:
			return true
		case <-changes:
			return changed()
		case <-timer.C:
			return true
		}
	}
	ticker := clk.NewTicker(checkEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-l.PullWatcher().ready:
			return true
		case <-changes:
			return changed()
		case <-timer.C:
			return true
		case <-ticker.C:
			b := l.Bin
			if b == nil {
				b = LoopBinary()
				l.Mu.Lock()
				l.Bin = b
				l.Mu.Unlock()
			}
			if b != nil && b.Replaced() && !l.reexecFailed && (!l.ReexecBusy() || (l.Host != nil && state.WriterFenced(l.Host.Store()))) {
				return true
			}
		}
	}
}

// LoopLockFile is the lock one running loop holds, beside the state.
const LoopLockFile = "supervisor-loop.lock"

// LoopPIDFile is the running loop's PID marker, beside the state: 0 once
// it stopped.
const LoopPIDFile = "supervisor-loop-pid.json"

// DryRunLoop is one pass that says what it would take: it runs
// only read-only queries, logs nothing, fires no timer and takes no lock,
// so it can run beside the loop.
func DryRunLoop(ctx context.Context, a Host, recordDirs ...string) error {
	l, err := NewLoop(a, true)
	if err != nil {
		return err
	}
	if len(recordDirs) > 0 {
		l.recordDir = recordDirs[0]
	}
	a.SetNow(clk.Now())
	if err := l.Pass(ctx); err != nil {
		return err
	}
	if l.recordDir != "" {
		if l.Recording == nil {
			return fmt.Errorf("no complete planner input to record")
		}
		return WriteLoopRecording(l.recordDir, *l.Recording)
	}
	return nil
}

// NewLoop logs the loop's start, unless it is a dry run, and
// bounds its events by its first.
func NewLoop(a Host, dryRun bool, progress ...func()) (*Loop, error) {
	// Every loop can run actions concurrently, including a directly constructed
	// loop. Give all its output paths the same synchronized writer.
	if _, ok := a.Out().(*loopOutput); !ok {
		a.SetOut(&loopOutput{out: a.Out()})
	}
	keepStart := func(e state.Event) bool { return e.Verb == LoopStartVerb }
	if len(progress) > 0 {
		keepStart = EventProgressFilter(keepStart, progress[0], clk.Now)
	}
	starts, err := state.FollowSnapshot(a.Store(), "supervisor-loop-starts", "v1", keepStart)
	if err != nil {
		return nil, err
	}
	l := &Loop{Host: a, Since: a.TimeNow().UTC(), StartedAt: a.TimeNow().UTC(), Said: map[string]bool{}, baseMerges: map[string]bool{}, DryRun: dryRun, Bin: LoopBinary()}
	if a.LoadedCfg() != "" {
		if raw, readErr := os.ReadFile(a.LoadedCfg()); readErr == nil { //nolint:gosec // configured path
			l.cfgSum = sha256.Sum256(raw)
		}
	}
	if len(starts) > 0 {
		l.Since = starts[0].At
	}
	if dryRun {
		return l, nil
	}
	if err := a.Store().Record(Cmd.Event(LoopParty, LoopStartVerb, "started; acting on the events since %s", l.Since.Format(time.RFC3339))); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Loop) ReexecBusy() bool {
	if l == nil {
		return false
	}
	l.Mu.Lock()
	defer l.Mu.Unlock()
	return len(l.inFlight) > 0
}

func (l *Loop) BackgroundContext(ctx context.Context) context.Context {
	if l == nil {
		return ctx
	}
	l.Mu.Lock()
	defer l.Mu.Unlock()
	if l.actionCtx == nil || l.actionCtx.Err() != nil {
		l.actionCtx, l.actionCancel = context.WithCancel(ctx)
	}
	return l.actionCtx
}

func (l *Loop) CancelBackgroundActions() {
	if l == nil {
		return
	}
	l.Mu.Lock()
	cancel := l.actionCancel
	l.actionCtx = nil
	l.actionCancel = nil
	l.Mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Reexec re-executes the loop on the binary installed over its path at a
// quiet point (no claim in flight). It returns true when the replacement
// binary was executed, so the caller ends the pass on the old binary.
func (l *Loop) Reexec(ctx context.Context) (bool, error) {
	if l == nil || ctx.Err() != nil || l.DryRun || l.Reexecuted || l.reexecFailed {
		return false, nil
	}
	fenced := l.Host != nil && state.WriterFenced(l.Host.Store())
	b := l.Bin
	if b == nil {
		b = LoopBinary()
		l.Mu.Lock()
		l.Bin = b
		l.Mu.Unlock()
	}
	if b == nil || !b.Replaced() {
		if fenced && l.StopWatch != nil {
			l.StopWatch()
			l.StopWatch = nil
		}
		return false, nil
	}
	busy := l.ReexecBusy()
	if busy && !fenced {
		l.SayOnce("reexec-busy", "SUPERVISOR LOOP restart held: claims are in flight; re-executing the installed binary once they finish")
		return false, nil
	}
	wasRefilling := l.StopRefill != nil
	if wasRefilling {
		l.StopRefill()
		l.StopRefill = nil
	}
	if fenced {
		// Once state writes are fenced after binary recovery, in-flight
		// background actions on the failed binary cannot record completions
		// anyway and must not hold re-execution onto the restored binary.
		l.CancelBackgroundActions()
	}
	l.MergeActions.Wait()
	l.PassActions.Wait()
	if _, err := fmt.Fprintln(l.Host.Out(), "SUPERVISOR LOOP its binary was replaced: re-executing it"); err != nil {
		return false, err
	}
	if out, code, err := BeekeeperRun(ctx, l.As(Cmd.BudgetGateArgs()...)...); (err != nil || code != 0) && ctx.Err() == nil {
		detail := Cmd.OneLine(out)
		if detail == "" && err != nil {
			detail = Cmd.OneLine(err.Error())
		}
		l.SayOnce("installed-gate", "SUPERVISOR RELEASE FAILED: installed binary failed budget --gate (exit %d): %s", code, detail)
	}
	l.TimerActs.Wait()
	// Shutdown wins over an update, including cancellation while actions drain.
	if ctx.Err() != nil {
		return false, nil
	}
	wasPullWatching := l.stopPullWatch != nil
	if wasPullWatching {
		l.stopPullWatch()
		l.stopPullWatch = nil
	}
	wasWatching := l.StopWatch != nil
	if wasWatching {
		l.StopWatch()
		l.StopWatch = nil
	}
	// The watch also drains asynchronous work, so shutdown can begin there.
	if ctx.Err() != nil {
		return false, nil
	}
	l.SaveCache()
	if err := b.Exec(); err != nil {
		l.Mu.Lock()
		l.reexecFailed = true
		l.Mu.Unlock()
		l.SayOnce("reexec", "SUPERVISOR LOOP the new binary does not start (%v): carrying on", err)
		if wasRefilling {
			l.StopRefill = l.startRefillCadence(ctx)
		}
		if wasPullWatching {
			l.stopPullWatch = StartLoopPullWatcher(ctx, l)
		}
		if wasWatching {
			l.StopWatch = l.Host.StartLoopWatch(ctx)
		}
		return false, nil
	}
	l.Reexecuted = true
	_ = l.Host.WriteLoopVersion()
	return true, nil
}

// ReloadConfig refreshes l.a.cfg from disk when the configured file changed
// since startup or the last pass, so live config edits and settled overrides
// take effect without restarting the loop process.
func (l *Loop) ReloadConfig() {
	l.RefillConfigMu.Lock()
	defer l.RefillConfigMu.Unlock()
	path := cmp.Or(l.Host.CfgPath(), l.Host.LoadedCfg())
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path) //nolint:gosec // configured path
	if err != nil {
		return
	}
	sum := sha256.Sum256(raw)
	if sum == l.cfgSum {
		return
	}
	c, err := config.Load(path)
	if err != nil {
		return
	}
	if l.Host.LoadedCfg() != "" {
		l.Host.SetCfg(c)
	} else {
		l.Host.Cfg().GitHub.PublicOnlyOwners = slices.Clone(c.GitHub.PublicOnlyOwners)
		l.Host.Cfg().Hooks.Guards = c.Hooks.Guards
		if len(c.GitHub.AllowedOrgs) > 0 {
			l.Host.Cfg().GitHub.AllowedOrgs = c.GitHub.AllowedOrgs
		}
		if len(c.Supervisor.Ownership.Never) > 0 {
			l.Host.Cfg().Supervisor.Ownership.Never = c.Supervisor.Ownership.Never
		}
		if len(c.Supervisor.Ownership.Authors) > 0 {
			l.Host.Cfg().Supervisor.Ownership.Authors = c.Supervisor.Ownership.Authors
		}
		if len(c.Supervisor.Ownership.Bots) > 0 {
			l.Host.Cfg().Supervisor.Ownership.Bots = c.Supervisor.Ownership.Bots
		}
		if len(c.Supervisor.Ownership.Review) > 0 {
			l.Host.Cfg().Supervisor.Ownership.Review = c.Supervisor.Ownership.Review
		}
		if c.Supervisor.Ownership.Team != "" {
			l.Host.Cfg().Supervisor.Ownership.Team = c.Supervisor.Ownership.Team
		}
	}
	github.SetDevctlSkipVersionCheck(!l.Host.Cfg().Hooks.GuardsEnabled())
	l.cfgSum = sum
}

func (l *Loop) unreadRepo(ref string) string {
	r, ok := Cmd.ParseRef(ref)
	if !ok {
		return ""
	}
	_, why := (superloop.Desk{Orgs: l.Host.Cfg().GitHub.AllowedOrgs, Never: l.Host.Cfg().Supervisor.Ownership.Never}).Unread(Cmd.RefName(r))
	return why
}

func (l *Loop) agentUnreadRepo(st *state.State, ag state.Agent, ref string) string {
	if why := l.unreadRepo(ref); why != "" {
		return why
	}
	if ag.Park != nil && ag.Park.On != "" {
		if why := l.unreadRepo(ag.Park.On); why != "" {
			return why
		}
	}
	if st != nil {
		for _, r := range st.Records {
			if r.Session.Is(ag.Party) && r.Issue != "" {
				if why := l.unreadRepo(r.Issue); why != "" {
					return why
				}
			}
		}
	}
	for _, m := range Cmd.NamedRefs(ag.Task+" "+ag.LastTask, l.Host.Cfg().Board.Owner) {
		if why := l.unreadRepo(m); why != "" {
			return why
		}
	}
	for _, pat := range l.Host.Cfg().Supervisor.Ownership.Never {
		if pat = strings.TrimSpace(pat); pat != "" && !strings.ContainsAny(pat, "*?") {
			if strings.Contains(strings.ToLower(ag.Task+" "+ag.LastTask), strings.ToLower(pat)) {
				return pat + " is in supervisor.ownership.never"
			}
		}
	}
	return ""
}

func (l *Loop) CachePull(ref string, p superloop.Pull) {
	if ref == "" || p.State == "" {
		return
	}
	l.Mu.Lock()
	defer l.Mu.Unlock()
	if l.Pulls == nil {
		l.Pulls = map[string]LoopPullObservation{}
	}
	prev := l.Pulls[ref]
	prev.Pull = p
	if prev.At.IsZero() {
		prev.At = l.Host.TimeNow()
	}
	l.Pulls[ref] = prev
	if p.State == superloop.Merged {
		if l.Merged == nil {
			l.Merged = map[string]superloop.Pull{}
		}
		l.Merged[ref] = p
	}
	if p.State != superloop.Open && len(l.OpenPulls) > 0 {
		l.OpenPulls = slices.DeleteFunc(l.OpenPulls, func(r string) bool { return strings.EqualFold(r, ref) })
	}
	if p.State != superloop.Open && l.Unmerged != nil {
		if r, ok := Cmd.ParseRef(ref); ok {
			key := strings.ToLower(r.Repo)
			if obs, exists := l.Unmerged[key]; exists {
				obs.Pulls = slices.DeleteFunc(obs.Pulls, func(u UnmergedPull) bool { return u.Number == r.N })
				obs.At = time.Time{}
				l.Unmerged[key] = obs
			}
		}
	}
}

const LoopCacheFile = "supervisor-loop-cache.json"

type cachedUnmergedObservation struct {
	Pulls  []UnmergedPull `json:"pulls,omitempty"`
	ETag   string         `json:"etag,omitempty"`
	At     time.Time      `json:"at,omitempty"`
	FullAt time.Time      `json:"full_at,omitempty"`
	Error  string         `json:"error,omitempty"`
}

type loopCache struct {
	Login           string                               `json:"login,omitempty"`
	Teams           map[string]string                    `json:"teams,omitempty"`
	TeamsAt         time.Time                            `json:"teams_at,omitempty"`
	TidyAt          time.Time                            `json:"tidy_at,omitempty"`
	OpenPulls       []string                             `json:"open_pulls,omitempty"`
	OpenPullsAt     time.Time                            `json:"open_pulls_at,omitempty"`
	Unmerged        map[string]cachedUnmergedObservation `json:"unmerged,omitempty"`
	UnmergedCursor  string                               `json:"unmerged_cursor,omitempty"`
	DefaultBranches map[string]string                    `json:"default_branches,omitempty"`
	// FlowWaits keeps the pending merge decisions' waits across a restart:
	// a new release must not restart the stall alarm's clock.
	FlowWaits map[string]cachedFlowWait `json:"flow_waits,omitempty"`
}

// cachedFlowWait is a pending merge decision's wait as the cache keeps it,
// with the hold its last run read.
type cachedFlowWait struct {
	Kind    string    `json:"kind"`
	Ref     string    `json:"ref"`
	URL     string    `json:"url,omitempty"`
	Head    string    `json:"head,omitempty"`
	Since   time.Time `json:"since"`
	Alarmed []string  `json:"alarmed,omitempty"`
	Last    time.Time `json:"last,omitzero"`
	Class   string    `json:"class,omitempty"`
	Cause   string    `json:"cause,omitempty"`
	At      time.Time `json:"at,omitzero"`
}

func (l *Loop) LoadCache() {
	if l == nil || l.Host == nil || l.Host.Store() == nil {
		return
	}
	var c loopCache
	if ok, err := l.Host.Store().ReadFile(LoopCacheFile, &c); err != nil || !ok {
		return
	}
	l.Mu.Lock()
	defer l.Mu.Unlock()
	if l.Login == "" {
		l.Login = c.Login
	}
	if len(l.Teams) == 0 && len(c.Teams) > 0 {
		l.Teams = maps.Clone(c.Teams)
		l.TeamsAt = c.TeamsAt
	}
	if l.TidyAt.IsZero() {
		l.TidyAt = c.TidyAt
	}
	if len(l.OpenPulls) == 0 && len(c.OpenPulls) > 0 {
		l.OpenPulls = slices.Clone(c.OpenPulls)
		l.OpenPullsAt = c.OpenPullsAt
	}
	if len(l.Unmerged) == 0 && len(c.Unmerged) > 0 {
		l.Unmerged = make(map[string]LoopUnmergedObservation, len(c.Unmerged))
		for k, v := range c.Unmerged {
			obs := LoopUnmergedObservation{Pulls: slices.Clone(v.Pulls), Etag: v.ETag, At: v.At, FullAt: v.FullAt}
			if v.Error != "" {
				obs.err = errors.New(v.Error)
			}
			l.Unmerged[k] = obs
		}
	}
	if l.UnmergedCursor == "" {
		l.UnmergedCursor = c.UnmergedCursor
	}
	if len(l.DefaultBranches) == 0 && len(c.DefaultBranches) > 0 {
		l.DefaultBranches = maps.Clone(c.DefaultBranches)
	}
	if len(l.FlowWaits) == 0 && len(c.FlowWaits) > 0 {
		l.FlowWaits = make(map[string]flowWait, len(c.FlowWaits))
		if l.MergeNotes == nil {
			l.MergeNotes = map[string]mergeNote{}
		}
		for key, w := range c.FlowWaits {
			if w.Ref == "" || w.Since.IsZero() {
				continue
			}
			l.FlowWaits[key] = flowWait{Act: superloop.Action{Key: key, Kind: w.Kind, Ref: w.Ref, URL: w.URL, Head: w.Head},
				Since: w.Since, alarmed: slices.Clone(w.Alarmed), last: w.Last}
			if w.Cause != "" {
				l.MergeNotes[key] = mergeNote{Class: w.Class, Cause: w.Cause, at: w.At}
			}
		}
	}
}

func (l *Loop) SaveCache() {
	if l == nil || l.DryRun || l.Host == nil || l.Host.Store() == nil {
		return
	}
	l.Mu.Lock()
	c := loopCache{
		Login:           l.Login,
		Teams:           maps.Clone(l.Teams),
		TeamsAt:         l.TeamsAt,
		TidyAt:          l.TidyAt,
		OpenPulls:       slices.Clone(l.OpenPulls),
		OpenPullsAt:     l.OpenPullsAt,
		UnmergedCursor:  l.UnmergedCursor,
		DefaultBranches: maps.Clone(l.DefaultBranches),
	}
	if len(l.Unmerged) > 0 {
		c.Unmerged = make(map[string]cachedUnmergedObservation, len(l.Unmerged))
		for k, v := range l.Unmerged {
			obs := cachedUnmergedObservation{Pulls: slices.Clone(v.Pulls), ETag: v.Etag, At: v.At, FullAt: v.FullAt}
			if v.err != nil {
				obs.Error = v.err.Error()
			}
			c.Unmerged[k] = obs
		}
	}
	if len(l.FlowWaits) > 0 {
		c.FlowWaits = make(map[string]cachedFlowWait, len(l.FlowWaits))
		for key, w := range l.FlowWaits {
			note := l.MergeNotes[key]
			c.FlowWaits[key] = cachedFlowWait{Kind: w.Act.Kind, Ref: w.Act.Ref, URL: w.Act.URL, Head: w.Act.Head,
				Since: w.Since, Alarmed: slices.Clone(w.alarmed), Last: w.last, Class: note.Class, Cause: note.Cause, At: note.at}
		}
	}
	l.Mu.Unlock()
	_ = l.Host.Store().WriteFile(LoopCacheFile, c)
}

// Pass is one look: gather, plan, act.
func (l *Loop) Pass(ctx context.Context) (result error) {
	ctx = context.WithValue(ctx, loopPullContextKey{}, l)
	dispatched := false
	defer func() {
		if !l.DryRun && l.Host.Cfg().Supervisor.WorkerBrief != "" && !dispatched && !l.IndependentFill {
			why := "pass ended before refill dispatch (GitHub observation held or loop stopping)"
			if result != nil {
				why = "pass ended before refill dispatch: " + Cmd.OneLine(result.Error())
			}
			_, _ = fmt.Fprintf(l.Host.Out(), RefillZeroFormat, why)
		}
	}()
	// A pass that ends before it plans runs no merge decision. The stall
	// alarm still measures the pending ones and names what ended the pass:
	// a held or failing pass must not silence it
	// (https://github.com/giantswarm/beekeeper/issues/3820).
	flowChecked := false
	ended := mergeWait{class: waitRestart, cause: "the loop ends its passes before planning while it restarts into its replaced binary, so it runs no merge decision"}
	defer func() {
		if l.DryRun || flowChecked || ctx.Err() != nil {
			return
		}
		if result != nil {
			ended = mergeWait{class: waitPass, cause: "the loop's pass failed before it planned or ran this decision: " + Cmd.OneLine(result.Error())}
		}
		if err := l.flowWaitsUnplanned(ended.class, ended.cause); err != nil {
			l.SayOnce("flow waits", "SUPERVISOR STALL alarm for the pending merge decisions failed: %v", err)
		} else {
			l.ClearSaid("flow waits")
		}
	}()
	l.LifecyclePhase("reload configuration")
	l.ReloadConfig()
	a := l.Host.SnapshotAtNow()
	if !l.IndependentFill {
		l.Mu.Lock()
		l.BoardNone = nil
		l.Mu.Unlock()
	}
	// A release installed over the loop's path runs from here on: an old
	// binary's saves are refused once the new one wrote the state.
	if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
		return err
	}
	if !l.DryRun {
		l.LifecyclePhase("apply session reports")
		if err := a.ConsumeReports(ctx); err != nil {
			return err
		}
	}
	if !l.DryRun {
		l.LifecyclePhase("watch conditions")
		if err := l.WatchConditions(ctx); err != nil {
			l.HoldStarts(true)
			l.SayOnce("watch-conditions", "SUPERVISOR WATCH conditions failed; starts remain held: %v", err)
		} else {
			l.ClearSaid("watch-conditions")
		}
	}
	if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
		return err
	}
	if !l.DryRun {
		l.LifecyclePhase("watch alerts")
		if err := l.watchAlerts(ctx); err != nil {
			l.SayOnce("alerts", "SUPERVISOR ALERTS FAILED: %v", err)
		} else {
			l.ClearSaid("alerts")
		}
	}
	if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
		return err
	}
	readState := a.Store().Read
	if l.DryRun {
		readState = a.Store().Peek
	}
	st, err := readState()
	if err != nil {
		return err
	}
	l.LifecyclePhase("read state and dispatch timers")
	// Wall-clock timer jobs keep running while GitHub observations are
	// held. Board commands they invoke still share the query admission gate.
	if !l.DryRun {
		if err := l.fireTimers(ctx, st); err != nil {
			return err
		}
		if err := l.CheckHostUtilization(st); err != nil {
			l.SayOnce("host utilization", "SUPERVISOR HOST utilization unread: %v", err)
		}
		l.settleOverrides(ctx, st)
		l.ReloadConfig()
		a = l.Host.SnapshotAtNow()
	}
	if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
		return err
	}
	// An unknown or stale reading is resolved by a reading of the loop's
	// own: the watch may sample too rarely or not at all, and a hold that
	// waits on a pass it ends early never recovers (#3724).
	if !l.DryRun && a.Cfg().GitHub.Floor > 0 && loopReadingStale(st.Budget, a.TimeNow()) {
		if err := Cmd.RefreshLoopBudget(a, ctx); err == nil {
			if fresh, err := readState(); err == nil {
				st = fresh
			}
		}
	}
	// A budget hold applies to observations too: reading every parked PR
	// while starts are held still drains the shared GraphQL window.
	if a.Cfg().GitHub.Floor > 0 && LoopBudgetUnknownOrLow(st.Budget, a.Cfg().GitHub.Floor, a.TimeNow()) {
		if !l.DryRun {
			if err := l.ObserveThroughput(superloop.Input{}, nil, false); err != nil {
				return err
			}
		}
		l.SayOnce("graphql budget", "SUPERVISOR GITHUB HELD: GraphQL budget unknown or under github.floor; no board, ownership or PR reads until a fresh reading recovers")
		ended = mergeWait{class: waitBudget, cause: "the loop holds its passes while the GitHub GraphQL budget is unknown or under github.floor: it reads no pull request and runs no merge decision until a fresh reading recovers"}
		return nil
	}
	l.ClearSaid("graphql budget")
	if !l.DryRun {
		if err := a.LoopGitHub(ctx, "pass", func() error { return nil }); err != nil {
			if saveErr := l.ObserveThroughput(superloop.Input{}, nil, false); saveErr != nil {
				return saveErr
			}
			l.SayOnce("graphql allowance", "SUPERVISOR GITHUB HELD: %v", err)
			ended = mergeWait{class: waitBudget, cause: "the loop holds its passes on its GitHub allowance, so it reads no pull request and runs no merge decision: " + cmp.Or(laterCause(err), Cmd.OneLine(err.Error()))}
			return nil
		}
		l.ClearSaid("graphql allowance")
	}
	if !l.DryRun {
		if err := l.CompleteEpics(ctx, st); err != nil {
			l.SayOnce("epic completion", "SUPERVISOR EPIC completion waits: %v", err)
		} else {
			l.ClearSaid("epic completion")
		}
		if err := l.WakeEpicOwners(ctx, st); err != nil {
			l.SayOnce("epic wake", "SUPERVISOR EPIC wake waits: %v", err)
		} else {
			l.ClearSaid("epic wake")
		}
		if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
			return err
		}
		l.LifecyclePhase(loopPhaseTidy)
		l.Tidy(ctx, st)
		if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
			return err
		}
		l.LifecyclePhase(loopPhaseTidy)
		l.UpdateHosts(ctx, st)
		if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
			return err
		}
		l.LifecyclePhase(loopPhaseTidy)
		if err := a.SyncClaims(ctx); err != nil {
			l.SayOnce("claims", "SUPERVISOR LOOP claims on GitHub: %v", err)
		}
		if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
			return err
		}
	}
	l.LifecyclePhase(LoopPhaseRecover)
	if err := l.recoverUnfinishedTurns(ctx, st); err != nil {
		return err
	}
	if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
		return err
	}
	l.LifecyclePhase(LoopPhaseRecover)
	if err := l.RecoverQuota(ctx, st); err != nil {
		return err
	}
	if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
		return err
	}
	l.LifecyclePhase(loopPhaseContinueParks)
	if !l.DryRun {
		st, err = a.Store().Read()
		if err != nil {
			return err
		}
	}
	if err := l.ContinueParks(ctx, st); err != nil {
		return err
	}
	if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
		return err
	}
	l.LifecyclePhase(loopPhaseRecoverParks)
	if !l.DryRun {
		st, err = a.Store().Read()
		if err != nil {
			return err
		}
	}
	if err := l.RecoverParks(ctx, st); err != nil {
		return err
	}
	if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
		return err
	}
	l.LifecyclePhase(loopPhaseRecoverReviews)
	if !l.DryRun {
		st, err = a.Store().Read()
		if err != nil {
			return err
		}
	}
	if err := l.RecoverReviewRequests(ctx, st); err != nil {
		return err
	}
	if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
		return err
	}
	l.LifecyclePhase(loopPhaseEndReviews)
	if !l.DryRun {
		st, err = a.Store().Read()
		if err != nil {
			return err
		}
	}
	if err := l.EndReviewWorkers(ctx, st); err != nil {
		return err
	}
	if !l.DryRun {
		st, err = a.Store().Read()
		if err != nil {
			return err
		}
	}
	if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
		return err
	}
	l.LifecyclePhase(LoopPhaseObserve)
	own := a.Cfg().Supervisor.Ownership
	observationUnknown := false
	in := superloop.Input{Rounds: a.Cfg().Supervisor.ReviewRounds, Person: a.Cfg().Guide.Person, Now: a.TimeNow(), PostMergeWindow: a.Cfg().Supervisor.PostMergeWindow.Duration,
		Desk: superloop.Desk{Since: l.Since, Orgs: a.Cfg().GitHub.AllowedOrgs, Authors: own.Authors, Bots: own.Bots, Team: own.Team,
			Review: own.Review, Never: own.Never, OwnerWait: own.OwnerWait.Duration},
		DevctlOwners: a.Cfg().Merge.DevctlOwners, Admin: a.Cfg().Merge.Admin, MergeMethods: a.Cfg().Merge.Methods,
		BaseMerged:  func(p superloop.Pull, approved string) bool { return l.baseMerged(ctx, p, approved) },
		NitsChecked: func(p superloop.Pull, approved string) bool { return st.ReviewNitsChecked(p.Ref, p.Head, approved) },
		Open:        func(ref string) bool { return l.open(ctx, ref) }, StaleAfter: a.Cfg().Agents.StaleAfter.Duration,
		Board: a.Cfg().Board.Owner != "" && a.Cfg().Board.Project != 0}
	if in.Board {
		in.Filed = l.filedIssues(st)
	}
	for _, ag := range st.Agents {
		w := superloop.Worker{Done: ag.Done, Name: ag.Name, SelfHosted: SelfHostedModel(a.Cfg().Capacity, agentRecordedModel(st, ag)), Started: st.StartIndex(ag.Party) >= 0}
		if ag.Park != nil {
			w.ParkedOn, w.ParkedAt, w.Waits = LoopParkOn(ag.Park), ag.Park.At, ag.Park.Waits
			// A verified park requests review even while retirement waits for
			// the worker's current turn to end.
			w.ReviewRequested = ag.Park.ReviewHead != ""
			w.ParkedHead = ag.Park.ReviewHead
		}
		w.Waking = slices.ContainsFunc(st.Timers, func(t state.Timer) bool { return t.Wake != "" && strings.EqualFold(t.Wake, ag.Name) })
		w.Serves = LoopServes(st, ag)
		for _, record := range st.Records {
			if record.Session.Is(ag.Party) && record.Ended.IsZero() {
				w.Issues = append(w.Issues, record.Issue)
			}
		}
		if len(w.Issues) == 0 && ag.Done {
			if served := LoopTaskServes(ag, w.ParkedOn); served != "" {
				w.Issues = append(w.Issues, served)
			}
		}
		if !ag.Done {
			w.Mentions = Cmd.NamedRefs(ag.Task, a.Cfg().Board.Owner)
		}
		if w.Serves == "" && ag.Park != nil && ag.Park.ReviewHead == "" {
			w.Serves = LoopTaskServes(ag, w.ParkedOn)
		}
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
	l.LifecyclePhase("read event history")
	if !l.DryRun {
		if err := l.CheckpointThroughput(clk.Now().UTC()); err != nil {
			return err
		}
	}
	taken, err := state.FollowSnapshot(a.Store(), "supervisor-loop-taken", "v1", l.eventHistoryFilter(func(e state.Event) bool { return strings.HasPrefix(e.Verb, "supervisor.") }))
	if err != nil {
		return err
	}
	folded := l.takenFold.Fold(taken)
	in.Taken = folded.Taken
	claims, stages, stageDetail := folded.Claims, folded.Stages, folded.stageDetail
	ownerless, failures, stuckFiled := folded.ownerless, folded.failures, folded.stuckFiled
	for _, t := range in.Taken {
		delete(claims, t.Key)
	}
	if in.Taken, err = l.DropInvalidMergeRepairs(in.Taken, stages, stageDetail); err != nil {
		return err
	}
	l.Mu.Lock()
	l.Stages, l.StageDetail = stages, stageDetail
	l.Mu.Unlock()
	since := l.Since
	evs, err := state.FollowSnapshot(a.Store(), "supervisor-loop", "v1:"+l.Since.Format(time.RFC3339Nano), l.eventHistoryFilter(func(e state.Event) bool {
		return slices.Contains(loopEventVerbs, e.Verb) && !e.At.Before(since)
	}))
	if err != nil {
		return err
	}
	for _, e := range evs {
		in.Events = append(in.Events, superloop.Event{At: e.At, Verb: e.Verb, Agent: loopEventAgent(st, e), Detail: e.Detail})
	}
	endedParks := false
	for _, rp := range st.ReviewParks {
		if rp.On == "" || rp.ReviewHead == "" {
			continue
		}
		if ReviewParkEnded(in.Taken, rp) {
			endedParks = true
			continue
		}
		if slices.ContainsFunc(st.Agents, func(ag state.Agent) bool {
			return ag.Park != nil && ag.Park.ReviewHead != "" && strings.EqualFold(LoopParkOn(ag.Park), rp.On)
		}) {
			continue
		}
		in.Workers = append(in.Workers, superloop.Worker{
			Name:            rp.Agent,
			ParkedOn:        rp.On,
			ParkedAt:        rp.At,
			ReviewRequested: true,
			ParkedHead:      rp.ReviewHead,
			SelfHosted:      SelfHostedModel(a.Cfg().Capacity, rp.Model),
			Started:         false,
			Serves:          rp.Serves,
			Saved:           true,
		})
	}
	if endedParks && !l.DryRun {
		_ = a.Store().Update(func(current *state.State) ([]state.Event, error) {
			current.ReviewParks = slices.DeleteFunc(current.ReviewParks, func(rp state.ReviewPark) bool { return ReviewParkEnded(in.Taken, rp) })
			return nil, nil
		})
	}
	// The person's answers to the loop's own notes: an ownership answer is
	// the loop's to act on, no session's.
	answers, err := state.FollowSnapshot(a.Store(), "supervisor-loop-answers", "v1", l.eventHistoryFilter(func(e state.Event) bool { return e.Verb == answerRecorded }))
	if err != nil {
		return err
	}
	for _, e := range answers {
		if an, ok := Cmd.ParseAnswered(e); ok && an.Asker == LoopParty.Name && strings.EqualFold(an.For, a.Cfg().Guide.Person) {
			in.Answers = append(in.Answers, superloop.Answer{Ref: an.About, Note: an.ID, Question: an.Question, Text: an.Answer, By: an.For, At: an.At})
		}
	}
	in.Pulls = map[string]superloop.Pull{}
	in.PullReadAt = map[string]time.Time{}
	tracked := DeliveryTrackedPulls(superloop.Tracked(in.Workers, in.Taken, in.Answers, in.Desk), st.Deliveries, in.Desk)
	tracked = slices.DeleteFunc(tracked, func(ref string) bool {
		return !a.ParkIsPull(ctx, st, ref)
	})
	SortTrackedPulls(in, tracked)
	l.PullWatcher().Set(tracked, in.Desk.Authors, a.Cfg().GitHub.AllowedOrgs, a.Cfg().GitHub.Floor, a.Cfg().Supervisor.Ownership.Never)
	l.PullWatcher().SetBots(in.Desk.Bots, in.Desk, l.Teams, l.Teams != nil)
	if l.Once && l.Host.LoopNow() != nil && !l.DryRun || l.DryRun && l.PullWatcher().needsPoll(tracked, in.Now) {
		PollOncePullWatcher(ctx, l)
	}
	// The sweep reads the desk's open pull requests that nothing tracks:
	// one whose worker ended without a park, or never parked, has no other
	// way into a pass.
	if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
		return err
	}
	l.LifecyclePhase("discover pull requests")
	swept := l.SweepPulls(ctx, in, tracked)
	if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
		return err
	}
	l.LifecyclePhase("observe repository ownership")
	if len(tracked)+len(swept)+len(in.Desk.Bots) > 0 && !l.ownership(ctx, &in.Desk) {
		tracked = nil
	} else {
		tracked = append(tracked, swept...)
	}
	l.PullWatcher().SetBots(in.Desk.Bots, in.Desk, l.Teams, l.Teams != nil)
	unread := map[string]bool{}
	unreviewedEnded := map[string]bool{}
	for _, ref := range tracked {
		if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
			return err
		}
		// This phase only consumes dated observations; the watcher owns IO.
		l.LifecyclePhase("observe pull requests")
		// A merged pull request stays merged: a worker kept for the open
		// work it serves does not have it read again every pass.
		l.Mu.Lock()
		mergedPull, wasMerged := l.Merged[ref]
		l.Mu.Unlock()
		if wasMerged {
			in.Pulls[ref] = mergedPull
			continue
		}
		changed := LoopPullChanged(in, ref)
		step := LoopPullStep
		if slices.Contains(swept, ref) {
			step = LoopSweepStep
		}
		p, err := l.ObservePullStep(ctx, ref, changed, step)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			unread[ref] = true
			observationUnknown = true
			l.SayOnce("read "+ref, "SUPERVISOR LOOP cannot read %s: %v", ref, err)
			continue
		}
		l.ClearSaid("graphql allowance")
		l.ClearSaid("read " + ref)
		if r, ok := Cmd.ParseRef(ref); ok {
			p.Team = l.Teams[strings.ToLower(r.Repo)]
		}
		p.Reviews = DeskVerdicts(p.Reviews, in.Desk.Authors)
		in.Pulls[ref] = p
		l.Mu.Lock()
		in.PullReadAt[ref] = l.Pulls[ref].At
		l.Mu.Unlock()
		if (p.State == superloop.Merged || p.State == superloop.Closed) && !hasLoopRound(in.Taken, ref) {
			unreviewedEnded[strings.ToLower(ref)] = true
		}
		if p.State == superloop.Merged {
			l.Mu.Lock()
			if l.Merged == nil {
				l.Merged = map[string]superloop.Pull{}
			}
			l.Merged[ref] = p
			l.Mu.Unlock()
			if !l.DryRun {
				for _, orig := range p.Absorbs {
					stage := "absorbed " + strings.ToLower(orig)
					if !stages[stage] {
						_ = a.Store().Record(Cmd.About(Cmd.Event(LoopParty, LoopStageVerb, "[%s] %s", stage, ref), orig))
						stages[stage] = true
						stageDetail[stage] = ref
					}
				}
			}
		}
	}
	for stage, by := range stageDetail {
		orig, ok := strings.CutPrefix(stage, "absorbed ")
		if !ok || !stages[stage] || orig == "" || by == "" {
			continue
		}
		mp := in.Pulls[by]
		mp.Ref = cmp.Or(mp.Ref, by)
		mp.State = superloop.Merged
		if !slices.ContainsFunc(mp.Absorbs, func(a string) bool { return strings.EqualFold(a, orig) }) {
			mp.Absorbs = append(mp.Absorbs, orig)
		}
		in.Pulls[by] = mp
	}
	l.Mu.Lock()
	for mref, mp := range l.Merged {
		if _, ok := in.Pulls[mref]; !ok && (len(mp.Absorbs) > 0 || len(superloop.ParseAbsorbedPulls(mref, mp.Title, "")) > 0) {
			in.Pulls[mref] = mp
		}
	}
	l.Mu.Unlock()
	if len(unreviewedEnded) > 0 && !l.DryRun && slices.ContainsFunc(st.ReviewParks, func(rp state.ReviewPark) bool {
		return unreviewedEnded[strings.ToLower(rp.On)]
	}) {
		_ = a.Store().Update(func(current *state.State) ([]state.Event, error) {
			current.ReviewParks = slices.DeleteFunc(current.ReviewParks, func(rp state.ReviewPark) bool {
				return unreviewedEnded[strings.ToLower(rp.On)]
			})
			return nil, nil
		})
	}
	// A merge may have landed before its completion record was saved. The
	// planner sees a merged PR then, so reconcile its pending claim before
	// planning the end and retaining its worker for any post-merge work.
	if !l.DryRun {
		for key, c := range claims {
			tuple, ok := strings.CutPrefix(key, "go ")
			if !ok {
				continue
			}
			ref, _, ok := strings.Cut(tuple, "@")
			head := superloop.KeyHead(key)
			p, present := in.Pulls[ref]
			if !ok || !present {
				continue
			}
			if staleMergeDecision(p, head) && (p.State != superloop.Merged || p.Head != head) {
				why := "merge decision overtaken by " + p.State + " head " + p.Head
				if err := a.Store().Record(Cmd.About(Cmd.Event(LoopParty, LoopClaimEndVerb, "[%s] %s", key, why), ref)); err != nil {
					return err
				}
				delete(claims, key)
				delete(failures, key)
				continue
			}
			if p.State != superloop.Merged || p.Head != head {
				continue
			}
			act := superloop.Action{Key: key, Kind: superloop.KindGo, Ref: ref, URL: p.URL, Head: head, Agent: c.Target}
			if err := l.Record(act, "merged before an interruption; confirmed at "+head); err != nil {
				return err
			}
			in.Taken = append(in.Taken, superloop.Taken{Key: key, Kind: superloop.KindGo, Ref: ref, Agent: c.Target, At: a.TimeNow()})
			delete(claims, key)
		}
	}
	l.LifecyclePhase("observe releases and plan")
	postMerge := superloop.ReleaseWorkers(in)
	l.Mu.Lock()
	l.PostMerge = postMerge
	l.Mu.Unlock()
	if len(postMerge) > 0 || superloop.HasOverlapWait(in.Workers, in.Taken, in.Answers) {
		sessions, _, sessionErr := Cmd.LoopSessions(a)
		for i, ag := range st.Agents {
			if ag.Execution != nil {
				in.Workers[i].LastActive = executionLastActive(st, ag, a.TimeNow())
			} else if sessionErr != nil && !platform.Missing(sessionErr) {
				in.Workers[i].LastActive = a.TimeNow() // never retire on a failed observation
			} else if s, ok := claude.Live(sessions, ag.Party); ok {
				in.Workers[i].LastActive = s.LastActive
			}
		}
	}
	if !l.DryRun && len(postMerge) > 0 {
		for i := len(in.Taken) - 1; i >= 0; i-- {
			if t := in.Taken[i]; t.Kind == superloop.KindRelease {
				version := strings.TrimPrefix(t.Key, "release ")
				if err := l.WakeReleased(ctx, superloop.Action{Key: t.Key, Version: version}); err != nil {
					l.SayOnce("release wake "+version, "SUPERVISOR RELEASE WAKE FAILED %s: %v", version, err)
				}
				break
			}
		}
	}
	if !st.AutoInstallPaused {
		// A release claimed and not finished goes on, whatever --check says
		// now that it is installed.
		for key := range claims {
			if v, ok := strings.CutPrefix(key, superloop.KindRelease+" "); ok {
				in.Release = v
			}
		}
		if in.Release == "" && !l.DryRun && a.TimeNow().Sub(l.releaseAt) >= a.Cfg().Supervisor.ReleaseEvery.Duration {
			l.releaseAt = a.TimeNow()
			in.Release = l.NewerRelease(ctx)
		}
	}
	if reexec, err := l.Reexec(ctx); err != nil || reexec {
		return err
	}
	if l.PauseForReexec() {
		return nil
	}
	if in.Ownerless, err = l.markOwnerless(in, claims, ownerless, unread); err != nil {
		return err
	}
	if !l.DryRun {
		if err := l.reportSweep(in.Ownerless, unread); err != nil {
			return err
		}
	}
	l.LifecyclePhase("plan and record heartbeat")
	in.ClosedIssues = l.closedIssues(in.Now)
	if l.DryRun {
		l.Recording = &LoopRecording{Agents: st.Agents, Deliveries: st.Deliveries}
		in = l.Recording.Capture(in)
	}
	actions := append(superloop.Plan(in), orphanedReviewClaims(claims, in)...)
	if err := ctx.Err(); err != nil {
		return err
	}
	if !l.DryRun {
		l.LifecyclePhase("observe deliveries")
		if err := l.ObserveDeliveries(ctx, in, unread); err != nil {
			return err
		}
		if err := l.SyncDeliveryHolds(ctx); err != nil {
			l.SayOnce("delivery board hold", "DELIVERY BOARD HOLD FAILED: %v", err)
		} else {
			l.ClearSaid("delivery board hold")
		}
	}
	if !l.DryRun {
		l.LifecyclePhase("record planned throughput")
		if err := l.ObserveThroughput(in, actions, observationUnknown); err != nil {
			return err
		}
	}
	if !l.DryRun {
		l.LifecyclePhase("check planner stalls")
		if err := l.Stuck(failures, stuckFiled); err != nil {
			return err
		}
		flowChecked = true
		if err := l.CheckFlowWaits(in, actions, failures); err != nil {
			return err
		}
		if err := l.CheckStall(); err != nil {
			return err
		}
		l.LifecyclePhase("observe repository backlogs")
		if err := l.CheckUnmergedBacklogs(ctx, in); err != nil {
			return err
		}
	}
	if l.DryRun {
		if len(unread) > 0 || l.SweepIncomplete || observationUnknown {
			return fmt.Errorf("dry-run observations incomplete; retry later")
		}
		preview := *st
		preview.Deliveries = superloop.Deliveries(in, st.Deliveries)
		l.Recording.Deliveries = preview.Deliveries
		if len(unread) == 0 && !l.SweepIncomplete {
			preview.DeliveriesAt = in.Now
		}
		l.FillPreview = &preview
		defer func() { l.FillPreview = nil }()
		stale := 0
		for _, act := range actions {
			if l.StaleNext(act) {
				stale++
				continue
			}
			if _, err := fmt.Fprintf(a.Out(), "SUPERVISOR WOULD %s %s\n", strings.ToUpper(act.Kind), superloop.Detail(act, would(act))); err != nil {
				return err
			}
		}
		if stale > 0 {
			if _, err := fmt.Fprintf(a.Out(), "SUPERVISOR WOULD NEXT skip %d stale done events; the refill fills capacity\n", stale); err != nil {
				return err
			}
		}
		return l.Fill(ctx, st, claims, in.Taken)
	}
	if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
		return err
	}
	l.LifecyclePhase("dispatch planned actions")
	dispatched = true
	return l.RunPassActions(ctx, st, claims, failures, in.Taken, actions)
}

func (l *Loop) BeginInFlight(key string) bool {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	if l.inFlight[key] {
		return false
	}
	if l.inFlight == nil {
		l.inFlight = map[string]bool{}
	}
	l.inFlight[key] = true
	return true
}

func (l *Loop) EndInFlight(key string) {
	l.Mu.Lock()
	delete(l.inFlight, key)
	l.Mu.Unlock()
}

func (l *Loop) ExecAction(ctx context.Context, st *state.State, claims map[string]LoopClaim, failures map[string]LoopFailure, act superloop.Action, reserved ...[]string) error {
	if ctx.Err() != nil {
		return nil
	}
	if l.PauseForReexec() {
		l.MergeWaited(act, MergeHeld(waitRestart, "the loop runs no action while it restarts into its replaced binary"))
		return nil
	}
	// An earlier pass can finish after this pass took its snapshot.
	l.Mu.Lock()
	done := l.completed[act.Key]
	if done {
		l.Mu.Unlock()
		return nil
	}

	// Older versions backed quota-only merge refusals off for up to an
	// hour. Fresh budget admission and the gate decide when they can retry.
	quotaRetry := act.Kind == superloop.KindGo && github.RateLimited(errors.New(failures[act.Key].Message))
	if l.Host.TimeNow().Before(failures[act.Key].RetryAt) && !quotaRetry {
		l.Mu.Unlock()
		return nil
	}
	l.Mu.Unlock()
	if c, ok := claims[act.Key]; ok && act.Kind != superloop.KindRelease {
		// Claimed before an interruption: what the log shows of its
		// side effect decides, never a second one.
		what, done, err := l.Reconcile(ctx, st, act, c)
		if act.Kind == superloop.KindGo && errors.Is(err, ErrLoopLater) {
			// An observation the claim's reconciliation cannot read yet is a
			// wait for the next pass, never the end of the loop.
			l.MergeWaited(act, namedWait(err, WaitUnread, "its claim cannot be reconciled, its pull request is unread"))
			return nil
		}
		if err != nil {
			l.MergeWaited(act, MergeHeld(waitFailed, "reconciling its claim failed: %s", Cmd.OneLine(err.Error())))
			return err
		}
		if done {
			return l.completeAction(act, what)
		}
	}
	if act.Kind == superloop.KindQuestion {
		if err := l.readdress(act); err != nil {
			return l.Failed(act, failures, err)
		}
		l.Mu.Lock()
		delete(failures, act.Key)
		l.Mu.Unlock()
		return nil
	}
	what, err := l.act(ctx, st, act, reserved...)
	switch {
	case errors.Is(err, ErrLoopLater):
		l.MergeWaited(act, err)
		return nil
	case err != nil && ctx.Err() != nil:
		// The loop is stopping: its cancellation is no failure of the
		// step, and a recorded one would back the step off across the
		// restart.
		return nil
	case err != nil:
		return l.Failed(act, failures, err)
	}
	if act.Kind == superloop.KindGo && what == LoopWorkerMergeGo {
		l.rememberAction(act)
		return nil
	}
	return l.completeAction(act, what)
}

// RunPassActions runs merges, review starts, intake, worker starts and PR/worker
// transitions independently so a merge waiting on CI or in a lane queue never
// delays a review or worker start, and the reverse.
func (l *Loop) RunPassActions(ctx context.Context, st *state.State, claims map[string]LoopClaim, failures map[string]LoopFailure, taken []superloop.Taken, actions []superloop.Action) (result error) {
	fillLaunched := false
	defer func() {
		if l.Host.Cfg().Supervisor.WorkerBrief != "" && !fillLaunched {
			why := "action dispatch cancelled before refill"
			if result != nil {
				why = "action dispatch failed before refill: " + Cmd.OneLine(result.Error())
			}
			_, _ = fmt.Fprintf(l.Host.Out(), RefillZeroFormat, why)
		}
	}()
	if ctx.Err() != nil {
		return nil
	}
	bgCtx := ctx
	if !l.Once {
		bgCtx = l.BackgroundContext(ctx)
	}
	current, readErr := l.Host.Store().Read()
	if readErr != nil {
		return readErr
	}
	// Freeze reservations before independent streams can register their starts.
	// A capacity read and a roster read are not one atomic observation.
	reservations, _ := l.DeliveryReservations(current)

	planned := map[string]bool{}
	var (
		laneOrder  []string
		laneMerges = map[string][]superloop.Action{}
		reviewActs []superloop.Action
		intakeActs []superloop.Action
		doneActs   []superloop.Action
		nextActs   []superloop.Action
		staleActs  []superloop.Action
		otherActs  []superloop.Action
	)
	for _, act := range actions {
		if act.Kind == superloop.KindFile || act.Kind == superloop.KindDone || act.Kind == superloop.KindProgress {
			planned[act.Agent] = true
		}
		switch act.Kind {
		case superloop.KindGo:
			if !l.BeginInFlight(act.Key) {
				continue
			}
			lane := act.Ref
			if r, ok := Cmd.ParseRef(act.Ref); ok {
				lane = strings.ToLower(r.Repo)
			}
			if _, ok := laneMerges[lane]; !ok {
				laneOrder = append(laneOrder, lane)
			}
			laneMerges[lane] = append(laneMerges[lane], act)
		case superloop.KindReview:
			reviewActs = append(reviewActs, act)
		case superloop.KindIntake:
			intakeActs = append(intakeActs, act)
		case superloop.KindDone:
			doneActs = append(doneActs, act)
		case superloop.KindNext:
			if l.StaleNext(act) {
				staleActs = append(staleActs, act)
				continue
			}
			nextActs = append(nextActs, act)
		default:
			otherActs = append(otherActs, act)
		}
	}

	setErr := func(err error) {
		l.Mu.Lock()
		defer l.Mu.Unlock()
		if err != nil && l.actionErr == nil {
			l.actionErr = err
		}
	}
	// Reserve an ordered stream before launching it. Later passes skip only
	// that stream, while still observing and scheduling unrelated work.
	launch := func(key string, run func() error) {
		if !l.BeginInFlight(key) {
			return
		}
		l.PassActions.Go(func() {
			defer l.EndInFlight(key)
			setErr(run())
		})
	}

	var passMerges sync.WaitGroup
	for _, lane := range laneOrder {
		acts := laneMerges[lane]
		passMerges.Add(1)
		l.mergeQueued(acts)
		l.MergeActions.Go(func() {
			defer passMerges.Done()
			defer func() {
				l.MergeStreamEnded(acts)
				for _, act := range acts {
					l.EndInFlight(act.Key)
				}
			}()
			for i, act := range acts {
				l.MergeStarted(acts, i)
				err := l.ExecAction(bgCtx, st, claims, failures, act)
				if err != nil {
					// The decisions behind it did not run this pass: say why.
					for _, later := range acts[i+1:] {
						l.MergeWaited(later, MergeHeld(waitLane, "the loop's merge stream of its lane ended on %s before it ran: %s", act.URL, Cmd.OneLine(err.Error())))
					}
					setErr(err)
					return
				}
			}
		})
	}

	l.reviewsPlanned(reviewActs)
	for _, act := range reviewActs {
		launch("review:"+act.Ref, func() error {
			err := l.ExecAction(bgCtx, st, claims, failures, act)
			l.reviewTried(act, failures)
			return err
		})
	}

	// Intake must never hold the capacity refill, including follow-ups filed
	// by workers retiring in this pass. Ranked follow-ups enter the next fill.
	if len(intakeActs) > 0 {
		launch("stream:intake", func() error {
			for _, act := range intakeActs {
				if err := l.ExecAction(bgCtx, st, claims, failures, act); err != nil {
					return err
				}
			}
			return nil
		})
	}

	// Retirement can wait on a remote stop or session archive. It must not
	// own the start stream: free capacity exists even when no retirement
	// finishes. Keep starts serialized separately across ticks.
	launch("stream:worker-retirement", func() error {
		for _, act := range doneActs {
			if err := l.ExecAction(bgCtx, st, claims, failures, act); err != nil {
				return err
			}
		}
		return nil
	})

	// The refill alone starts board workers. Legacy done-event next actions
	// are bookkeeping now: neither they nor intake may hold the start stream
	// across passes, or compete with refill for the same capacity snapshot.
	fillLaunched = true
	if l.IndependentFill {
		// The cadence owns refill; observation never dispatches a second one.
	} else if l.BeginInFlight(LoopFillStream) {
		l.PassActions.Go(func() {
			defer l.EndInFlight(LoopFillStream)
			setErr(l.Fill(bgCtx, st, claims, taken, reservations))
		})
	} else if l.Host.Cfg().Supervisor.WorkerBrief != "" {
		_, _ = fmt.Fprintln(l.Host.Out(), "SUPERVISOR REFILL started 0: previous refill still running")
	}
	launch("stream:next", func() error {
		var errs []error
		if err := l.SettleStaleNext(staleActs); err != nil {
			errs = append(errs, err)
		}
		for _, act := range nextActs {
			if l.IsCompleted(act.Key) {
				continue
			}
			if err := l.completeAction(act, "superseded by the capacity refill"); err != nil {
				if ferr := l.nextFailed(act, failures, err); ferr != nil {
					errs = append(errs, ferr)
				}
			}
		}
		return errors.Join(errs...)
	})

	// Keep transitions for one worker ordered, without making a stalled
	// transition hold housekeeping or transitions for another worker.
	transitionOrder := []string{}
	transitions := map[string][]superloop.Action{}
	for _, act := range otherActs {
		key := "transition:" + cmp.Or(act.Agent, act.Ref, act.Kind)
		if act.Kind == superloop.KindEnd {
			if by, ok := strings.CutPrefix(act.Message, "absorbed by "); ok && by != "" {
				key = "transition:" + strings.TrimSpace(by)
			}
		}
		if _, ok := transitions[key]; !ok {
			transitionOrder = append(transitionOrder, key)
		}
		transitions[key] = append(transitions[key], act)
	}
	for _, key := range transitionOrder {
		acts := transitions[key]
		launch(key, func() error {
			for _, act := range acts {
				if err := l.ExecAction(bgCtx, st, claims, failures, act); err != nil {
					return err
				}
			}
			return nil
		})
	}
	launch("stream:roster", func() error {
		return l.Roster(bgCtx, planned)
	})

	if l.Once {
		l.PassActions.Wait()
		passMerges.Wait()
	} else if len(laneOrder) > 0 {
		done := make(chan struct{})
		go func() {
			passMerges.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-clk.After(20 * time.Millisecond):
		}
	}
	l.Mu.Lock()
	defer l.Mu.Unlock()
	err := l.actionErr
	l.actionErr = nil
	return err
}

// baseMerged reports whether p's head only merges its base into approved,
// read once per pair of heads; a read that fails is said once and counts as
// no: the new head gets its review.
func (l *Loop) baseMerged(ctx context.Context, p superloop.Pull, approved string) bool {
	key := p.Ref + " " + approved + " " + p.Head
	l.Mu.Lock()
	if ok, read := l.baseMerges[key]; read {
		l.Mu.Unlock()
		return ok
	}
	l.Mu.Unlock()
	ok, err := ReadBaseMerge(ctx, p, approved)
	if err != nil {
		l.SayOnce("base merge "+key, "SUPERVISOR LOOP cannot tell whether %s's head only merges its base: %v", p.Ref, err)
		return false
	}
	l.Mu.Lock()
	if l.baseMerges == nil {
		l.baseMerges = map[string]bool{}
	}
	l.baseMerges[key] = ok
	l.Mu.Unlock()
	return ok
}

// ownership readies the desk's policy for a pass's reads: the logins the
// desk opens its pull requests as and, once supervisor.ownership.team is
// set, the team files, read again every teamsEvery. False, said once, while
// the login is unknown: the pass reads no pull request, so nothing is
// reviewed on a guess. Team files never read leave the desk's own team's
// pull requests waiting (Desk.TeamsUnknown); ones that cannot be read
// again keep the last ones read.
func (l *Loop) ownership(ctx context.Context, d *superloop.Desk) bool {
	if len(d.Authors) == 0 {
		if l.Login == "" {
			login, err := ReadDeskLogin(ctx)
			if err != nil {
				l.SayOnce("login", "SUPERVISOR LOOP cannot tell the login the desk opens pull requests as (%v): it reads none until it can", err)
				return false
			}
			l.Login = login
		}
		d.Authors = []string{l.Login}
	}
	own := l.Host.Cfg().Supervisor.Ownership
	if own.Team == "" || l.Teams != nil && l.Host.TimeNow().Sub(l.TeamsAt) < teamsEvery {
		return true
	}
	teams, err := ReadTeams(ctx, own)
	switch {
	case err == nil:
		l.Teams, l.TeamsAt = teams, l.Host.TimeNow()
	case l.Teams == nil:
		l.SayOnce("teams", "SUPERVISOR LOOP cannot read the team files %s (%v): until it can, it acts only on supervisor.ownership.review and overridden pull requests", own.TeamFiles, err)
		d.TeamsUnknown = true
	default:
		l.SayOnce("teams "+l.TeamsAt.Format(time.RFC3339), "SUPERVISOR LOOP cannot read the team files %s again (%v): the ones read at %s stand",
			own.TeamFiles, err, l.TeamsAt.Format(time.RFC3339))
	}
	return true
}

// would says what a dry run's act would do.
func would(act superloop.Action) string {
	switch act.Kind {
	case superloop.KindReview:
		return fmt.Sprintf("start reviewer %q for %s at %s, round %d", superloop.ReviewerName(act.Ref, act.Round), act.URL, act.Head, act.Round)
	case superloop.KindOwnership:
		return "ask the guide's person: " + act.Question
	case superloop.KindDone:
		return "take off the roster " + act.Message
	case superloop.KindNext:
		return "start the next board item"
	case superloop.KindRelease:
		return "install " + act.Version
	case superloop.KindClassify:
		return fmt.Sprintf("send its %s to the guide", act.Event.Verb)
	case superloop.KindIntake:
		return fmt.Sprintf("put %s on the board in Backlog and rank it", act.URL)
	case superloop.KindGo:
		return "merge " + act.URL + " at " + act.Head
	case superloop.KindOwner:
		return fmt.Sprintf("give %s an owner (%s)", act.Ref, act.Agent)
	case superloop.KindForward, superloop.KindRework, superloop.KindAnswer, superloop.KindRelay, superloop.KindRed:
		return "tell: " + Cmd.Truncate(Cmd.OneLine(act.Message), 300)
	}
	return Cmd.Truncate(Cmd.OneLine(act.Message), 300)
}

// Record logs act taken, which no pass takes again, and says it.
func (l *Loop) Record(act superloop.Action, what string) error {
	detail := superloop.Detail(act, what)
	kind := act.Kind
	if kind == superloop.KindGo && !strings.HasPrefix(what, LoopWorkerMergeGo) {
		kind = superloop.KindMerge
	}
	if err := l.Host.Store().Record(Cmd.About(Cmd.Event(LoopParty, "supervisor."+kind, "%s", detail), act.Ref)); err != nil {
		return err
	}
	_, err := fmt.Fprintf(l.Host.Out(), "SUPERVISOR %s %s\n", strings.ToUpper(kind), Cmd.EscapedLine(detail))
	return err
}

// completeAction remembers only the final successful outcome, after any
// intermediate records and follow-up side effects have finished.
func (l *Loop) completeAction(act superloop.Action, what string) error {
	if err := l.Record(act, what); err != nil {
		return err
	}
	l.rememberAction(act)
	return nil
}

func (l *Loop) rememberAction(act superloop.Action) {
	l.Mu.Lock()
	if l.completed == nil {
		l.completed = map[string]bool{}
	}
	l.completed[act.Key] = true
	l.Mu.Unlock()
}

func (l *Loop) IsCompleted(key string) bool {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	return l.completed[key]
}

// LoopClaim is a claim read back: when, and what its side effect names
// (the session started, the agent woken or removed, the note's question).
type LoopClaim struct {
	At     time.Time
	Target string
}

// orphanedReviewClaims reports a pending review whose verified park is gone.
// Claims remain in the event log, so the warning must be durable across loop
// restarts too. It does not complete the claim: a later park can still use it.
func orphanedReviewClaims(claims map[string]LoopClaim, in superloop.Input) []superloop.Action {
	keys := make([]string, 0, len(claims))
	for key := range claims {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var out []superloop.Action
	for _, key := range keys {
		pending, ok := strings.CutPrefix(key, "review ")
		if !ok {
			continue
		}
		ref, head, ok := strings.Cut(pending, "@")
		pr, valid := Cmd.ParseRef(ref)
		if !ok || !valid || head == "" {
			continue
		}
		if slices.ContainsFunc(in.Workers, func(w superloop.Worker) bool {
			return w.ReviewRequested && strings.EqualFold(w.ParkedOn, ref)
		}) {
			continue
		}
		warning := "leave " + key + " orphaned"
		if slices.ContainsFunc(in.Taken, func(t superloop.Taken) bool { return t.Key == warning }) {
			continue
		}
		url := fmt.Sprintf("https://github.com/%s/pull/%d", pr.Repo, pr.N)
		out = append(out, superloop.Action{Key: warning, Kind: superloop.KindLeave, Ref: ref, URL: url,
			Message: fmt.Sprintf("%s head %s: orphaned pending review claim; no worker holds a verified review park; claim retained for a new park", url, head)})
	}
	return out
}

// Keyed reads a loop line's detail: "[<key>] <rest>".
func Keyed(detail string) (key, rest string, ok bool) {
	m := keyedLine.FindStringSubmatch(detail)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// keyedLine is a loop line's detail.
var keyedLine = regexp.MustCompile(`(?s)^\[([^\]]+)\] (.*)$`)

// claim logs act's side effect about to happen on target, before it does:
// a restart finds it and reconciles instead of acting twice.
func (l *Loop) claim(act superloop.Action, target string) error {
	return l.Host.Store().Record(Cmd.About(Cmd.Event(LoopParty, LoopClaimVerb, "[%s] %s", act.Key, target), act.Ref))
}

// Reconcile decides a claimed action from the log since its claim: done
// when the command it ran left its line (agents start, agents wake, agents
// remove, note add, all logged as the loop), else not, and it runs.
func (l *Loop) Reconcile(ctx context.Context, st *state.State, act superloop.Action, c LoopClaim) (string, bool, error) {
	// Reconciliation runs in background action streams. Its history reads must
	// not mutate the main pass's lifecycle or keep a stalled pass healthy.
	all, err := state.FollowSnapshot(l.Host.Store(), "supervisor-loop-reconcile", "v1", func(e state.Event) bool {
		return e.By.Name == LoopParty.Name
	})
	if err != nil {
		return "", false, err
	}
	var evs []state.Event
	for _, e := range all {
		if !e.At.Before(c.At) {
			evs = append(evs, e)
		}
	}
	logged := func(verb string, match func(string) bool) bool {
		return slices.ContainsFunc(evs, func(e state.Event) bool { return e.Verb == verb && match(e.Detail) })
	}
	opens := func(d string) bool { return strings.HasPrefix(d, c.Target+":") }
	var done bool
	switch act.Kind {
	case superloop.KindReview, superloop.KindNext:
		done = logged(verbStart, opens)
	case superloop.KindGo:
		l.Mu.Lock()
		repairing := l.Stages[act.Key+" repair"]
		l.Mu.Unlock()
		var p superloop.Pull
		if !repairing {
			var err error
			p, err = l.readPull(ctx, act.Ref)
			if err != nil {
				return "", false, err
			}
			if p.State == superloop.Merged && p.Head == act.Head {
				done = true
				break
			}
		}
		if logged(agentWakeEvent, opens) || logged(continuationQueuedEvent, opens) || logged(verbStart, opens) {
			if !repairing && p.State == superloop.Open && p.Head == act.Head && p.Green && !PullNeedsBranchRepair(p) {
				return fmt.Sprintf("%s before an interruption: %s, as the log shows", LoopWorkerMergeGo, c.Target), true, nil
			}
			done = true
		}
	case superloop.KindForward, superloop.KindRework, superloop.KindAnswer, superloop.KindRelay, superloop.KindRed, superloop.KindOwner:
		done = logged(agentWakeEvent, opens) || logged(continuationQueuedEvent, opens) || logged(verbStart, opens)
	case superloop.KindEnd:
		done = logged(agentWakeEvent, opens) || logged(continuationQueuedEvent, opens) || logged(verbStart, opens) || slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == c.Target && ag.Done }) ||
			!slices.ContainsFunc(st.ReviewParks, func(rp state.ReviewPark) bool { return strings.EqualFold(rp.On, act.Ref) }) && !slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == c.Target })
	case superloop.KindDone:
		done = !slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == c.Target }) ||
			logged("agents.remove", func(d string) bool { return RemovedAgent(d, c.Target) })
	case superloop.KindClassify:
		// An unclassified park's wake claims the agent's name; a note for
		// the guide claims its question.
		if c.Target == act.Agent {
			done = logged(agentWakeEvent, opens) || logged(continuationQueuedEvent, opens)
		} else {
			done = logged("note.add", func(d string) bool { return strings.Contains(d, c.Target) })
		}
	case superloop.KindOwnership:
		done = logged("note.add", func(d string) bool { return strings.Contains(d, c.Target) })
	case superloop.KindIntake:
		for _, e := range evs {
			if e.Verb == "board.move" && opens(e.Detail) {
				moveStage := act.Key + " move"
				l.Mu.Lock()
				l.Stages[moveStage] = true
				if l.StageDetail != nil && l.StageDetail[moveStage] == "" {
					l.StageDetail[moveStage] = e.Detail
				}
				l.Mu.Unlock()
			}
		}
		done = logged("board.rank", opens)
	}
	if !done {
		return "", false, nil
	}
	if act.Kind == superloop.KindReview {
		if err := l.Host.AssignExecutionReview(ctx, act); err != nil {
			return "", false, err
		}
	}
	return fmt.Sprintf("taken before an interruption: %s, as the log shows", c.Target), true, nil
}

// readdress puts a worker's question into the guide's queue and logs it in
// one write.
func (l *Loop) readdress(act superloop.Action) error {
	person := l.Host.Cfg().Guide.Person
	detail := superloop.Detail(act, "re-addressed to the guide's person: "+act.Message)
	err := l.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		i := slices.IndexFunc(st.Notes, func(n state.Note) bool { return n.ID == act.Note })
		if i < 0 {
			return nil, fmt.Errorf("note #%d is closed", act.Note)
		}
		st.Notes[i].For = person
		return []state.Event{Cmd.Event(LoopParty, "supervisor."+act.Kind, "%s", detail)}, nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(l.Host.Out(), "SUPERVISOR %s %s\n", strings.ToUpper(act.Kind), Cmd.EscapedLine(detail))
	return err
}

// LoopFailure persists identical failures across passes and restarts. Two
// consecutive failures defer the action for five minutes, keeping it pending;
// each further identical failure doubles the deferral up to an hour, so a
// refusal that does not change is not retried every five minutes all day.
// Since is when the step began failing this way, on this item or on another
// item still failing the same way; At is the latest attempt.
type LoopFailure struct {
	Message string    `json:"message"`
	Count   int       `json:"count"`
	RetryAt time.Time `json:"retryAt"`
	Kind    string    `json:"kind,omitempty"`
	Ref     string    `json:"ref,omitempty"`
	Since   time.Time `json:"since,omitempty"`
	At      time.Time `json:"at,omitempty"`
}

const (
	loopFailureBackoff    = 5 * time.Minute
	loopFailureBackoffCap = time.Hour
)

// LoopFailureDelay is the deferral after count identical failures.
func LoopFailureDelay(count int) time.Duration {
	d := loopFailureBackoff
	for i := 2; i < count && d < loopFailureBackoffCap; i++ {
		d *= 2
	}
	return min(d, loopFailureBackoffCap)
}

// A step failing the same way for LoopStuckAfter, or on more than one item,
// is stuck: the guide hears of it once (stuck). Failures missing their
// scheduled retry by LoopStuckRecent drop out.
const (
	LoopStuckAfter  = 30 * time.Minute
	LoopStuckRecent = 3 * loopFailureBackoff
	loopStuckKey    = "stuck "
)

// recent keeps an episode alive through its scheduled backoff. A step
// missing its retry for LoopStuckRecent starts a new episode.
func (f LoopFailure) recent(now time.Time) bool {
	at := f.At
	if f.RetryAt.After(at) {
		at = f.RetryAt
	}
	return now.Sub(at) < LoopStuckRecent
}

// Failed logs act's failure and keeps it in failures, by key.
func (l *Loop) Failed(act superloop.Action, failures map[string]LoopFailure, cause error) error {
	now := l.Host.TimeNow()
	l.Mu.Lock()
	previous := failures[act.Key]
	f := LoopFailure{Message: cause.Error(), Count: 1, Kind: act.Kind, Ref: act.Ref, Since: now, At: now}
	// Compare exact messages: a changing timestamp or PID is a different
	// failure and starts a new count, rather than hiding a new cause.
	// An episode expires after its retry has been missed for LoopStuckRecent.
	if f.Message == previous.Message && previous.recent(now) {
		f.Count = previous.Count + 1
		f.Since = cmp.Or(previous.Since, f.Since)
	} else {
		// The same failure of the same step on another item continues
		// that episode rather than starting one.
		for _, o := range l.StuckGroup(failures, f.Kind, f.Message) {
			f.Since = minTime(f.Since, o.Since)
		}
	}
	if f.Count >= 2 {
		f.RetryAt = now.Add(LoopFailureDelay(f.Count))
	}
	failures[act.Key] = f
	l.Mu.Unlock()
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := l.Host.Store().Record(Cmd.About(Cmd.Event(LoopParty, LoopFailureVerb, "[%s] %s", act.Key, b), act.Ref)); err != nil {
		return err
	}
	switch f.Count {
	case 1:
		_, err = fmt.Fprintf(l.Host.Out(), "SUPERVISOR %s FAILED %s: %v\n", strings.ToUpper(act.Kind), act.Key, cause)
	case 2:
		_, err = fmt.Fprintf(l.Host.Out(), "SUPERVISOR BACKOFF %s: identical failures; retry after %s\n", act.Key, f.RetryAt.UTC().Format(time.RFC3339))
	}
	return err
}

// StuckGroup are the failures of kind with message attempted recently, by key.
func (l *Loop) StuckGroup(failures map[string]LoopFailure, kind, message string) map[string]LoopFailure {
	now := l.Host.TimeNow()
	group := map[string]LoopFailure{}
	for key, f := range failures {
		if f.Kind == kind && f.Message == message && f.recent(now) {
			group[key] = f
		}
	}
	return group
}

// Stuck files each step failing the same way for LoopStuckAfter or on more
// than one item, once per episode: the guide's feed says it as GUIDE
// PROBLEM. An episode filed before it began (filed, by step and error) is a
// new one.
func (l *Loop) Stuck(failures map[string]LoopFailure, filed map[string]time.Time) error {
	now := l.Host.TimeNow()
	keys := slices.Sorted(maps.Keys(failures))
	done := map[string]bool{}
	for _, key := range keys {
		f := failures[key]
		sum := sha256.Sum256([]byte(f.Kind + "\x00" + f.Message))
		step := loopStuckKey + f.Kind + " " + hex.EncodeToString(sum[:6])
		if done[step] {
			continue
		}
		done[step] = true
		group := l.StuckGroup(failures, f.Kind, f.Message)
		if len(group) == 0 {
			continue
		}
		since, attempts := now, 0
		var items []string
		for _, k := range slices.Sorted(maps.Keys(group)) {
			g := group[k]
			since = minTime(since, g.Since)
			attempts += g.Count
			items = append(items, cmp.Or(g.Ref, k))
		}
		if len(group) < 2 && now.Sub(since) < LoopStuckAfter {
			continue
		}
		if at, ok := filed[step]; ok && !at.Before(since) {
			continue
		}
		slices.Sort(items)
		items = slices.Compact(items)
		text := fmt.Sprintf("The supervisor loop's %s step has failed the same way since %s, %d attempt(s), blocking %s: %s. "+
			"It retries with backoff from %s up to %s; own the failure to the component its error names.",
			f.Kind, since.UTC().Format(time.RFC3339), attempts, strings.Join(items, ", "), f.Message, loopFailureBackoff, loopFailureBackoffCap)
		line := fmt.Sprintf("%s reported at %s%s%s", LoopParty.Name, now.UTC().Format(time.RFC3339), FileReportSeparator, text)
		// Each episode its own key: guide watch says each one once.
		fileKey := step + " " + since.UTC().Format("20060102T150405Z")
		if err := l.Record(superloop.Action{Kind: superloop.KindFile, Key: fileKey}, line); err != nil {
			return err
		}
		filed[step] = now
	}
	return nil
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// ErrLoopLater is an action that waits for a later pass, quietly: no room
// for a start yet.
var ErrLoopLater = errors.New("later")

// unreadCause is why a read that waits for a later pass has no answer.
func unreadCause(err error) string {
	if errors.Is(err, ErrLoopLater) {
		return cmp.Or(laterCause(err), "no current observation")
	}
	return Cmd.OneLine(err.Error())
}

// SayOnce prints a failure once per key and run.
func (l *Loop) SayOnce(key, format string, args ...any) {
	l.Mu.Lock()
	if l.Said[key] {
		l.Mu.Unlock()
		return
	}
	l.Said[key] = true
	l.Mu.Unlock()
	_, _ = fmt.Fprintf(l.Host.Out(), format+"\n", args...)
}

func (l *Loop) ClearSaid(key string) {
	l.Mu.Lock()
	delete(l.Said, key)
	l.Mu.Unlock()
}

// NewerRelease is a newer release's version, "" when this is the latest or
// the look failed; a failed look is said, once until it succeeds again, so
// the desk does not stay on an old release unnoticed.
func (l *Loop) NewerRelease(ctx context.Context) string {
	out, code, err := BeekeeperRun(ctx, SelfUpdateCheck...)
	switch {
	case err != nil:
		l.SayOnce(releaseLookupKey, "SUPERVISOR RELEASE LOOKUP FAILED: %v", err)
		return ""
	case code != 0 && code != ExitOutdated:
		l.SayOnce(releaseLookupKey, "SUPERVISOR RELEASE LOOKUP FAILED: self-update --check exits %d: %s", code, Cmd.OneLine(out))
		return ""
	}
	delete(l.Said, releaseLookupKey)
	if code != ExitOutdated {
		return ""
	}
	if m := newerLine.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

// Tidy runs board tidy every board.tidyEvery while a board is configured
// and the GitHub budget is above its floor, and says what it did.
func (l *Loop) Tidy(ctx context.Context, st *state.State) {
	a := l.Host
	b := a.Cfg().Board
	if b.Owner == "" || b.Project == 0 || b.TidyEvery.Duration <= 0 || a.TimeNow().Sub(l.TidyAt) < b.TidyEvery.Duration ||
		BoardBudgetLow(st.Budget, a.Cfg().GitHub.Floor, a.TimeNow()) {
		return
	}
	l.TidyAt = a.TimeNow()
	out, err := l.Run(ctx, LoopBoard, BoardTidyName)
	if err != nil {
		if !errors.Is(err, ErrLoopLater) {
			l.SayOnce("board tidy", "SUPERVISOR BOARD TIDY FAILED: %v", err)
		}
		return
	}
	l.ClearSaid("board tidy")
	sayBoard(a, out)
	// The board's blocked items reach the person, once each, and again
	// on their re-check timers while they stay blocked.
	if b.EscalateEvery.Duration < 0 {
		return
	}
	if out, err = l.Run(ctx, LoopBoard, boardEscalateName); err != nil {
		if !errors.Is(err, ErrLoopLater) {
			l.SayOnce("board escalate", "SUPERVISOR BOARD ESCALATE FAILED: %v", err)
		}
		return
	}
	l.ClearSaid("board escalate")
	sayBoard(a, out)
}

// sayBoard prints each line of a board command's output as the loop's.
func sayBoard(a Host, out string) {
	for line := range strings.Lines(out) {
		if line = strings.TrimSpace(line); line != "" {
			_, _ = fmt.Fprintln(a.Out(), "SUPERVISOR BOARD "+line)
		}
	}
}

// BoardTidyName is the board tidy subcommand.
const BoardTidyName = "tidy"

// releaseLookupKey is the said key of a failed look for a newer release.
const releaseLookupKey = "release lookup"

// SelfUpdateCheck is the look for a newer release.
var SelfUpdateCheck = []string{selfUpdateCmdName, "--check"}

// newerLine is self-update --check's line for a newer release.
var newerLine = regexp.MustCompile(`(\S+) is newer than`)

// The subcommands the loop runs that no other constant names.
const (
	LoopCapacity = "capacity"
	LoopRemove   = "remove"
	LoopStop     = bgStopCmd
	LoopRecover  = "recover"
	LoopBoard    = "board"
	LoopTimer    = "timer"
	LoopAdd      = "add"
	LoopNote     = "note"
	// The placement flags a remote start names on its agents start line.
	LoopHostFlag = "--host"
	LoopRepoFlag = "--repo"
	LoopRefFlag  = "--ref"
)

// AsFlag is the global flag that names the party a command acts as.
const AsFlag = "as"

// As is a beekeeper command line the loop runs as itself.
func (l *Loop) As(args ...string) []string {
	out := []string{"--" + AsFlag, LoopParty.Name}
	if l.Host.CfgPath() != "" {
		out = append([]string{"--" + configFlag, l.Host.CfgPath()}, out...)
	}
	return append(out, args...)
}

func isLoopGitHubHeldOutput(line string) bool {
	return strings.Contains(line, "loop GitHub ") && strings.HasSuffix(line, ": "+ErrLoopLater.Error())
}

// Run runs a beekeeper command as the loop; a non-zero exit is an error
// with its output.
func (l *Loop) Run(ctx context.Context, args ...string) (string, error) {
	if len(args) >= 4 && args[0] == agentsName && args[1] == agentStartName && !slices.Contains(args, effortFlag) {
		args = append(args, l.startEffort(args)...)
	}
	out, code, err := BeekeeperRun(ctx, l.As(args...)...)
	if err != nil {
		return out, err
	}
	if code != 0 {
		line := Cmd.OneLine(out)
		if isLoopGitHubHeldOutput(line) {
			return out, fmt.Errorf("beekeeper %s: %s: %w", strings.Join(args[:min(2, len(args))], " "), line, ErrLoopLater)
		}
		var wrapped error
		if strings.Contains(line, state.ErrRecoveryWriterFenced.Error()) {
			wrapped = state.ErrRecoveryWriterFenced
		}
		return out, Cmd.NewExitError(code, fmt.Sprintf("beekeeper %s: exit %d: %s", strings.Join(args[:min(2, len(args))], " "), code, line), wrapped)
	}
	return out, nil
}

// Remove takes name off the roster, saying why in the log. An agent already
// gone is removed: the doctor or a person can take it off between the pass's
// reading and this call, and a step must not fail forever on work done.
func (l *Loop) Remove(ctx context.Context, name, reason string) error {
	_, err := l.Run(ctx, agentsName, LoopRemove, name, "--"+reasonFlagName, reason)
	if err == nil || Cmd.Code(err) != ExitRefused {
		return err
	}
	st, readErr := l.Host.Store().Read()
	if readErr != nil {
		return err
	}
	if slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return strings.EqualFold(ag.Name, name) }) {
		return err
	}
	return nil
}

// act takes one transition and says what it did.
func (l *Loop) act(ctx context.Context, st *state.State, act superloop.Action, reserved ...[]string) (string, error) {
	a := l.Host
	switch act.Kind {
	case superloop.KindReview:
		return l.StartReviewer(ctx, act)
	case superloop.KindGo:
		return l.MergePull(ctx, st, act)
	case superloop.KindForward:
		if err := l.WakeOrStartFixer(ctx, st, act); err != nil {
			return "", err
		}
		return fmt.Sprintf("the review of %s asking for changes on %s forwarded", act.URL, act.Head), nil
	case superloop.KindRed:
		act = AttachRedLogs(ctx, act)
		if err := l.WakeOrStartFixer(ctx, st, act); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s at %s: red checks told: %s", act.URL, act.Head, redChecks(act.Key)), nil
	case superloop.KindRework:
		if err := l.WakeOrStartFixer(ctx, st, act); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s at %s: the review's findings stand with no round left, rework told", act.URL, act.Head), nil
	case superloop.KindAnswer:
		if err := l.WakeOrStartFixer(ctx, st, act); err != nil {
			return "", err
		}
		return fmt.Sprintf("the person's answer about %s told", act.URL), nil
	case superloop.KindRelay:
		if err := l.WakeOrStartFixer(ctx, st, act); err != nil {
			return "", err
		}
		return fmt.Sprintf("the owning team's review of %s told", act.URL), nil
	case superloop.KindOwner:
		return l.ownOverlap(ctx, st, act)
	case superloop.KindOwnership:
		if err := l.toGuide(ctx, act, act.URL, act.Question, act.Message,
			"its owning team's review blocks the desk's work, and only a person overrides the ownership of a pull request; the loop reviews it and gives the merge go as for the desk's own on an answer that overrides it",
			fmt.Sprintf("leave %s to its owning team's review; its worker stays parked", act.URL), act.Options...); err != nil {
			return "", err
		}
		return "to the guide: " + act.Message, nil
	case superloop.KindLeave, superloop.KindOverride:
		return act.Message, nil
	case superloop.KindEnd:
		return l.EndMerged(ctx, st, act)
	case superloop.KindDone:
		i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == act.Agent })
		if i < 0 {
			return "already off the roster", nil
		}
		// A removal or retirement waits for the session's turn to end:
		// its last report is not cut off, and its session can be archived
		// or stopped.
		if Cmd.LoopInTurn(a, st.Agents[i].Party) {
			l.SayOnce("turn "+act.Key, "SUPERVISOR DONE WAITS %s: its session runs a turn", act.Agent)
			return "", ErrLoopLater
		}
		// A managed SSH execution ends with a stop the host acknowledges:
		// removal is refused while its reservation stands. An unreachable
		// host fails the stop; the loop says so once and tries again.
		if ref := st.Agents[i].Execution; ref != nil {
			if r, _ := Cmd.ExecutionRecord(st, *ref); r == nil || !r.Terminal() {
				if _, err := l.Run(ctx, agentsName, LoopStop, act.Agent, "--reason", "supervisor loop: task done"); err != nil {
					return "", err
				}
			}
		}
		if err := l.claim(act, act.Agent); err != nil {
			return "", err
		}
		if err := l.Remove(ctx, act.Agent, cmp.Or(act.Message, "supervisor loop: task done")); err != nil {
			return "", err
		}
		if act.Message != "" {
			return "off the roster: " + act.Message, nil
		}
		return "off the roster", nil
	case superloop.KindNext:
		return l.StartNext(ctx, act, reserved...)
	case superloop.KindRelease:
		return l.Install(ctx, act)
	case superloop.KindClassify:
		return l.Classify(ctx, st, act)
	case superloop.KindProgress:
		return "delivery progress: " + act.Message, nil
	case superloop.KindFile:
		return l.toFile(ctx, st, act)
	case superloop.KindIntake:
		return l.intakeIssue(ctx, st, act)
	}
	return "", fmt.Errorf("no transition for %s", act.Kind)
}

// redChecks are the checks a red wake's key names.
func redChecks(key string) string {
	_, checks, _ := strings.Cut(key, ": ")
	return checks
}

// executionLastActive is the latest activity recorded for ag's managed SSH
// execution, or now when its host is unreachable so a failed observation
// never retires a post-merge worker.
func executionLastActive(st *state.State, ag state.Agent, now time.Time) time.Time {
	if ag.Execution == nil {
		return time.Time{}
	}
	r, _ := Cmd.ExecutionRecord(st, *ag.Execution)
	if r == nil {
		return time.Time{}
	}
	if r.Observation.Reachability == execution.Unreachable {
		return now
	}
	last := r.LastActive
	if r.Observation.LastActive.After(last) {
		last = r.Observation.LastActive
	}
	if r.Published != nil && r.Published.At.After(last) {
		last = r.Published.At
	}
	if !r.Terminal() && r.StopRequest == nil && r.Observation.InTurn && r.Observation.ObservedAt.After(last) {
		last = r.Observation.ObservedAt
	}
	return last
}

// wake tells act's worker its message (agents wake), claimed first.
func (l *Loop) wake(ctx context.Context, act superloop.Action) error {
	if err := l.claim(act, act.Agent); err != nil {
		return err
	}
	_, err := l.Run(ctx, agentsName, agentWakeName, act.Agent, act.Message)
	return err
}

func ShouldEndReviewWorker(st *state.State, ag state.Agent) bool {
	if ag.Park == nil || ag.Park.ReviewHead == "" || LoopParkOn(ag.Park) == "" {
		return false
	}
	if slices.ContainsFunc(st.Timers, func(t state.Timer) bool {
		return t.Fired.IsZero() && t.Wake != "" && (strings.EqualFold(t.Wake, ag.Name) || ag.Is(state.Party{Name: t.Wake, Session: t.Wake, HostSession: t.Wake}))
	}) {
		return false
	}
	k := st.StartIndex(ag.Party)
	if k < 0 {
		return false
	}
	if ag.Execution != nil {
		return st.Starts[k].Execution != nil
	}
	// The watch stops parked background sessions to free memory. Their
	// roster entry and park remain the durable owner of the review and wake.
	if st.Starts[k].Background {
		return false
	}
	return ag.Park.PostMerge != nil || st.Starts[k].Dir != ""
}

func ReviewParkServes(st *state.State, ag state.Agent, on string) string {
	if s := LoopServes(st, ag); s != "" && !strings.EqualFold(s, on) {
		return s
	}
	if ref := AgentRef(st, ag.Name); ref != "" && !superloop.IsPull(ref) && !strings.Contains(strings.ToLower(ref), "/pull/") {
		if r, ok := Cmd.ParseRef(ref); ok && !strings.EqualFold(Cmd.RefName(r), on) {
			return Cmd.RefName(r)
		}
	}
	if rp, ok := st.ReviewParkFor(on); ok && rp.Serves != "" {
		return rp.Serves
	}
	return ""
}

// ReviewParkEnded reports a park whose own pull request the loop already
// ended. A park is per pull request: nothing about its worker ends it, since a
// worker finishing one pull request can have others waiting for review.
func ReviewParkEnded(taken []superloop.Taken, rp state.ReviewPark) bool {
	return slices.ContainsFunc(taken, func(t superloop.Taken) bool {
		return t.Kind == superloop.KindEnd && strings.EqualFold(t.Ref, rp.On) && !rp.At.After(t.At)
	})
}

func hasLoopRound(taken []superloop.Taken, ref string) bool {
	for _, t := range taken {
		if (t.Kind == superloop.KindReview || t.Kind == superloop.KindGo) && strings.EqualFold(t.Ref, ref) {
			return true
		}
	}
	return false
}

func SortTrackedPulls(in superloop.Input, tracked []string) {
	type priority struct {
		tier    int
		changed time.Time
	}
	pri := make(map[string]priority, len(tracked))
	for _, ref := range tracked {
		tier := 3
		for _, w := range in.Workers {
			if !strings.EqualFold(w.ParkedOn, ref) || !w.ReviewRequested {
				continue
			}
			if !w.Saved {
				tier = 0
				break
			}
			tier = min(tier, 1)
		}
		if tier > 1 && hasLoopRound(in.Taken, ref) {
			tier = 2
		}
		pri[ref] = priority{tier: tier, changed: LoopPullChanged(in, ref)}
	}
	slices.SortStableFunc(tracked, func(a, b string) int {
		pa, pb := pri[a], pri[b]
		if pa.tier != pb.tier {
			return cmp.Compare(pa.tier, pb.tier)
		}
		return pb.changed.Compare(pa.changed)
	})
}

func (l *Loop) EndReviewWorkers(ctx context.Context, st *state.State) error {
	for _, ag := range st.Agents {
		if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
			return err
		}
		if ag.Park == nil || ag.Park.ReviewHead == "" {
			continue
		}
		on := LoopParkOn(ag.Park)
		if on == "" || l.unreadRepo(on) != "" {
			continue
		}
		if l.DryRun || !ShouldEndReviewWorker(st, ag) {
			continue
		}
		l.LifecyclePhase(loopPhaseEndReviews)
		if Cmd.LoopInTurn(l.Host, ag.Party) {
			l.SayOnce("turn "+ag.Name, "SUPERVISOR DONE WAITS %s: its session runs a turn", ag.Name)
			continue
		}
		var selector, model string
		worktreeDir := ag.Park.Worktree
		if k := st.StartIndex(ag.Party); k >= 0 {
			model = st.Starts[k].Model
			if st.Starts[k].Execution != nil {
				selector = st.Starts[k].Execution.Selector
			}
			if worktreeDir == "" {
				worktreeDir = st.Starts[k].Dir
			}
		}
		rp := state.ReviewPark{
			On:         on,
			At:         ag.Park.At,
			ReviewHead: ag.Park.ReviewHead,
			Agent:      ag.Name,
			Party:      ag.Party,
			Execution:  ag.Execution,
			Selector:   selector,
			Model:      cmp.Or(ag.Model, model),
			Serves:     ReviewParkServes(st, ag, on),
			PostMerge:  ag.Park.PostMerge,
			Worktree:   worktreeDir,
		}
		if err := l.Host.Store().Update(func(current *state.State) ([]state.Event, error) {
			current.UpsertReviewPark(rp)
			return nil, nil
		}); err != nil {
			return err
		}
		if ref := ag.Execution; ref != nil {
			if r, _ := Cmd.ExecutionRecord(st, *ref); r == nil || !r.Terminal() {
				if _, err := l.Run(ctx, agentsName, LoopStop, ag.Name, "--reason", "supervisor loop: desk review requested"); err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if errors.Is(err, state.ErrRecoveryWriterFenced) {
						return err
					}
					l.SayOnce("stop "+ag.Name, "SUPERVISOR STOP FAILED %s: %v", ag.Name, err)
					continue
				}
				if err := l.Host.Store().Update(func(current *state.State) ([]state.Event, error) {
					if r, _ := Cmd.ExecutionRecord(current, *ref); r != nil {
						r.Observation.StopAcknowledged = true
						r.Observation.ProcessState = execution.Exited
					}
					return nil, nil
				}); err != nil {
					return err
				}
			}
		}
		if err := l.Remove(ctx, ag.Name, "supervisor loop: desk review of "+on+" requested; its park stays saved for the review and the merge"); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, state.ErrRecoveryWriterFenced) {
				return err
			}
			l.SayOnce("remove "+ag.Name, "SUPERVISOR REMOVE FAILED %s: %v", ag.Name, err)
			continue
		}
		if err := l.Host.Store().Update(func(current *state.State) ([]state.Event, error) {
			current.UpsertReviewPark(rp)
			if i := slices.IndexFunc(current.Agents, func(other state.Agent) bool { return other.Is(ag.Party) }); i >= 0 {
				Cmd.RemoveAgent(current, i)
			}
			return nil, nil
		}); err != nil {
			return err
		}
		l.Host.CleanRemovedWorktree(ctx, worktreeDir, ag.Park.ReviewHead)
	}
	return nil
}

// WakeOrStartFixer tells act's worker, or starts one for it. Each wait for a
// later pass names its cause: a merge decision's stall alarm reads it.
func (l *Loop) WakeOrStartFixer(ctx context.Context, st *state.State, act superloop.Action) error {
	if why := l.unreadRepo(act.Ref); why != "" {
		return fmt.Errorf("its repository is unread (%s): %w", why, ErrLoopLater)
	}
	if i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == act.Agent }); i >= 0 {
		if act.Kind != superloop.KindEnd && act.Ref != "" {
			p, err := l.readPull(ctx, act.Ref)
			if err != nil {
				return fmt.Errorf("its pull request is unread (%s): %w", unreadCause(err), ErrLoopLater)
			}
			l.CachePull(act.Ref, p)
			if p.State != superloop.Open {
				return fmt.Errorf("its pull request is %s, not open: %w", cmp.Or(p.State, "in no known state"), ErrLoopLater)
			}
		}
		// A timed-out remote start registers the worker before its first
		// task is accepted. A wake only queues another message; it cannot
		// activate that execution. Recover the original start before telling
		// it the current findings, and keep the action pending on failure.
		if ref := st.Agents[i].Execution; ref != nil {
			if rec, _ := Cmd.ExecutionRecord(st, *ref); rec != nil && Cmd.Unactivated(rec) {
				if _, err := l.remoteStartRefused(ctx, act.Agent); err != nil {
					return err
				}
			}
		}
		return l.wake(ctx, act)
	}
	return l.StartFixer(ctx, st, act)
}

const defaultFixerBrief = `{{if .ServesURL}}Continue {{.ServesURL}} and fix{{else}}Fix{{end}} the findings on {{.URL}} (branch {{.Branch}} at head {{.Head}} in {{.Repo}}).

{{.Task}}

Fetch origin and work on branch {{.Branch}} in a worktree under ~/.local/state/beekeeper/worktrees/{{.Repo}}/{{.Branch}}-<session suffix from your startup prompt> (or in the workspace beekeeper cloned for you when BEEKEEPER_EXECUTION_ID is set). Collect all fixes for the round without running local tests or lints, then push once to {{.Branch}} (or run beekeeper agents publish when BEEKEEPER_EXECUTION_ID is set), immediately hand CI and review to the loop with:
cd "<worktree>" && beekeeper agents park --on {{.Repo}}#{{.Number}}{{if .PostMerge}} --post-merge {{printf "%q" .PostMerge}}{{end}} "desk review"
`

const defaultRepairBrief = `{{if .ServesURL}}Continue {{.ServesURL}} and repair{{else}}Repair{{end}} the branch for {{.URL}} (branch {{.Branch}} at head {{.Head}} in {{.Repo}}).

{{.Task}}

Fetch origin and work on branch {{.Branch}} in a worktree under ~/.local/state/beekeeper/worktrees/{{.Repo}}/{{.Branch}}-<session suffix from your startup prompt> (or in the workspace beekeeper cloned for you when BEEKEEPER_EXECUTION_ID is set). Resolve the conflict or repair the branch without running local tests or lints, then push once to {{.Branch}} (or run beekeeper agents publish when BEEKEEPER_EXECUTION_ID is set), immediately park from the worktree so the loop owns CI:
cd "<worktree>" && beekeeper agents park --on {{.Repo}}#{{.Number}}{{if .PostMerge}} --post-merge {{printf "%q" .PostMerge}}{{end}} "desk review"
If the new head only merges the base branch with the PR patch unchanged, its earlier approval still covers it and the supervisor merges once CI is green without starting a new review. A rebase or a conflict resolution that changes the patch needs a new review.
`

const defaultContinuationBrief = `{{if .ServesURL}}Continue {{.ServesURL}} after {{.URL}}{{if .Head}} ({{.Head}}){{end}} in {{.Repo}}.{{else}}Continue the task for {{.URL}}{{if .Head}} ({{.Head}}){{end}} in {{.Repo}}.{{end}}

{{.Task}}

Fetch origin and work from {{.Branch}} in a worktree under ~/.local/state/beekeeper/worktrees/{{.Repo}} (or in the workspace beekeeper cloned for you when BEEKEEPER_EXECUTION_ID is set). Complete the remaining work and finish with beekeeper agents idle --done --report and --problem.
`

func (l *Loop) StartFixer(ctx context.Context, st *state.State, act superloop.Action) error {
	if why := l.unreadRepo(act.Ref); why != "" {
		return fmt.Errorf("its repository is unread (%s): %w", why, ErrLoopLater)
	}
	if err := l.RepositoryStart(act.Ref); err != nil {
		return err
	}
	if l.HoldingStarts() {
		return fmt.Errorf("starts are held (the SUPERVISOR STARTS HELD line names the watch budget or machine condition): %w", ErrLoopLater)
	}
	r, ok := Cmd.ParseRef(act.Ref)
	if !ok {
		return Cmd.Refused("invalid pull request ref %q", act.Ref)
	}
	var livePull superloop.Pull
	if act.Kind != superloop.KindEnd {
		p, err := l.readPull(ctx, act.Ref)
		if err != nil {
			return fmt.Errorf("its pull request is unread (%s): %w", unreadCause(err), ErrLoopLater)
		}
		l.CachePull(act.Ref, p)
		if p.State != superloop.Open {
			return fmt.Errorf("its pull request is %s, not open: %w", cmp.Or(p.State, "in no known state"), ErrLoopLater)
		}
		livePull = p
	}
	rp, hasRP := st.ReviewParkFor(act.Ref)
	var servesURL, postMerge string
	if hasRP {
		if rp.Serves != "" && !strings.EqualFold(rp.Serves, act.Ref) {
			servesURL = Cmd.IssueURL(rp.Serves)
		}
		if rp.PostMerge != nil {
			postMerge = strings.TrimSpace(*rp.PostMerge)
		}
	}
	name := cmp.Or(act.Agent, fmt.Sprintf("%s %d", RepoName(r.Repo), r.N))
	var place startHost
	var err error
	if act.Kind == superloop.KindEnd {
		// Terminal continuations can need the live desk checkout and host
		// policy sync. Execution hosts only have prepared workspaces, so
		// keep this work on the coordinator even under hostsFirst.
		flags := startFlags(false, false)
		if model := l.Host.RuleModel(name, workerRole); model != "" {
			flags = append(flags, modelFlag, model)
		}
		out, code, readErr := BeekeeperRun(ctx, l.As(append([]string{LoopCapacity}, flags...)...)...)
		var capacity struct {
			Room int `json:"room"`
		}
		if readErr != nil || code != 0 || json.Unmarshal([]byte(out), &capacity) != nil || capacity.Room <= 0 {
			err = ErrLoopLater
		}
	} else {
		place, err = l.startPlacement(ctx, name, r.Repo, false, false)
	}
	if err != nil {
		if errors.Is(err, ErrLoopLater) {
			where := "on any host"
			if act.Kind == superloop.KindEnd {
				where = "on the coordinator"
			}
			l.SayOnce("room "+act.Key, "SUPERVISOR FIXER WAITS %s: no room for a start yet %s", act.URL, where)
			return fmt.Errorf("no room for a worker start yet %s (beekeeper capacity and the SUPERVISOR HOST WAITS lines name the limit): %w", where, ErrLoopLater)
		}
		return err
	}
	tmpl := defaultFixerBrief
	if act.Kind == superloop.KindGo {
		tmpl = defaultRepairBrief
	}
	branch := ""
	head := act.Head
	if act.Kind == superloop.KindEnd {
		tmpl = defaultContinuationBrief
		l.Mu.Lock()
		merged := l.Merged[act.Ref]
		l.Mu.Unlock()
		head = cmp.Or(merged.MergeCommit, act.Head, rp.ReviewHead)
		branch = cmp.Or(merged.Base, head)
	} else {
		branch = livePull.Branch
		if branch == "" {
			branch = fmt.Sprintf("pr-%d", r.N)
		}
		if place.remote() {
			place.Ref = branch
		}
	}
	brief, err := l.Brief("", tmpl, act.Key, BriefData{
		URL:       act.URL,
		Repo:      r.Repo,
		Number:    r.N,
		Head:      head,
		Branch:    branch,
		Worker:    act.Agent,
		Task:      act.Message,
		ServesURL: servesURL,
		PostMerge: postMerge,
	})
	if err != nil {
		return err
	}
	args := []string{agentsName, agentStartName, name, brief}
	if len(l.Host.Cfg().Capacity.Choices) > 0 {
		args = append(args, "--harness", harnessAuto)
	}
	if place.remote() {
		args = append(args, "--host", place.Selector, "--repo", place.Repo, "--ref", place.Ref)
	}
	args = append(args, place.excludeFlags()...)
	// Placement and preparation may block while a repository hold is set.
	if err := l.RepositoryStart(act.Ref); err != nil {
		return err
	}
	if err := l.claim(act, name); err != nil {
		return err
	}
	if _, err := l.RunStart(ctx, act, args...); err != nil {
		if Cmd.Code(err) == ExitRefused && place.remote() {
			if _, rerr := l.remoteStartRefused(ctx, name); rerr == nil {
				return l.Host.OperationOutcome(agentStartName, "", act.URL, true)
			} else if !errors.Is(rerr, ErrLoopLater) {
				return rerr
			}
		}
		if Cmd.Code(err) == ExitRefused {
			return fmt.Errorf("its worker start was refused (%s): %w", Cmd.Truncate(Cmd.OneLine(err.Error()), 300), ErrLoopLater)
		}
		return err
	}
	if hasRP && rp.Serves != "" {
		if err := l.Host.Store().Update(func(current *state.State) ([]state.Event, error) {
			if i := slices.IndexFunc(current.Agents, func(ag state.Agent) bool { return strings.EqualFold(ag.Name, name) }); i >= 0 {
				Cmd.InheritReviewParkOnAgent(current, &current.Agents[i], rp, l.Host.TimeNow().UTC())
			}
			return nil, nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// BriefData is what a brief template renders.
type BriefData struct {
	URL, Repo, Head, Branch, Worker, Task, ServesURL, PostMerge string
	Number, Round                                               int
	Context                                                     string
}

// DefaultReviewBrief is the review brief without supervisor.reviewBrief.
const DefaultReviewBrief = `Review {{.URL}} at head {{.Head}}, review round {{.Round}}, the work of {{.Worker}}.
CI green at {{.Head}}.
{{.Context}}

You are an independent reviewer in a fresh session. Read the shipped beekeeper review skill.
Read the PR, its closing issues and their parent epic, including acceptance lines and comments.
Check fulfilment in full, not just correctness or the minimum that passes CI. Nothing may be
narrowed or deferred without a linked follow-up allowed by the issue. Check behaviour tests,
including failure paths; dead paths; unsupported claims in code, docs and the PR; and changes
that a later epic sub-issue would need to undo. Behaviour the PR adds or changes is documented
in its docs/ topic page, not appended to README.md.

Trust green CI on the exact reviewed head. Do not run local tests, lints, builds or probes.
Inspect the code and tests. For a full review use your own worktree at the pinned head;
for a light review read the diff, issue and CI. Never merge.

Every finding is blocking or a nit to fix in this round; drop anything else. Review only:
never edit, commit or push fixes to the reviewed branch ({{.Branch}}). Post findings and
request changes; report the verdict and review URL with beekeeper agents idle --done --report
so the supervisor forwards them to the implementing worker. Approve only the assigned,
unchanged head when it needs no fixes. Open an issue
only for a real gap outside this PR or an explicitly permitted deferral, label it follow-up,
leave it unranked in the backlog and do not attach it as an epic sub-issue.
An approval's nit-only push is checked by the supervisor against the approved head; do not
start another independent review for it. Other changes require review of the new head.

Post exactly one review comment (gh pr review {{.Number}} --repo {{.Repo}} --comment --body-file <file>).
First line exactly "Desk review (opus): approved" or "Desk review (opus): changes requested",
regardless of your configured model. Second line "Head reviewed: ` + "`{{.Head}}`" + `" (the assigned head only).
Each finding: <file>:<line>: <what is wrong> → <what would be right>.
Never use gh pr review --approve. Report to the supervisor:
beekeeper agents idle --done --report "<the same Desk review (opus): verdict line, a newline,
and the exact review URL>" --problem none (or --problem "<finding, evidence, owning repo>"
for each problem encountered). The supervisor relays findings to the worker and gives the
merge go only after approval. Do not review an unchanged approved head again.
`

// Brief renders the template in file (def when file is empty) with d into
// a file under the state's directory, named after key.
func (l *Loop) Brief(file, def, key string, d BriefData) (string, error) {
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
	if st, err := l.Host.Store().Read(); err != nil {
		return "", err
	} else {
		for _, delivery := range st.Deliveries {
			if delivery.Outcome == state.OutcomeStalled && (Cmd.DeliveryURL(delivery.Ref, false) == d.URL || delivery.Ref == key) {
				fmt.Fprintf(&b, "\nPrevious worker exhausted its continuations. Last evidence: %s\n", delivery.Evidence)
			}
		}
	}
	dir := filepath.Join(l.Host.Store().Dir(), "supervisor-loop")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, briefName.ReplaceAllString(key, "-")+".md")
	return path, os.WriteFile(path, b.Bytes(), 0o600)
}

// briefName is what a key may not keep in a file name.
var briefName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// StartReviewer starts the reviewer of act's head.
func (l *Loop) StartReviewer(ctx context.Context, act superloop.Action) (string, error) {
	if l.unreadRepo(act.Ref) != "" {
		return "", ErrLoopLater
	}
	if err := l.RepositoryStart(act.Ref); err != nil {
		return "", err
	}
	if l.HoldingStarts() {
		return "", ErrLoopLater
	}
	r, _ := Cmd.ParseRef(act.Ref)
	var authorFlags []string
	if l.Host.Cfg().Capacity.TaskPolicies[taskKindReview].DifferentFamily {
		st, err := l.Host.Store().Read()
		if err != nil {
			return "", err
		}
		if m := reviewAuthorModel(st, act); m != "" {
			authorFlags = []string{"--" + authorModelFlag, m}
		} else {
			return "", Cmd.Refused("model policy: review requires the author's recorded model")
		}
	}
	name := superloop.ReviewerName(act.Ref, act.Round)
	placementFlags := slices.Clone(authorFlags)
	if l.Host.Cfg().Supervisor.ReviewModel != "" {
		placementFlags = append(placementFlags, modelFlag, l.Host.Cfg().Supervisor.ReviewModel)
	}
	place, err := l.startPlacement(ctx, name, r.Repo, true, act.SelfHosted, placementFlags...)
	if err != nil {
		if errors.Is(err, ErrLoopLater) {
			l.SayOnce("room "+act.Key, "SUPERVISOR REVIEW WAITS %s: no room for a start yet on any host", act.URL)
			return "", ErrLoopLater
		}
		return "", err
	}
	rawBranch := ""
	l.Mu.Lock()
	if cached, ok := l.Pulls[act.Ref]; ok {
		rawBranch = cached.Pull.Branch
	}
	l.Mu.Unlock()
	if act.Message == "" {
		p, err := l.readPull(ctx, act.Ref)
		if err != nil {
			return "", err
		}
		l.CachePull(act.Ref, p)
		rawBranch = p.Branch
		if p.State != "" && p.State != superloop.Open {
			return "", ErrLoopLater
		}
		if p.Head != act.Head || !p.Green {
			return "", Cmd.Refused("review head moved or CI is not green")
		}
		previous := ""
		for _, review := range p.Reviews {
			if review.Verdict != "" {
				previous = review.Body
			}
		}
		act.Message, err = Cmd.ReviewContext(ctx, p, previous, l.Host.Cfg().Supervisor.ReviewLightLines)
		if err != nil {
			return "", err
		}
	} else if rawBranch == "" {
		if p, err := l.readPull(ctx, act.Ref); err == nil {
			l.CachePull(act.Ref, p)
			rawBranch = p.Branch
		}
	}
	branch := cmp.Or(rawBranch, fmt.Sprintf("pr-%d", r.N))
	brief, err := l.Brief(l.Host.Cfg().Supervisor.ReviewBrief, DefaultReviewBrief, act.Key, BriefData{URL: act.URL, Repo: r.Repo, Number: r.N, Head: act.Head, Branch: branch, Round: act.Round, Worker: act.Agent, Context: act.Message})
	if err != nil {
		return "", err
	}
	if place.remote() {
		place.Ref = branch
	}
	args := []string{agentsName, agentStartName, name, brief, "--review"}
	if l.ReviewDir != "" {
		args = append(args, "--dir", l.ReviewDir)
	}
	if l.Host.Cfg().Supervisor.ReviewModel != "" {
		args = append(args, modelFlag, l.Host.Cfg().Supervisor.ReviewModel)
	}
	args = append(args, authorFlags...)
	if len(l.Host.Cfg().Capacity.Choices) > 0 && l.Host.Cfg().Supervisor.ReviewModel == "" {
		// The reviewer takes capacity's pick, decided as a worker's start
		// is; a self-hosted model's work gets one on no self-hosted provider.
		args = append(args, "--harness", harnessAuto)
		if act.SelfHosted {
			args = append(args, "--"+reviewOfSelfHostedFlag)
		}
	}
	args = append(args, place.remoteStartFlags()...)
	args = append(args, place.excludeFlags()...)
	// Placement and preparation may block while a repository hold is set.
	if err := l.RepositoryStart(act.Ref); err != nil {
		return "", err
	}
	if err := l.Host.ReserveReview(act); err != nil {
		return "", err
	}
	started := false
	defer func() {
		l.Host.FinishReviewRequest(act, started)
	}()
	if err := l.claim(act, name); err != nil {
		return "", err
	}
	if _, err := l.RunStart(ctx, act, args...); err != nil {
		if Cmd.Code(err) == ExitRefused {
			if place.remote() {
				// A refusal of a placed start on a host is either room or
				// an execution its interrupted start left reserved.
				if what, rerr := l.remoteStartRefused(ctx, name); rerr == nil {
					if saveErr := l.Host.OperationOutcome(agentStartName, "", act.URL, true); saveErr != nil {
						return "", saveErr
					}
					return what, nil
				} else if !errors.Is(rerr, ErrLoopLater) {
					return "", rerr
				}
			}
			l.SayOnce("room "+act.Key, "SUPERVISOR REVIEW WAITS %s: no room for a start yet (%v)", act.URL, err)
			return "", ErrLoopLater
		}
		return "", err
	}
	started = true
	if err := l.Host.AssignExecutionReview(ctx, act); err != nil {
		return "", err
	}
	if place.remote() {
		return fmt.Sprintf("reviewer %q started for %s at %s, round %d, on %s", name, act.URL, act.Head, act.Round, place.Selector), nil
	}
	return fmt.Sprintf("reviewer %q started for %s at %s, round %d", name, act.URL, act.Head, act.Round), nil
}

// RepoName is owner/repo's repo.
func RepoName(repo string) string {
	_, name, _ := strings.Cut(repo, "/")
	return name
}

func agentRecordedModel(st *state.State, ag state.Agent) string {
	if ag.Model != "" {
		return ag.Model
	}
	if k := st.StartIndex(ag.Party); k >= 0 {
		return st.Starts[k].Model
	}
	return ""
}

func reviewAuthorModel(st *state.State, act superloop.Action) string {
	for _, ag := range st.Agents {
		if ag.Name == act.Agent {
			if m := agentRecordedModel(st, ag); m != "" {
				return m
			}
		}
	}
	if rp, ok := st.ReviewParkFor(act.Ref); ok {
		if rp.Model != "" {
			return rp.Model
		}
		if k := st.StartIndex(rp.Party); k >= 0 && st.Starts[k].Model != "" {
			return st.Starts[k].Model
		}
	}
	for _, rp := range st.ReviewParks {
		if rp.Agent == act.Agent {
			if rp.Model != "" {
				return rp.Model
			}
			if k := st.StartIndex(rp.Party); k >= 0 && st.Starts[k].Model != "" {
				return st.Starts[k].Model
			}
		}
	}
	return ""
}

// SelfHostedModel reports whether model is a choice of a self-hosted
// provider; an omp agent's recorded provider/id is the choice that names
// its bare id.
func SelfHostedModel(k config.Capacity, model string) bool {
	if model == "" {
		return false
	}
	for _, c := range k.Choices {
		if c.Model == model || c.Harness+"/"+c.Model == model || Cmd.OmpChoiceModel(c, model) {
			if p, ok := k.Providers[c.EffectiveProvider()]; ok && p.SelfHosted() {
				return true
			}
		}
	}
	return false
}

// startHost is one placement of a loop start: local with Selector "", else
// an execution host (agents.hosts) whose repository is mapped and whose
// ref to pin is Ref.
type startHost struct {
	Selector string
	Repo     string
	Ref      string
	// Exclude are the choices a local start leaves to the hosts
	// (supervisor.hostsFirst): those a mapped host can run, which wait for
	// a host with room rather than start here.
	Exclude []string
}

// excludeFlags are what a local start placed past the hosts' choices
// names on its agents start line.
func (h startHost) excludeFlags() []string {
	var args []string
	for _, e := range h.Exclude {
		args = append(args, "--"+excludeFlag, e)
	}
	return args
}

// remote reports whether the start runs on an execution host.
func (h startHost) remote() bool { return h.Selector != "" }

// roomOpen says whether a `capacity --json` answer leaves room for a
// start. An unreadable answer leaves the room open: the start judges it
// again itself, as before.
func roomOpen(out string) bool {
	var v struct {
		Room int `json:"room"`
	}
	if json.Unmarshal([]byte(out), &v) != nil {
		return true
	}
	return v.Room > 0
}

// capacityRoom reads one `capacity [--host <selector>] … --json` verdict:
// whether a start fits there. A run that failed or refused (a host that
// fails its check) has no room.
func (l *Loop) capacityRoom(ctx context.Context, args ...string) bool {
	out, code, err := BeekeeperRun(ctx, l.As(append([]string{LoopCapacity}, args...)...)...)
	return err == nil && code == 0 && roomOpen(out)
}

// startFlags are the capacity flags of a start's kind: review marks a
// review, selfHosted the review of a self-hosted model's work.
func startFlags(review, selfHosted bool) []string {
	args := []string{"--json"}
	if review {
		args = append(args, "--"+reviewFlag)
		if selfHosted {
			args = append(args, "--"+reviewOfSelfHostedFlag)
		}
	}
	return args
}

// PlacesRemotely says whether the loop's starts may be placed on an
// execution host: supervisor.hosts names one and capacity.choices gives a
// remote start its model name. Then the local room alone never stops a
// start; startPlacement judges each host's too.
func (l *Loop) PlacesRemotely() bool {
	return len(l.Host.Cfg().Supervisor.Hosts) > 0 && len(l.Host.Cfg().Capacity.Choices) > 0
}

// startPlacement prefers configured execution hosts by default. If none can
// take the repository and selected task kind, ordinary local capacity is the
// fallback. Explicit hostsFirst: false retains local-first placement.
func (l *Loop) startPlacement(ctx context.Context, name, repo string, review, selfHosted bool, extra ...string) (startHost, error) {
	kind := workerRole
	if review {
		kind = taskKindReview
	}
	model := l.Host.RuleModel(name, kind)
	flags := startFlags(review, selfHosted)
	if model != "" {
		flags = append(flags, modelFlag, model)
	}
	flags = append(flags, extra...)
	// agents review names its checkout, so the reviewer stays on this machine.
	if review && l.ReviewDir != "" {
		if !l.capacityRoom(ctx, flags...) {
			return startHost{}, ErrLoopLater
		}
		return startHost{}, nil
	}
	k := l.Host.Cfg().Supervisor
	if !l.PlacesRemotely() {
		// Nothing to place remotely: the local machine does what it did
		// before, and the start's own gate makes it wait when it has no
		// room. When agents.models selects a model, check its room before
		// claiming the action.
		if (review || model != "") && !l.capacityRoom(ctx, flags...) {
			return startHost{}, ErrLoopLater
		}
		return startHost{}, nil
	}
	out, code, err := BeekeeperRun(ctx, l.As(append([]string{LoopCapacity}, flags...)...)...)
	if (err != nil || code != 0) && !k.HostsFirst {
		return startHost{}, ErrLoopLater
	}
	localReady := err == nil && code == 0 && roomOpen(out)
	if localReady && !k.HostsFirst {
		for _, selector := range k.Hosts {
			l.RecordHostPlacement(selector, repo, "not tried: explicit local-first preference and coordinator has room")
		}
		return startHost{}, nil
	}
	for i, selector := range k.Hosts {
		h, err := l.Host.ExecutionHost(selector)
		if err != nil {
			l.RecordHostPlacement(selector, repo, err.Error())
			l.SayOnce("host "+selector, "SUPERVISOR HOST FAILED %s: %v", selector, err)
			continue
		}
		if !h.AdmitsRepository(repo) {
			l.RecordHostPlacement(selector, repo, "repository is not mapped")
			l.SayOnce("host "+selector+" "+repo, "SUPERVISOR HOST WAITS: %s maps no %s", selector, repo)
			continue
		}
		hostOut, hostCode, hostErr := BeekeeperRun(ctx, l.As(append([]string{LoopCapacity, "--host", selector}, flags...)...)...)
		if reason := HostCapacityReason(hostOut, hostCode, hostErr); reason != "" {
			l.RecordHostPlacement(selector, repo, reason)
			l.SayOnce("host capacity "+selector+" "+reason, "SUPERVISOR HOST WAITS %s: %s", selector, reason)
			continue
		}
		ref, err := l.defaultBranch(ctx, repo)
		if err != nil || ref == "" {
			l.RecordHostPlacement(selector, repo, fmt.Sprintf("default branch unavailable: %v", err))
			l.SayOnce("branch "+repo, "SUPERVISOR HOST WAITS: %s: no default branch of %s to pin a remote start (%v)", selector, repo, err)
			continue
		}
		l.RecordHostPlacement(selector, repo, "selected for start")
		for _, later := range k.Hosts[i+1:] {
			l.RecordHostPlacement(later, repo, "not tried: an earlier host was selected")
		}
		return startHost{Selector: selector, Repo: repo, Ref: ref}, nil
	}
	if !localReady {
		return startHost{}, ErrLoopLater
	}
	return startHost{}, nil
}

// remoteStartFlags are what a placed start names on its agents start line:
// the host, the mapped repository and the ref to pin.
func (h startHost) remoteStartFlags() []string {
	if !h.remote() {
		return nil
	}
	return []string{LoopHostFlag, h.Selector, LoopRepoFlag, h.Repo, LoopRefFlag, h.Ref}
}

// GhDefaultBranch reads a repository's default branch.
var GhDefaultBranch = func(ctx context.Context, repo string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := github.RunGH(ctx, "api", "repos/"+repo, "--jq", ".default_branch")
	return strings.TrimSpace(string(out)), err
}

func (l *Loop) defaultBranch(ctx context.Context, repo string) (string, error) {
	key := strings.ToLower(repo)
	l.Mu.Lock()
	ref := l.DefaultBranches[key]
	l.Mu.Unlock()
	if ref != "" {
		return ref, nil
	}
	ref, err := GhDefaultBranch(ctx, repo)
	if err != nil || ref == "" {
		return ref, err
	}
	l.Mu.Lock()
	if l.DefaultBranches == nil {
		l.DefaultBranches = map[string]string{}
	}
	l.DefaultBranches[key] = ref
	l.Mu.Unlock()
	return ref, nil
}

// remoteStartRefused decides a refused placed start on a host: an
// interruption between the reservation and first-task acceptance leaves
// the managed execution reserved, and every later agents start then refuses
// the name; the loop recovers the recorded execution (agents recover), the
// way a hand start's operator would, and reports its result as the start's.
// A start a host refused for room, with nothing reserved, waits for a
// later pass as before.
func (l *Loop) remoteStartRefused(ctx context.Context, name string) (string, error) {
	st, err := l.Host.Store().Read()
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return strings.EqualFold(ag.Name, name) && ag.Execution != nil })
	if i < 0 {
		return "", ErrLoopLater
	}
	if rec, _ := Cmd.ExecutionRecord(st, *st.Agents[i].Execution); rec != nil && rec.Terminal() {
		return "", ErrLoopLater
	}
	out, err := l.Run(ctx, agentsName, LoopRecover, name)
	if err != nil {
		l.SayOnce("recover "+name, "SUPERVISOR RECOVER FAILED %s: %v", name, err)
		return "", ErrLoopLater
	}
	return fmt.Sprintf("%q recovered the execution its interrupted start reserved: %s", name, Cmd.OneLine(out)), nil
}

// StartNext starts the next ranked item with the worker brief, when
// capacity has room — on an execution host when supervisor.hosts names one
// with room (and the board item's repository is mapped there), else on the
// coordinator's own machine.
func (l *Loop) StartNext(ctx context.Context, act superloop.Action, reserved ...[]string) (string, error) {
	if l.HoldingStarts() || l.Host.RecoveryHeld() != "" {
		return "", ErrLoopLater
	}
	k := l.Host.Cfg().Supervisor
	if k.WorkerBrief == "" {
		return "no supervisor.workerBrief: no worker starts", nil
	}
	if err := l.deliveryPriority(ctx, reserved...); err != nil {
		return "", err
	}
	if !l.PlacesRemotely() {
		// The room check as before: a machine with no room reads no board.
		if _, code, err := BeekeeperRun(ctx, l.As(LoopCapacity)...); err != nil || code != 0 {
			return "", ErrLoopLater
		}
	}
	next, err := l.NextItem(ctx)
	if err != nil {
		return "", err
	}
	if next == nil {
		return "the board offers nothing: no worker starts", nil
	}
	what, err := l.StartItem(ctx, act, *next, reserved...)
	var refused *RefillStartError
	if errors.As(err, &refused) {
		l.DeferFill(next.Ref)
		if strings.Contains(err.Error(), OpenPullRefusal) {
			// The item's delivery is in flight: nothing is left for this
			// done event to start, and the refill takes the next item.
			return fmt.Sprintf("%s %s: its delivery is in flight, no second start", next.URL, OpenPullRefusal), nil
		}
		l.SayOnce(act.Key, "SUPERVISOR NEXT FAILED %s: %v", next.URL, err)
		return "", ErrLoopLater
	}
	return what, err
}

// loopBoardNone is a board next answer without a pick: nothing free, or
// the error that kept the board from answering.
type loopBoardNone struct {
	err        error
	exemptOnly bool
}

// NextItem is the board's first free ranked item, or nil when none is free.
// A walk costs a large share of the GraphQL budget: an empty or failed one
// answers the rest of the pass.
// No walk runs while the GitHub budget is under github.floor: the pass
// answers ErrLoopLater, so a done event waits unsettled and costs nothing.
func (l *Loop) NextItem(ctx context.Context) (*LoopBoardItem, error) {
	st, err := l.Host.Store().Read()
	if err != nil {
		return nil, err
	}
	if l.DryRun && l.FillPreview != nil {
		st = l.FillPreview
	}
	v := Cmd.ReadDeliveryWIP(st, l.Host.Cfg().Delivery.WIPLimit, l.Host.Cfg().Supervisor.LoopInterval.Duration, l.Host.TimeNow(), l.Host.DeliveryPolicy())
	exemptOnly := v.Reason != ""
	if exemptOnly && !l.Host.DeliveryExemptIntake(st) {
		return nil, fmt.Errorf("%s: %w", v.Reason, ErrLoopLater)
	}
	l.Mu.Lock()
	if l.BoardNone != nil && l.BoardNone.exemptOnly == exemptOnly {
		err := l.BoardNone.err
		l.Mu.Unlock()
		return nil, err
	}
	l.Mu.Unlock()
	if BoardBudgetLow(st.Budget, l.Host.Cfg().GitHub.Floor, l.Host.TimeNow()) {
		l.Mu.Lock()
		wait := fmt.Errorf("GitHub budget under github.floor (%d): %w", l.Host.Cfg().GitHub.Floor, ErrLoopLater)
		l.BoardNone = &loopBoardNone{err: wait, exemptOnly: exemptOnly}
		l.Mu.Unlock()
		l.SayOnce("board budget", "SUPERVISOR NEXT HELD: the GitHub budget is under github.floor (%d), no board walk until it recovers", l.Host.Cfg().GitHub.Floor)
		return nil, wait
	}
	l.ClearSaid("board budget")
	next, err := l.readNextItemSkipping(ctx, exemptOnly, l.FillSkipped())
	// A scoped read can never fall back to ordinary work, even if the
	// observation becomes fresh or a delivery ends while the read runs.
	if next != nil && exemptOnly && !Cmd.DeliveryExempt(st, l.Host.TimeNow(), next.Ref, next.WIPExempt) {
		return nil, ErrLoopLater
	}
	if next == nil {
		l.Mu.Lock()
		l.BoardNone = &loopBoardNone{err: err, exemptOnly: exemptOnly}
		l.Mu.Unlock()
	}
	return next, err
}

// BoardBudgetLow reports whether a board walk, which costs a large share of
// the GraphQL budget, must wait: the REST budget is under floor until its
// reset, GraphQL refuses, or the last GraphQL reading is under floor until
// its window resets (its reset, else an hour after the reading).
func BoardBudgetLow(b *state.Budget, floor int, now time.Time) bool {
	if Cmd.LowBudget(b, floor, now) {
		return true
	}
	if b == nil || b.GraphQL == nil {
		return false
	}
	g := b.GraphQL
	if g.Blocks(now) {
		return true
	}
	if g.Remaining >= floor {
		return false
	}
	if !g.Reset.IsZero() {
		return now.Before(g.Reset)
	}
	return now.Sub(g.At) < time.Hour
}

// ReadNextItem reads board next --json. With nothing free board next exits
// 3 after printing its document with no pick: that is the board's answer,
// nothing free, never a failure to retry.
func (l *Loop) ReadNextItem(ctx context.Context, onlyExempt ...bool) (*LoopBoardItem, error) {
	exemptOnly := len(onlyExempt) > 0 && onlyExempt[0]
	return l.readNextItemSkipping(ctx, exemptOnly, nil)
}

func (l *Loop) readNextItemSkipping(ctx context.Context, exemptOnly bool, skip []string) (*LoopBoardItem, error) {
	if !exemptOnly {
		if next, err := l.Host.NextMonitoredIssue(ctx, skip...); next != nil || err != nil {
			return next, err
		}
	}
	args := []string{LoopBoard, DeliveryNextCommand, deliveryJSONFlag}
	for _, ref := range skip {
		args = append(args, "--skip", ref)
	}
	if exemptOnly {
		args = append(args, deliveryExemptOnlyFlag)
	}
	out, code, err := BeekeeperRun(ctx, l.As(args...)...)
	if err != nil {
		return nil, err
	}
	failed := func() error {
		return Cmd.NewExitError(code, fmt.Sprintf("beekeeper %s next: exit %d: %s", LoopBoard, code, Cmd.OneLine(out)), nil)
	}
	if code != 0 && code != ExitRefused {
		if isLoopGitHubHeldOutput(Cmd.OneLine(out)) {
			return nil, ErrLoopLater
		}
		return nil, failed()
	}
	// The output is stdout and then stderr: the document is its first value.
	var next struct {
		Pick      json.RawMessage   `json:"pick"`
		Skipped   []board.Candidate `json:"skipped"`
		AfterPick []board.Candidate `json:"after_pick"`
	}
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&next); err != nil {
		if code != 0 {
			return nil, failed()
		}
		return nil, fmt.Errorf("board next --json: %w", err)
	}
	for _, c := range append(next.Skipped, next.AfterPick...) {
		key := "repository hold " + c.Ref
		if strings.HasPrefix(c.Skip, "held: repository ") || strings.HasPrefix(c.Skip, "held: merge incident ") {
			l.SayOnce(key, "SUPERVISOR NEXT SKIPS %s: %s", c.Ref, c.Skip)
		} else {
			l.ClearSaid(key)
		}
	}
	none := string(next.Pick) == JsonNullLiteral
	if code != 0 {
		// Only a document that says no pick settles a refusal.
		if !none {
			return nil, failed()
		}
		return nil, nil
	}
	if none || len(next.Pick) == 0 {
		return nil, nil
	}
	var pick LoopBoardItem
	if err := json.Unmarshal(next.Pick, &pick); err != nil {
		return nil, fmt.Errorf("board next --json: %w", err)
	}
	if _, ok := Cmd.ParseRef(pick.Ref); !ok {
		return nil, fmt.Errorf("board next: invalid item ref %q", pick.Ref)
	}
	l.ClearSaid("repository hold " + pick.Ref)
	return &pick, nil
}

type LoopBoardItem struct {
	EpicOwner bool   `json:"epic_owner,omitempty"`
	Epic      string `json:"epic,omitempty"`
	WIPExempt bool   `json:"wip_exempt,omitempty"`
	Ref       string `json:"ref"`
	URL       string `json:"url"`
}

func (l *Loop) StartItem(ctx context.Context, act superloop.Action, next LoopBoardItem, reserved ...[]string) (string, error) {
	if err := l.deliveryPriority(ctx, reserved...); err != nil {
		return "", err
	}
	if err := l.epicStart(next); err != nil {
		return "", err
	}
	if err := l.deliveryStart(next.Ref, next.WIPExempt); err != nil {
		return "", err
	}
	if why := l.unreadRepo(next.Ref); why != "" {
		return "", fmt.Errorf("repository unread: %s: %w", why, ErrLoopLater)
	}
	if err := l.Host.AllowMonitoredIssue(ctx, next.Ref); err != nil {
		return "", err
	}
	if err := l.RepositoryStart(next.Ref); err != nil {
		return "", err
	}
	if err := l.UnmergedStart(ctx, next.Ref); err != nil {
		return "", err
	}
	k := l.Host.Cfg().Supervisor
	r, _ := Cmd.ParseRef(next.Ref)
	name := fmt.Sprintf("%s %d", RepoName(r.Repo), r.N)
	if next.EpicOwner {
		name = "Epic owner " + next.Ref
	}
	if err := l.Host.FatalAdmission(name, false); err != nil {
		return "", fmt.Errorf("fatal admission: %v: %w", err, ErrLoopLater)
	}
	place, err := l.startPlacement(ctx, name, r.Repo, false, false)
	if err != nil {
		if errors.Is(err, ErrLoopLater) {
			l.SayOnce("room "+act.Key, "SUPERVISOR NEXT WAITS %s: no room for a start yet on any host", next.URL)
			return "", fmt.Errorf("no room for a start on any host: %w", ErrLoopLater)
		}
		return "", err
	}
	brief, err := l.Brief(k.WorkerBrief, "", "next "+next.Ref, BriefData{URL: next.URL, Repo: r.Repo, Number: r.N})
	if err != nil {
		return "", err
	}
	if next.EpicOwner {
		brief, err = l.Brief("", "Own the epic {{.URL}}. Read its acceptance, comments and plan. Maintain ordered sub-issues and their GitHub blocker dependencies. The loop schedules implementation workers and independent reviewers; do not implement, review or merge. Follow their reports and reviews, raise barriers, and prove acceptance end to end after all children merge. Park on the epic between events; the loop wakes you when children change. Only after acceptance is proven, report idle --done with the exact marker Epic acceptance proven: followed by the acceptance evidence. The loop verifies that no sub-issue remains open, closes the epic and moves it to Done. Never mark an epic done merely because its children merged.", "epic-owner "+next.Ref, BriefData{URL: next.URL, Repo: r.Repo, Number: r.N})
		if err != nil {
			return "", err
		}
	}
	args := []string{agentsName, agentStartName, name, brief}
	if next.EpicOwner {
		args = append(args, "--task", epicOwnerTask+next.Ref)
	}
	if len(l.Host.Cfg().Capacity.Choices) > 0 {
		// The start takes capacity's choice of harness and model, as a
		// reviewer's does.
		args = append(args, "--harness", harnessAuto)
	}
	if place.remote() {
		// A remote start needs its mapped repository and a ref to pin; the
		// host's start judges its own capacity, so --harness auto's pick
		// comes from that host's decision.
		args = append(args, "--host", place.Selector, "--repo", place.Repo, "--ref", place.Ref)
	}
	args = append(args, place.excludeFlags()...)
	// Placement and preparation may block while admission changes.
	if err := l.RepositoryStart(next.Ref); err != nil {
		return "", err
	}
	if err := l.UnmergedStart(ctx, next.Ref); err != nil {
		return "", err
	}
	if err := l.deliveryPriority(ctx, reserved...); err != nil {
		return "", err
	}
	if err := l.epicStart(next); err != nil {
		return "", err
	}
	if err := l.deliveryStart(next.Ref, next.WIPExempt); err != nil {
		return "", err
	}
	if err := l.claim(act, name); err != nil {
		return "", err
	}
	if out, err := l.RunStart(ctx, act, args...); err != nil {
		if Cmd.Code(err) == ExitRefused && place.remote() {
			// A refusal of a placed start on a host is either room or
			// an execution its interrupted start left reserved. The
			// recovery, when it succeeds, is the start taken after all:
			// it is decided before any failure closes the claim, so a
			// recovered execution is reconciled from the claim even
			// when the pass dies before the taken line.
			what, rerr := l.remoteStartRefused(ctx, name)
			if rerr == nil {
				if saveErr := l.Host.OperationOutcome(agentStartName, "", act.URL, true); saveErr != nil {
					return "", saveErr
				}
				l.StallClear()
				return what, nil
			}
			if !errors.Is(rerr, ErrLoopLater) {
				err = rerr
			}
		}
		if strings.HasPrefix(act.Key, LoopFillKey) {
			// A returned failure is settled, rather than an interruption
			// that a later start under the same name could reconcile.
			closed := Cmd.About(Cmd.Event(LoopParty, LoopClaimEndVerb, "[%s] start failed or refused: %v; %s", act.Key, err, Cmd.OneLine(out)), next.Ref)
			if closeErr := l.Host.Store().Record(closed); closeErr != nil {
				return "", fmt.Errorf("closing failed start claim: %w", closeErr)
			}
		}
		if Cmd.Code(err) == ExitRefused {
			return "", &RefillStartError{cause: err, output: Cmd.OneLine(out)}
		}
		return "", err
	}
	if err := l.Host.MonitoredIssueStarted(ctx, next.Ref); err != nil {
		return "", err
	}
	l.StallClear()
	if place.remote() {
		return fmt.Sprintf("%q started on %s on %s at %s", name, next.URL, place.Selector, place.Ref), nil
	}
	return fmt.Sprintf("%q started on %s", name, next.URL), nil
}

// Install takes release act.Version through its stages, each resumed on a
// later pass while the release's claim stands: the install (self-update,
// while --check still finds it newer), the broker on the installed binary,
// supervisor.postInstall (once), then --check finding it the latest.
func (l *Loop) Install(ctx context.Context, act superloop.Action) (string, error) {
	st, err := l.Host.Store().Read()
	if err != nil {
		return "", err
	}
	if st.AutoInstallPaused {
		return "", ErrLoopLater
	}
	if why := l.Host.RecoveryHeld(); why != "" {
		l.SayOnce("recovery-hold", "%s", why)
		return "", ErrLoopLater
	}
	// A candidate under supervision gets its verdict before the next one.
	if exe, err := ExecutablePath(); err == nil && update.WatchedProbation(exe) {
		return "", ErrLoopLater
	}

	version := act.Version
	if err := l.claim(act, version); err != nil {
		return "", err
	}
	_, code, err := BeekeeperRun(ctx, l.As(SelfUpdateCheck...)...)
	if err != nil {
		return "", err
	}
	if code == ExitOutdated {
		if _, err := l.Run(ctx, selfUpdateCmdName); err != nil {
			_ = l.Host.Store().Record(Cmd.Event(LoopParty, LoopClaimEndVerb, "[%s] %v", act.Key, err))
			return "", err
		}
	}
	if out, code, err := BeekeeperRun(ctx, l.As(Cmd.BudgetGateArgs()...)...); err != nil || code != 0 {
		detail := Cmd.OneLine(out)
		if detail == "" && err != nil {
			detail = Cmd.OneLine(err.Error())
		}
		return "", fmt.Errorf("%s installed, but budget --gate exits %d: %s", version, code, detail)
	}
	what := []string{version + " installed"}
	broker, err := Cmd.LoopBroker(ctx)
	if errors.Is(err, ErrLoopLater) {
		l.SayOnce("broker "+act.Key, "SUPERVISOR RELEASE WAITS %s: %s, its restart waits for them", version, broker)
		return "", ErrLoopLater
	}
	if err != nil {
		return "", err
	}
	what = append(what, broker)
	if argv := l.Host.Cfg().Supervisor.PostInstall; len(argv) > 0 {
		stage := act.Key + " post-install"
		l.Mu.Lock()
		doneStage := l.Stages[stage]
		l.Mu.Unlock()
		if !doneStage {
			out, errOut, code, err := LoopCommand(ctx, "", argv)
			if err != nil || code != 0 {
				return "", fmt.Errorf("%s installed, supervisor.postInstall failed (exit %d, %v): %s", version, code, err, Cmd.OneLine(out+errOut))
			}
			if err := l.Host.Store().Record(Cmd.Event(LoopParty, LoopStageVerb, "[%s] ran", stage)); err != nil {
				return "", err
			}
			l.Mu.Lock()
			l.Stages[stage] = true
			l.Mu.Unlock()
		}
		what = append(what, "supervisor.postInstall ran")
	}
	out, code, err := BeekeeperRun(ctx, l.As(SelfUpdateCheck...)...)
	if err != nil || code != 0 {
		return "", fmt.Errorf("%s installed, but self-update --check exits %d: %s", version, code, Cmd.OneLine(out))
	}
	if err := l.WakeReleased(ctx, act); err != nil {
		return "", err
	}
	return strings.Join(append(what, "the latest"), ", "), nil
}

// LoopParkOn preserves explicit wait owners. Older workers sometimes omit
// --on and name the pull request in their wait instead. Only an unambiguous
// pull URL supplies that missing owner; issue URLs and prose do not.
func LoopParkOn(p *state.Park) string {
	if p.On != "" {
		return p.On
	}
	matches := loopParkPull.FindAllStringSubmatch(p.Waits, -1)
	on := ""
	for _, m := range matches {
		ref := m[1] + "#" + m[2]
		if on != "" && on != ref {
			return ""
		}
		on = ref
	}
	return on
}

var loopParkPull = regexp.MustCompile(`https://github\.com/([\w.-]+/[\w.-]+)/pull/([0-9]+)\b`)

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

// Classify routes an event the rules cannot place to the guide, with the
// classifier's class when it is sure enough.
func (l *Loop) Classify(ctx context.Context, st *state.State, act superloop.Action) (string, error) {
	k := l.Host.Cfg().Supervisor
	e := *act.Event
	if e.Verb == parkEvent {
		// Planning can precede a timer add, a replacement park, or idle
		// --done. Never send a corrective wake from that old snapshot.
		current, err := l.Host.Store().Read()
		if err != nil {
			return "", err
		}
		st = current
		i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return strings.EqualFold(ag.Name, e.Agent) })
		if i < 0 || st.Agents[i].Done || st.Agents[i].Park == nil {
			return "park already ended", nil
		}
		ag := st.Agents[i]
		if e.At.Before(ag.Park.At) || LoopParkOn(ag.Park) != "" || !ag.Park.Resumable.IsZero() {
			return "park already replaced or settled", nil
		}
		if slices.ContainsFunc(st.Timers, func(t state.Timer) bool {
			return strings.EqualFold(t.Wake, ag.Name) && t.Fired.IsZero() &&
				(t.WakeParkAt.IsZero() || t.WakeParkAt.Equal(ag.Park.At))
		}) {
			return "park has a pending wake timer", nil
		}
	}
	label := "unclassified"
	why := "beekeeper's supervisor loop has no rule for it and no classifier (supervisor.classifier)"
	if len(k.Classifier) > 0 {
		in, _ := json.Marshal(classifierInput{Verb: e.Verb, Agent: e.Agent, Detail: e.Detail, At: e.At})
		// Its stdout is the answer; its stderr is for the failure.
		out, errOut, code, err := LoopCommand(ctx, string(in), k.Classifier)
		var c classification
		switch {
		case err != nil || code != 0:
			why = fmt.Sprintf("its classifier failed (exit %d, %v): %s", code, err, Cmd.OneLine(errOut))
		case json.Unmarshal([]byte(strings.TrimSpace(out)), &c) != nil || !slices.Contains(loopClasses, c.Class):
			why = "its classifier answered no class: " + Cmd.OneLine(out)
		case c.Confidence < k.ClassifierConfidence:
			why = fmt.Sprintf("its classifier says %s at confidence %.2f, below %.2f", c.Class, c.Confidence, k.ClassifierConfidence)
		default:
			label = c.Class
			why = fmt.Sprintf("its classifier says %s (confidence %.2f), which only a person or the guide resolves", c.Class, c.Confidence)
		}
	}
	// A park the loop cannot place is the worker's to classify: it is told
	// how to park on a wait the loop watches, and nothing goes to the
	// person, who could weigh no option of it.
	if label == "unclassified" && e.Verb == parkEvent {
		for _, d := range st.Deliveries {
			if !d.Active() {
				continue
			}
			for _, ag := range st.Agents {
				if strings.EqualFold(ag.Name, e.Agent) && Cmd.DeliveryAgent(st, d, ag) && Cmd.ClassifyDelivery(st, d, l.Host.TimeNow(), l.Host.DeliveryPolicy()).Class == deliveryBlocked {
					return fmt.Sprintf("%s remains blocked on an external delivery wait", e.Agent), nil
				}
			}
		}
		act.Message = unclassifiedParkMessage(e)
		if err := l.wake(ctx, act); err != nil {
			return "", err
		}
		return fmt.Sprintf("woke %s to park on a wait the loop watches (%s): no note for the person", e.Agent, why), nil
	}
	ref := AgentRef(st, e.Agent)
	if ref == "" {
		return fmt.Sprintf("dropped %s from %s: names no issue or pull request to file the guide's note on; %s", e.Verb, e.Agent, e.Detail), nil
	}
	q := fmt.Sprintf("Decide what %s does next about its %s (%s)?", e.Agent, strings.TrimPrefix(e.Verb, "agents."), label)
	if err := l.toGuide(ctx, act, ref, q, fmt.Sprintf("%s logged %s at %s: %s", e.Agent, e.Verb, e.At.UTC().Format(time.RFC3339), e.Detail), why,
		fmt.Sprintf("leave %s as it is: it keeps its task", e.Agent)); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s to the guide (%s) on %s", e.Verb, label, ref), nil
}

// unclassifiedParkMessage tells the worker whose park e the loop cannot
// place how to park so that it can.
func unclassifiedParkMessage(e superloop.Event) string {
	return fmt.Sprintf("Your park at %s (%q) names no wait the supervisor loop watches, so nothing would ever resume you. "+
		"If your task is finished, report it: agents idle --done --report and --problem. Otherwise park again on what you wait for: "+
		"a pull request (agents park --on <owner/repo#n>), a time or an external state (timer add --wake <you>, then agents park), "+
		"or a person's decision (note add --for <person> with its status quo, why, options and default, then agents park --on '#<note>').",
		e.At.UTC().Format(time.RFC3339), Cmd.Truncate(Cmd.OneLine(e.Detail), 300))
}

// FileReportSeparator identifies reports stored in the event itself. Older
// file transitions only summarized delivery to a note and must not be replayed.
const FileReportSeparator = "; file in the owning repositories:\n"

// toFile leaves the full finding in the settled transition's event. The
// guide's feed reads that event once; no decision or thread is needed.
func (l *Loop) toFile(_ context.Context, st *state.State, act superloop.Action) (string, error) {
	e := *act.Event
	from := ""
	if ref := AgentRef(st, e.Agent); ref != "" && !strings.Contains(act.Message, ref) {
		from = " (from " + ref + ")"
	}
	return fmt.Sprintf("%s reported at %s%s%s%s", e.Agent, e.At.UTC().Format(time.RFC3339), from, FileReportSeparator, act.Message), nil
}

func (l *Loop) ownOverlap(ctx context.Context, st *state.State, act superloop.Action) (string, error) {
	if l.unreadRepo(act.Ref) != "" {
		return "", ErrLoopLater
	}
	blockStage := act.Key + " blocked"
	l.Mu.Lock()
	blocked := l.Stages[blockStage]
	l.Mu.Unlock()
	if act.OwnerIssue != "" && act.BlockIssue != "" && !blocked && l.Host.Cfg().Board.Owner != "" && l.Host.Cfg().Board.Project != 0 {
		if _, err := l.Run(ctx, LoopBoard, "unblock", act.OwnerIssue, "--by", act.BlockIssue); err != nil {
			return "", fmt.Errorf("remove inverse overlap dependency: %w", err)
		}
		if _, err := l.Run(ctx, LoopBoard, "block", act.BlockIssue, "--by", act.OwnerIssue); err != nil {
			return "", fmt.Errorf("record overlap dependency: %w", err)
		}
		if err := l.Host.Store().Record(Cmd.About(Cmd.Event(LoopParty, LoopStageVerb, "[%s] %s blocked by %s", blockStage, act.Block, act.Ref), act.Ref)); err != nil {
			return "", err
		}
		l.Mu.Lock()
		l.Stages[blockStage] = true
		l.Mu.Unlock()
	}
	if err := l.WakeOrStartFixer(ctx, st, act); err != nil {
		return "", err
	}
	if act.BlockIssue != "" {
		return fmt.Sprintf("owner %q assigned to %s (%s blocked by %s)", act.Agent, cmp.Or(act.URL, act.Ref), act.Block, act.Ref), nil
	}
	return fmt.Sprintf("owner %q assigned to %s", act.Agent, cmp.Or(act.URL, act.Ref)), nil
}

// intakeIssue puts act.Ref onto the configured board in Backlog, unless it
// already sits on another board where it is being worked, and ranks it on the
// board right after the epic or item it blocks or follows from.
func (l *Loop) intakeIssue(ctx context.Context, st *state.State, act superloop.Action) (string, error) {
	if BoardBudgetLow(st.Budget, l.Host.Cfg().GitHub.Floor, l.Host.TimeNow()) {
		return "", ErrLoopLater
	}
	moveStage := act.Key + " move"
	l.Mu.Lock()
	moveLine := l.StageDetail[moveStage]
	moved := l.Stages[moveStage]
	l.Mu.Unlock()
	if !moved {
		if err := l.claim(act, act.Ref); err != nil {
			return "", err
		}
		out, err := l.Run(ctx, LoopBoard, "move", act.Ref, "backlog", "--intake")
		if err != nil {
			if Cmd.Code(err) == ExitRefused && strings.Contains(err.Error(), " is no issue:") {
				return fmt.Sprintf("dropped intake of %s: not an issue (%s)", act.Ref, Cmd.OneLine(out)), nil
			}
			return "", err
		}
		l.Mu.Lock()
		l.BoardNone = nil
		l.FillPause = FillPause{}
		l.Mu.Unlock()
		moveLine = Cmd.OneLine(out)
		if strings.HasPrefix(moveLine, "left ") {
			return moveLine, nil
		}
		if err := l.Host.Store().Record(Cmd.Event(LoopParty, LoopStageVerb, "[%s] %s", moveStage, moveLine)); err != nil {
			return "", err
		}
		l.Mu.Lock()
		l.Stages[moveStage] = true
		if l.StageDetail != nil {
			l.StageDetail[moveStage] = moveLine
		}
		l.Mu.Unlock()
	}
	from := cmp.Or(act.Message, AgentRef(st, act.Agent))
	anchor, err := Cmd.ReadIntakeAnchor(ctx, l.Host, act.Ref, from)
	if err != nil {
		return "", err
	}
	if anchor == "" && from != "" && !superloop.IsPull(from) {
		if r, ok := Cmd.ParseRef(from); ok && !strings.EqualFold(Cmd.RefName(r), act.Ref) {
			anchor = Cmd.RefName(r)
		}
	}
	if anchor != "" && !strings.EqualFold(anchor, act.Ref) {
		rankOut, err := l.Run(ctx, LoopBoard, "rank", act.Ref, "--after", anchor)
		if err != nil {
			return "", err
		}
		if moveLine == "" {
			return Cmd.OneLine(rankOut), nil
		}
		return fmt.Sprintf("%s; %s", moveLine, Cmd.OneLine(rankOut)), nil
	}
	return moveLine, nil
}

// AgentRef is the issue or pull request the agent works: what it is
// parked on, else its serve record, else the first one its task names.
func AgentRef(st *state.State, name string) string {
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
			if m := Cmd.RefURL().FindString(task); m != "" {
				return m
			}
		}
		for i := len(st.WorkerReports) - 1; i >= 0; i-- {
			if r := st.WorkerReports[i]; r.By.Is(ag.Party) || r.By.Name == name {
				if m := Cmd.RefURL().FindString(r.Task + " " + r.Text); m != "" {
					return m
				}
			}
		}
	}
	for i := len(st.WorkerReports) - 1; i >= 0; i-- {
		if r := st.WorkerReports[i]; r.By.Name == name {
			if m := Cmd.RefURL().FindString(r.Task + " " + r.Text); m != "" {
				return m
			}
		}
	}
	return ""
}

// toGuide files a decision for the guide's person on ref (note add).
func (l *Loop) toGuide(ctx context.Context, act superloop.Action, ref, question, statusQuo, why, dflt string, options ...string) error {
	person := l.Host.Cfg().Guide.Person
	if person == "" {
		return errors.New("guide.person is unset: no guide's queue to file it in")
	}
	question = Cmd.Truncate(question, 150)
	if err := l.claim(act, question); err != nil {
		return err
	}
	args := []string{LoopNote, LoopAdd, "--for", person}
	if ref != "" {
		args = append(args, "--ref", ref)
	}
	args = append(args, "--due", l.Host.Cfg().Supervisor.NoteDue.String(),
		"--status-quo", Cmd.Truncate(statusQuo, 2900), "--why", why, "--default", dflt,
		"--checked", "beekeeper supervisor loop: dated pull observations and the event log")
	for _, o := range options {
		args = append(args, "--option", o)
	}
	_, err := l.Run(ctx, append(args, question)...)
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

// LoopReleaseContains keeps self-update wakes scoped to this program's repository.
// Delivery release recovery uses the general containment check independently.
var LoopReleaseContains = func(ctx context.Context, ref, commit, version string) (bool, error) {
	r, ok := Cmd.ParseRef(ref)
	if !ok || r.Repo != "giantswarm/beekeeper" {
		return false, nil
	}
	return ReleaseTagContains(ctx, ref, commit, version)
}

// ReleaseTagContains proves the actual merge is an ancestor of the tag using REST.
func ReleaseTagContains(ctx context.Context, ref, commit, version string) (bool, error) {
	r, ok := Cmd.ParseRef(ref)
	if !ok || commit == "" {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := github.RunGH(ctx, "api", "repos/"+r.Repo+"/compare/"+commit+"..."+version, "--jq", ".status")
	if err != nil {
		return false, err
	}
	status := strings.TrimSpace(string(out))
	return status == "ahead" || status == "identical", nil
}

func (l *Loop) WakeReleased(ctx context.Context, release superloop.Action) error {
	l.Mu.Lock()
	workers := slices.Clone(l.PostMerge)
	l.Mu.Unlock()
	for _, w := range workers {
		stage := "post-merge wake " + w.Ref + "@" + w.Head + " " + w.Agent
		checked := release.Key + " excludes " + w.Ref + "@" + w.Head
		l.Mu.Lock()
		skip := w.Head == "" || l.Stages[stage] || l.Stages[checked]
		l.Mu.Unlock()
		if skip {
			continue
		}
		contains, err := LoopReleaseContains(ctx, w.Ref, w.Head, release.Version)
		if err != nil {
			return err
		}
		if !contains {
			if err := l.Host.Store().Record(Cmd.Event(LoopParty, LoopStageVerb, "[%s] merge absent from installed release", checked)); err != nil {
				return err
			}
			l.Mu.Lock()
			l.Stages[checked] = true
			l.Mu.Unlock()
			continue
		}
		w.Key = stage
		w.Message = fmt.Sprintf("Release %s is installed and contains the merge of %s (%s). Complete the live proof and board update, then report with agents idle --done --report and --problem.", release.Version, w.URL, w.Head)
		if err := l.wake(ctx, w); err != nil {
			return err
		}
		if err := l.Host.Store().Record(Cmd.Event(LoopParty, LoopStageVerb, "[%s] woke worker", stage)); err != nil {
			return err
		}
		l.Mu.Lock()
		l.Stages[stage] = true
		l.Mu.Unlock()
	}
	return nil
}

// startEffort keeps a matching agents.models effort ahead of the loop's
// default; the start resolves that rule itself.
func (l *Loop) startEffort(args []string) []string {
	kind := workerRole
	if slices.Contains(args, "--"+reviewFlag) || slices.Contains(args, "--"+reviewOfSelfHostedFlag) {
		kind = taskKindReview
	}
	if i := slices.Index(args, taskKindArg); i >= 0 && i+1 < len(args) {
		kind = args[i+1]
	}
	if l.Host.Cfg().Agents.EffortFor(args[2], kind) != "" {
		return nil
	}
	return Cmd.EffortFlags(l.Host.Cfg().Supervisor.Effort)
}

func (l *Loop) HoldingStarts() bool {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	return l.StartsHeld
}

func (l *Loop) HoldStarts(held bool) {
	l.Mu.Lock()
	l.StartsHeld = held
	l.Mu.Unlock()
}

// PauseForReexec lets outstanding streams drain without launching more work
// on a replaced binary. Only the main loop calls reexec at the quiet point.
func (l *Loop) PauseForReexec() bool {
	if l != nil && l.Host != nil && state.WriterFenced(l.Host.Store()) {
		return true
	}
	l.Mu.Lock()
	b, failed := l.Bin, l.reexecFailed
	l.Mu.Unlock()
	return !failed && b != nil && b.Replaced()
}

func (l *Loop) stopPassForReexec(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if reexec, err := l.Reexec(ctx); err != nil || reexec {
		return true, err
	}
	return l.PauseForReexec(), nil
}
