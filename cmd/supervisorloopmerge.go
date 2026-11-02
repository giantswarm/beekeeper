package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// mergePull executes the planner's exact-head decision in the coordinator,
// including for workers on an execution host. No worker gets a merge grant.
func (l *supervisorLoop) mergePull(ctx context.Context, _ *state.State, act superloop.Action) (string, error) {
	a := l.a
	ref, ok := parseRef(act.Ref)
	if !ok || act.Head == "" {
		return "", refused("invalid merge decision")
	}
	st, err := a.store.Read()
	if err != nil {
		return "", err
	}
	if st.Stop != nil {
		return "", refused("the desk is stopped")
	}
	if lowBudget(st.Budget, a.cfg.GitHub.Floor, a.now) || a.resetFirst(st) {
		return "", errLoopLater
	}
	p, err := readLoopPull(ctx, act.Ref)
	if err != nil {
		return "", err
	}
	if p.State == superloop.Merged && (p.Head == act.Head || l.baseMerged(ctx, p, act.Head)) {
		return fmt.Sprintf("merged %s at %s", act.URL, p.Head), nil
	}
	if p.State != superloop.Open || p.Head != act.Head || !p.Green {
		return "", refused("merge head moved or current CI is not green")
	}
	// A detached gate owns the queued/running mutation. Observe its result
	// on later passes; do not submit another command for the same PR.
	if slices.ContainsFunc(st.Merges, func(m state.Merge) bool {
		return m.Repo == ref.Repo && m.PR == ref.N && (m.Phase == state.Waiting || m.Phase == state.Running) && merge.Runs(m, proc.Alive)
	}) {
		return "", errLoopLater
	}
	for _, worker := range st.Agents {
		if worker.Name == act.Agent && worker.Execution != nil {
			use := execution.MergeUse{Ref: *worker.Execution, Repository: ref.Repo, PR: ref.N, Head: act.Head}
			if err := a.executionMergeLive(ctx, st, use); err != nil {
				return "", err
			}
		}
	}
	if err := l.claim(act, act.Agent); err != nil {
		return "", err
	}
	if err := a.store.Record(about(event(loopParty, "merge.start", "merging %s at %s", act.URL, act.Head), act.Ref)); err != nil {
		return "", err
	}
	out, code, err := beekeeperRun(ctx, l.as(gateCmdName, "--limit", "0", "--reviewed-head", act.Head, "--", "devctl", "pr", superloop.KindMerge, ref.Repo, fmt.Sprint(ref.N))...)
	// Read after every outcome, including a lost response: a restart never
	// repeats a mutation already observed merged.
	p, readErr := readLoopPull(ctx, act.Ref)
	if readErr != nil {
		return "", readErr
	}
	if p.State == superloop.Merged {
		// The merge brings a behind branch up to date first: a merged head
		// that only merges the base into the reviewed one is the reviewed
		// change.
		if p.Head != act.Head && !l.baseMerged(ctx, p, act.Head) {
			return "", refused("merged head differs from the reviewed head; reconciliation required")
		}
		return fmt.Sprintf("merged %s at %s (merge commit %s)", act.URL, p.Head, p.MergeCommit), nil
	}
	if err != nil {
		return "", err
	}
	var reply github.SquashDoc
	mutationResult := json.NewDecoder(strings.NewReader(out)).Decode(&reply) == nil && reply.Route == reviewedMergeRoute && reply.Repo == ref.Repo && reply.Number == ref.N && reply.HeadSha == act.Head
	if code == ExitGateQueued || code == ExitGateDuplicate && !mutationResult {
		return "", errLoopLater
	}
	if code == ExitGateUnread || code == ExitGateRefused {
		return "", refused("merge gate refused: %s", oneLine(out))
	}
	if code == 0 {
		return "", refused("merge command ended without a confirmed merge")
	}
	if !mutationResult || code != github.SquashNotApplicable && code != github.SquashRed {
		return "", refused("merge command failed (exit %d): %s", code, oneLine(out))
	}
	if reply.Reason != "" {
		out = reply.Reason
	}

	// A confirmed open pull request needs repair. Recording this action after
	// the wake prevents repeated repair wakes for the same failed head.
	act.Message = fmt.Sprintf("The supervisor could not merge %s at reviewed head %s (exit %d): %s. Repair the branch, push all fixes together, wait for green CI and park for desk review again. The supervisor performs the merge.", act.URL, act.Head, code, oneLine(out))
	if err := l.wake(ctx, act); err != nil {
		return "", err
	}
	return "merge failed; branch repair requested", nil
}

// endMerged closes tasks that explicitly declared no post-merge work and
// wakes only tasks with a remaining release, rollout or proof. Older parks
// retain the existing release-containment and retirement behavior.
func (l *supervisorLoop) endMerged(ctx context.Context, st *state.State, act superloop.Action) (string, error) {
	i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool {
		return ag.Park != nil && ag.Park.On == act.Ref && ag.Park.ReviewHead != "" && ag.Park.PostMerge != nil
	})
	if i < 0 {
		return fmt.Sprintf("%s %s: tracking ends", act.URL, act.Message), nil
	}
	ag := st.Agents[i]
	if err := l.claim(act, ag.Name); err != nil {
		return "", err
	}
	if act.Message == strings.ToLower(superloop.Closed) {
		act.Agent = ag.Name
		act.Message = fmt.Sprintf("%s closed without merging. Continue or finish the owning task, and report what remains open.", act.URL)
		if err := l.wake(ctx, act); err != nil {
			return "", err
		}
		return "closed; worker told to continue its task", nil
	}
	if work := strings.TrimSpace(*ag.Park.PostMerge); work != "" {
		act.Agent = ag.Name
		act.Message = fmt.Sprintf("The supervisor merged %s. Complete the remaining post-merge work: %s. Report the version, proof, live facts and open items with agents idle --done --report and --problem.", act.URL, work)
		if err := l.wake(ctx, act); err != nil {
			return "", err
		}
		return "merged; post-merge work requested", nil
	}
	// Do not finish a task serving other open work merely because one PR merged.
	serves := loopServes(st, ag)
	if serves != "" && serves != act.Ref && l.open(ctx, serves) {
		act.Agent = ag.Name
		act.Message = fmt.Sprintf("The supervisor merged %s. The task at %s is still open or its closure could not be confirmed. Continue its remaining work, or verify it is complete and report done.", act.URL, issueURL(serves))
		if err := l.wake(ctx, act); err != nil {
			return "", err
		}
		return "merged; worker woken for remaining open work", nil
	}
	if serves != "" && serves != act.Ref && l.a.cfg.Board.Owner != "" {
		if _, err := l.run(ctx, loopBoard, "move", serves, "Done"); err != nil {
			return "", err
		}
	}
	err := l.a.store.Update(func(current *state.State) ([]state.Event, error) {
		i := slices.IndexFunc(current.Agents, func(other state.Agent) bool { return other.Is(ag.Party) })
		if i < 0 || current.Agents[i].Done {
			return nil, nil
		}
		worker := &current.Agents[i]
		report := fmt.Sprintf("%s merged at %s; no post-merge work declared; open items: none", act.URL, l.merged[act.Ref].MergeCommit)
		reportIdle(worker, time.Now())
		worker.Done = true
		endNotes(current, worker.Party, l.a.now)
		clearRecords(current, worker.Party)
		current.WorkerReports = append(current.WorkerReports, state.WorkerReport{Finished: true, By: worker.Party, At: l.a.now, Task: worker.LastTask, Text: report})
		return []state.Event{event(worker.Party, superloop.VerbDone, "%s done: %s", worker.Name, worker.LastTask), event(worker.Party, superloop.VerbReport, "%s: %s", worker.Name, report)}, nil
	})
	return "merged; task closed without waking worker", err
}

// The reasons a reviewed merge stops for desk policy, before and after a
// branch update alike.
const (
	reasonDeskStopped = "the desk is stopped"
	reasonMergeHeld   = "merge held by desk policy"
)

// reviewedMerge is the gate's mutation child. A branch behind its base it
// updates once, as the gate's --update-branch would, and merges the updated
// head only once its checks are green and the approval covers it
// (updateReviewedBranch); it never waits for a head anyone else pushed, or
// retries a mutation. The gate retains its
// ordinary holds, queue, readiness, review and release settlement policy.
func (a *app) reviewedMerge(ctx context.Context, repo string, number int, head string) github.SquashDoc {
	doc := github.SquashDoc{Repo: repo, Number: number, HeadSha: head, SchemaVersion: 1, Command: "pr merge", Route: reviewedMergeRoute, ExitCode: github.SquashNotApplicable}
	method := a.cfg.Merge.Methods[repo]
	if method == "" {
		method = "squash"
	}
	if method != "squash" && method != "rebase" && method != superloop.KindMerge {
		doc.Reason = "invalid configured merge method"
		return doc
	}
	ref := fmt.Sprintf("%s#%d", repo, number)
	p, err := readLoopPull(ctx, ref)
	if err != nil {
		doc.Reason = err.Error()
		return doc
	}
	if p.State == superloop.Merged && p.Head == head {
		doc.ExitCode = 0
		doc.MergeCommitSha = p.MergeCommit
		return doc
	}
	if p.State != superloop.Open || p.Head != head || !p.Green {
		doc.Reason = "merge head moved or current CI is not green"
		return doc
	}
	if p.Draft || p.Mergeable == "CONFLICTING" {
		doc.Reason = "branch is draft or conflicts with its base"
		return doc
	}
	behind := p.MergeState == mergeStateBehind
	if !behind {
		n, err := readReviewedBehind(ctx, p)
		if err != nil {
			doc.ExitCode = github.SquashTooling
			doc.Reason = "base comparison unavailable: " + err.Error()
			return doc
		}
		behind = n > 0
	}
	// A behind branch is brought up to date below, once the merge is
	// authorized: by merging the base, which a rebase merge cannot take.
	if behind && method == "rebase" {
		doc.Reason = "branch is behind its base"
		return doc
	}
	st, err := a.store.Read()
	if err != nil {
		doc.Reason = err.Error()
		return doc
	}
	evs, logErr := a.store.Events(0, func(e state.Event) bool { return e.By == loopParty && e.Subject == ref })
	if logErr != nil {
		doc.Reason = "merge authority unreadable"
		return doc
	}
	claimed := false
	claimAgent := ""
	for _, e := range evs {
		key, rest, ok := keyed(e.Detail)
		if !ok {
			continue
		}
		if key == "go "+ref+"@"+head {
			if e.Verb == loopClaimVerb {
				claimed = true
				claimAgent = rest
			}
			if e.Verb == "supervisor.merge" || e.Verb == "supervisor.go" {
				claimed = false
			}
		}
		if e.Verb == "supervisor.end" && key == "end "+ref {
			claimed = false
		}
	}
	for _, worker := range st.Agents {
		if worker.Name == claimAgent && worker.Execution != nil {
			use := execution.MergeUse{Ref: *worker.Execution, Repository: repo, PR: number, Head: head}
			record, _ := executionRecord(st, *worker.Execution)
			if record == nil || record.StopRequest != nil || record.Terminal() {
				doc.Reason = "execution stopped"
				return doc
			}
			if err := a.authenticateExecutionReview(ctx, st, use, p); err != nil {
				doc.Reason = err.Error()
				return doc
			}
		}
	}
	if !claimed {
		doc.Reason = "no pending exact-head supervisor merge claim"
		return doc
	}
	if st.Stop != nil {
		doc.Reason = reasonDeskStopped
		return doc
	}
	if _, held := merge.BlockingFor(st, time.Now(), loopParty.Name, repo, number, a.cfg.LaneOf(repo)); held {
		doc.Reason = reasonMergeHeld
		return doc
	}
	merging := head
	if behind {
		up, reason, code := updateReviewedBranch(ctx, p, head)
		if code != github.SquashMerged {
			doc.ExitCode, doc.Reason = code, reason
			return doc
		}
		// The wait for the updated head's checks is long: a stop or a hold
		// meanwhile still stops the merge.
		if st, err = a.store.Read(); err != nil {
			doc.Reason = err.Error()
			return doc
		}
		if st.Stop != nil {
			doc.Reason = reasonDeskStopped
			return doc
		}
		if _, held := merge.BlockingFor(st, time.Now(), loopParty.Name, repo, number, a.cfg.LaneOf(repo)); held {
			doc.Reason = reasonMergeHeld
			return doc
		}
		p, merging = up, up.Head
	}
	argv := []string{"gh", "pr", superloop.KindMerge, fmt.Sprint(number), "--repo", repo, "--" + method, "--delete-branch", "--match-head-commit", merging}
	if a.cfg.Merge.AdminMerge(repo) {
		argv = append(argv, "--admin")
	}
	out, stderr, code, runErr := loopCommand(ctx, "", argv)
	doc.Reason = oneLine(out + stderr)
	doc.ExitCode = code
	if runErr != nil {
		doc.ExitCode = github.SquashTooling
		doc.Reason = runErr.Error()
	}
	p, err = readLoopPull(ctx, ref)
	if err != nil {
		doc.ExitCode = github.SquashTooling
		doc.Reason = err.Error()
		return doc
	}
	if p.State == superloop.Merged && p.Head == merging {
		doc.ExitCode = 0
		doc.MergeCommitSha = p.MergeCommit
		doc.Verdict = "merged"
	} else if doc.ExitCode == 0 {
		doc.ExitCode = github.SquashNotApplicable
		doc.Reason = "merge not confirmed"
	}
	return doc
}

// Compare against the current base even when branch protection does not
// require an up-to-date branch. Admin permission never waives this check.
var readReviewedBehind = func(ctx context.Context, p superloop.Pull) (int, error) {
	if p.Base == "" {
		return 0, nil
	}
	ref, ok := parseRef(p.Ref)
	if !ok {
		return 0, fmt.Errorf("invalid pull request")
	}
	out, err := github.RunGH(ctx, "api", "repos/"+ref.Repo+"/compare/"+url.PathEscape(p.Base)+"..."+url.PathEscape(p.Head), "--jq", ".behind_by")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}
