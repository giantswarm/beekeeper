package cmd

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	defaultReviewModel = "opus"
	deskReviewApproved = "Desk review (opus): approved"
)

func (a *app) agentReviewCmd() *cobra.Command {
	var model, nitsOf string
	var lightLines int
	var verdicts bool
	c := &cobra.Command{
		Use:   "review <owner/repo>#<n>",
		Short: "Start one independent review of a green pull request head",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, ok := parseRef(args[0])
			if !ok {
				return usageErr("expected owner/repo#n")
			}
			p, err := readLoopPull(cmd.Context(), refName(r))
			if err != nil {
				return err
			}
			if verdicts {
				return a.printReviewVerdicts(cmd.Context(), p)
			}
			if p.State != superloop.Open || p.Draft || p.Head == "" {
				return refused("review requires an open, ready pull request with a head commit")
			}
			if !p.Green {
				return refused("CI must be green at %s before review", p.Head)
			}
			if nitsOf != "" {
				return a.checkReviewNits(cmd.Context(), p, nitsOf)
			}
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			events, err := a.store.Events(0, func(e state.Event) bool {
				return e.By.Name == loopParty.Name && e.Verb == "supervisor.review" && strings.HasPrefix(e.Detail, "[review "+p.Ref+"@"+p.Head+"]")
			})
			if err != nil {
				return err
			}
			if len(events) > 0 {
				return refused("%s already started review of head %s", p.Ref, p.Head)
			}
			p.Reviews, err = a.trustedReviews(cmd.Context(), p)
			if err != nil {
				return err
			}
			round, previous, err := reviewRound(st, p)
			if err != nil {
				return err
			}
			dir, _ := a.findReviewedRepoDir(cmd.Context(), r.Repo)
			if dir == "" {
				if cwd, err := os.Getwd(); err == nil && matchesReviewedRepo(cmd.Context(), cwd, r.Repo) {
					dir = cwd
				}
			}
			if dir == "" {
				return refused("no repository checkout found for %s; configure agents.dir or agents.roots", r.Repo)
			}
			threshold := a.cfg.Supervisor.ReviewLightLines
			if cmd.Flags().Changed("light-lines") {
				threshold = lightLines
			}
			context, err := reviewContext(cmd.Context(), p, previous, threshold)
			if err != nil {
				return err
			}
			worker := ""
			if park, ok := st.ReviewParkFor(p.Ref); ok {
				worker = park.Agent
			}
			if worker == "" {
				for _, ag := range st.Agents {
					if ag.Park != nil && strings.EqualFold(ag.Park.On, p.Ref) {
						worker = ag.Name
						break
					}
				}
			}
			act := superloop.Action{Kind: superloop.KindReview, Key: fmt.Sprintf("review %s@%s", p.Ref, p.Head), Ref: p.Ref, URL: p.URL, Head: p.Head, Round: round, Agent: worker, Message: context}
			l, err := a.newSupervisorLoop(false)
			if err != nil {
				return err
			}
			l.reviewDir = dir
			l.cachePull(p.Ref, p)
			oldModel := a.cfg.Supervisor.ReviewModel
			a.cfg.Supervisor.ReviewModel = cmp.Or(model, oldModel, defaultReviewModel)
			defer func() { a.cfg.Supervisor.ReviewModel = oldModel }()
			what, err := l.startReviewer(cmd.Context(), act)
			if err != nil {
				return err
			}
			if err := a.store.Record(about(event(loopParty, "supervisor.review", "%s", act.Line(what)), p.Ref)); err != nil {
				return err
			}
			if err := a.noteReviewStarter(act); err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, what)
			return err
		},
	}
	c.Flags().BoolVar(&verdicts, "verdict", false, "print the desk verdicts on the pull request, a commented one included, and start nothing")
	c.Flags().StringVar(&nitsOf, "nits-of", "", "supervisor attestation: this head only fixes the approval nits at this full SHA; no new reviewer")
	c.Flags().StringVar(&model, "model", "", "review model (default: supervisor.reviewModel, then opus)")
	c.Flags().IntVar(&lightLines, "light-lines", 0, "diff-only review below this changed-line count (default: supervisor.reviewLightLines, then 150; -1 disables size shortcut)")
	return c
}

// A completed review or an already assigned/requested round forbids a second
// session on that head, including after restart and before its comment lands.
func reviewRound(st *state.State, p superloop.Pull) (int, string, error) {
	round := 1
	previous := ""
	for _, r := range p.Reviews {
		if r.Verdict == "" {
			continue
		}
		if sameCommit(r.Head, p.Head) {
			return 0, "", refused("%s already has a review of head %s", p.Ref, p.Head)
		}
		round++
		previous = r.Body
	}
	for _, r := range st.DeskReviews {
		if !strings.EqualFold(r.Ref, p.Ref) {
			continue
		}
		if sameCommit(r.Head, p.Head) {
			return 0, "", refused("%s already has a reviewer for head %s", p.Ref, p.Head)
		}
		round = max(round, r.Round+1)
	}
	for _, r := range st.ReviewRequests {
		if !reviewRequestActive(st, r) || !strings.EqualFold(r.Ref, p.Ref) {
			continue
		}
		if sameCommit(r.Head, p.Head) {
			return 0, "", refused("%s already requested review of head %s", p.Ref, p.Head)
		}
		round = max(round, r.Round+1)
	}
	return round, previous, nil
}

func lightReview(p superloop.Pull, threshold int) bool {
	// Missing/truncated file data must not classify a large code PR as docs-only.
	docsTests := len(p.Files) > 0 && len(p.Files) == p.ChangedFiles
	for _, path := range p.Files {
		docsTests = docsTests && (strings.HasPrefix(path, "docs/") || strings.HasSuffix(path, ".md") || strings.HasSuffix(path, ".rst") || strings.HasSuffix(path, "_test.go") || strings.HasPrefix(path, "tests/") || strings.Contains(path, "/tests/") || strings.HasPrefix(path, "test/") || strings.Contains(path, "/test/") || strings.HasPrefix(filepath.Base(path), "test_") || strings.HasSuffix(path, ".test.ts") || strings.HasSuffix(path, ".test.js") || strings.HasSuffix(path, ".spec.ts") || strings.HasSuffix(path, ".spec.js") || strings.HasPrefix(path, "testdata/") || strings.Contains(path, "/testdata/"))
	}
	return docsTests || threshold > 0 && p.Additions+p.Deletions < threshold
}

// GraphQL distinguishes an absent parent from an unreadable one. Never fill
// "none" after an API failure, which would silently narrow the review scope.
var reviewParents = func(ctx context.Context, issues []string) ([]string, error) {
	var parents []string
	for _, issue := range issues {
		r, ok := parseRef(issue)
		if !ok {
			return nil, fmt.Errorf("invalid closing issue %q", issue)
		}
		owner, name, _ := strings.Cut(r.Repo, "/")
		raw, err := github.RunGH(ctx, "api", "graphql", "-f", "query=query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){issue(number:$number){parent{url}}}}", "-f", "owner="+owner, "-f", "name="+name, "-F", fmt.Sprintf("number=%d", r.N))
		if err != nil {
			return nil, fmt.Errorf("reading parent of %s: %w", issue, err)
		}
		var result struct {
			Data struct {
				Repository struct {
					Issue *struct {
						Parent *struct {
							URL string `json:"url"`
						} `json:"parent"`
					} `json:"issue"`
				} `json:"repository"`
			} `json:"data"`
			Errors []json.RawMessage `json:"errors"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, err
		}
		if len(result.Errors) > 0 || result.Data.Repository.Issue == nil {
			return nil, fmt.Errorf("parent of %s could not be read", issue)
		}
		if parent := result.Data.Repository.Issue.Parent; parent != nil && !slices.Contains(parents, parent.URL) {
			parents = append(parents, parent.URL)
		}
	}
	return parents, nil
}

// Only a supervisor may attest the semantic delta. File-name heuristics cannot
// prove that a change fixes nits, and a worker cannot authorize its own push.
func (a *app) checkReviewNits(ctx context.Context, p superloop.Pull, approved string) error {
	me, err := a.caller()
	if err != nil {
		return err
	}
	if !supervising(me) {
		return refused("only a supervisor may attest approval nits, never a worker session")
	}
	reviews, err := a.trustedReviews(ctx, p)
	if err != nil {
		return err
	}
	if len(reviews) == 0 {
		return refused("no trusted approval for %s", approved)
	}
	last := reviews[len(reviews)-1]
	if last.Verdict != superloop.Approved || last.Head != approved || approved == p.Head {
		return refused("--nits-of must name the latest trusted approved head before this push")
	}
	// Display the delta for the supervisor's audit trail; an attestation is an
	// explicit instruction that the supervisor has already checked these fixes.
	files, err := readChangedPast(ctx, p, approved)
	if err != nil {
		return err
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.ReviewNitChecks = append(st.ReviewNitChecks, state.ReviewNitCheck{Ref: p.Ref, Head: p.Head, ApprovedHead: approved, By: me, At: a.timeNow().UTC()})
		return []state.Event{about(event(me, "review.nits-checked", "%s at %s only fixes approval nits of %s", p.URL, p.Head, approved), p.Ref)}, nil
	}); err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.out, "Supervisor checked approval nits at %s against %s (%s); no second review. Park for the merge go.\n", p.Head, approved, strings.Join(files, ", "))
	return err
}

func reviewContext(ctx context.Context, p superloop.Pull, previous string, threshold int) (string, error) {
	parents, err := reviewParents(ctx, p.Closes)
	if err != nil {
		return "", err
	}
	if threshold == 0 {
		threshold = 150
	}
	mode := "Full review: create your own detached worktree at the reviewed head."
	if lightReview(p, threshold) {
		mode = "Light review: inspect the diff, issue and CI; no checkout and no build."
	}
	return fmt.Sprintf("CI green at %s\n%s\nIssues: %s\nParent epic(s): %s\nLast round's findings:\n%s", p.Head, mode, cmp.Or(strings.Join(p.Closes, ", "), "none linked; check the PR's stated scope"), cmp.Or(strings.Join(parents, ", "), "none"), cmp.Or(previous, "none (first round)")), nil
}

// All entry points reserve atomically before starting a process, including the
// automatic loop and the CLI racing on the same head.
// noteReviewStarter records the caller as who asked for act's review, for
// its verdict to reach it (reviewStarter).
func (a *app) noteReviewStarter(act superloop.Action) error {
	me, err := a.caller()
	if err != nil {
		return nil //nolint:nilerr // no session to tell: the review still stands
	}
	return a.store.Update(func(st *state.State) ([]state.Event, error) {
		for i := range st.ReviewRequests {
			if r := &st.ReviewRequests[i]; strings.EqualFold(r.Ref, act.Ref) && r.Head == act.Head {
				r.By = me
			}
		}
		return nil, nil
	})
}

// reviewStarter is the session that started the review ag reports the
// verdict of: the one that asked through agents review, else the one that
// ran agents start --review. A review the loop started names nobody: the
// loop reads its verdict itself.
func reviewStarter(st *state.State, ag state.Agent, report string) state.Party {
	if github.DeskVerdict(report) == "" {
		return state.Party{}
	}
	for _, r := range st.ReviewRequests {
		if r.By != (state.Party{}) && strings.EqualFold(ag.Name, superloop.ReviewerName(r.Ref, r.Round)) {
			return r.By
		}
	}
	if k := st.StartIndex(ag.Party); k >= 0 {
		if by := st.Starts[k].By; by != (state.Party{}) && by.Name != loopParty.Name && !by.Is(ag.Party) {
			return by
		}
	}
	return state.Party{}
}

// printReviewVerdicts says the desk verdicts on p, oldest first, as the
// loop and the merge gate read them: a verdict posted as a plain comment,
// all GitHub allows on a pull request of the reviewer's own login, counts
// as one GitHub recorded. Exit 3 when there is none.
func (a *app) printReviewVerdicts(ctx context.Context, p superloop.Pull) error {
	reviews, err := a.trustedReviews(ctx, p)
	if err != nil {
		return err
	}
	if len(reviews) == 0 {
		return refused("%s has no desk verdict", p.Ref)
	}
	slices.SortStableFunc(reviews, func(x, y superloop.Review) int { return x.At.Compare(y.At) })
	if a.json {
		return a.printJSON(reviews)
	}
	for _, r := range reviews {
		reviewed := cmp.Or(r.Head, r.Commit)
		line := fmt.Sprintf("%s: %s at head %s by %s, %s", p.Ref, r.Verdict, shortSHA(reviewed), r.Author, r.At.UTC().Format(time.RFC3339))
		if !sameCommit(reviewed, p.Head) {
			line += fmt.Sprintf(" (head is now %s)", shortSHA(p.Head))
		}
		if _, err := fmt.Fprintln(a.out, line); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) reserveReview(act superloop.Action) error {
	return a.store.Update(func(st *state.State) ([]state.Event, error) {
		for _, request := range st.ReviewRequests {
			if reviewRequestActive(st, request) && strings.EqualFold(request.Ref, act.Ref) && (request.Head == act.Head || request.Round == act.Round) {
				return nil, refused("%s already requested this head or review round", act.Ref)
			}
		}
		for _, round := range st.DeskReviews {
			if strings.EqualFold(round.Ref, act.Ref) && round.Head == act.Head {
				return nil, refused("%s already assigned review of this head", act.Ref)
			}
		}
		st.ReviewRequests = slices.DeleteFunc(st.ReviewRequests, func(r state.ReviewRequest) bool {
			return strings.EqualFold(r.Ref, act.Ref) && (r.Head == act.Head || r.Round == act.Round)
		})
		st.ReviewRequests = append(st.ReviewRequests, state.ReviewRequest{Ref: act.Ref, Head: act.Head, Round: act.Round, PID: os.Getpid()})
		return nil, nil
	})
}

// reviewRequestActive distinguishes a start in progress from historical intent.
// Legacy requests have no PID and can be retried when no reviewer owns them.
func reviewRequestActive(st *state.State, r state.ReviewRequest) bool {
	if r.PID > 0 && proc.Alive(r.PID) {
		return true
	}
	return reviewRequestOwned(st, r)
}

func reviewRequestOwned(st *state.State, r state.ReviewRequest) bool {
	name := superloop.ReviewerName(r.Ref, r.Round)
	for _, ag := range st.Agents {
		if strings.EqualFold(ag.Name, name) && !ag.Done {
			if ag.Execution != nil {
				rec, _ := executionRecord(st, *ag.Execution)
				if rec != nil && rec.Terminal() {
					continue
				}
			}
			return true
		}
	}
	// Execution reservations survive roster pruning; missing termination evidence
	// must never cause a second remote launch.
	for _, start := range st.Starts {
		if strings.EqualFold(start.Name, name) && start.Execution != nil && !start.Execution.Terminal() {
			return true
		}
	}
	for _, round := range st.DeskReviews {
		if strings.EqualFold(round.Ref, r.Ref) && (round.Head == r.Head || round.Round == r.Round) {
			return true
		}
	}
	return false
}

func (a *app) finishReviewRequest(act superloop.Action, started bool) {
	_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.ReviewRequests = slices.DeleteFunc(st.ReviewRequests, func(r state.ReviewRequest) bool {
			return strings.EqualFold(r.Ref, act.Ref) && r.Head == act.Head && r.PID == os.Getpid() && !started && !reviewRequestOwned(st, r)
		})
		for i := range st.ReviewRequests {
			r := &st.ReviewRequests[i]
			if strings.EqualFold(r.Ref, act.Ref) && r.Head == act.Head && r.PID == os.Getpid() {
				r.PID = 0
			}
		}
		return nil, nil
	})
}

// readChangedPast reads the files p's head changes past the change of
// reviewed (github.ChangedPast).
var readChangedPast = func(ctx context.Context, p superloop.Pull, reviewed string) ([]string, error) {
	r, ok := parseRef(p.Ref)
	if !ok || p.Base == "" {
		return nil, fmt.Errorf("%s names no pull request with a base", p.Ref)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return github.ChangedPast(ctx, github.RunGH, r.Repo, p.Base, reviewed, p.Head)
}
