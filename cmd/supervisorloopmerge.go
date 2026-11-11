package cmd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const loopWorkerMergeGo = "merge go given"
const reviewedMergeQueued = "queued"
const loopMergeFailed = "merge.failed"
const loopMergeFailureDropped = "merge.failure-dropped"

func pullNeedsBranchRepair(p superloop.Pull) bool {
	return p.Draft || p.Mergeable == "CONFLICTING"
}

func ghMergeNeedsBranchRepair(reason string) bool {
	lower := strings.ToLower(reason)
	return strings.Contains(lower, "conflict") || strings.Contains(lower, "cannot be cleanly created")
}

// A branch policy refusal belongs to the coordinator, even when GitHub's
// merge state also says BEHIND. A branch edit cannot supply missing review.
func ghMergePolicyRefused(reason string) bool {
	lower := strings.ToLower(reason)
	return strings.Contains(lower, "behind") || strings.Contains(lower, "up to date") || strings.Contains(lower, "out of date") || strings.Contains(lower, "out-of-date") || strings.Contains(lower, "base branch policy") || strings.Contains(lower, "review_required") || strings.Contains(lower, "required approving review")
}

// mergePull executes the planner's exact-head decision in the coordinator,
// including for workers on an execution host. Failed or unreadable validation
// never delegates a direct merge around the required server-side queue.
func (l *supervisorLoop) mergePull(ctx context.Context, _ *state.State, act superloop.Action) (result string, resultErr error) {
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
	// A hold is a wait, not a failed merge attempt. Keep the exact-head
	// review available and reconsider it on the next pass, without claiming
	// the go, entering the gate or starting failure backoff.
	waitKey := act.Key + " hold"
	if h, held := merge.BlockingFor(st, a.timeNow(), loopParty.Name, ref.Repo, ref.N, a.cfg.LaneOf(ref.Repo)); held {
		l.sayOnce(waitKey, "SUPERVISOR WAIT %s on hold:%s", act.URL, h.Target)
		return "", errLoopLater
	}
	l.clearSaid(waitKey)
	if boardBudgetLow(st.Budget, a.cfg.GitHub.Floor, a.timeNow()) || a.resetFirst(st) {
		return "", errLoopLater
	}
	if err := a.fatalPublicationBarrier(ctx, st, ref.Repo, ref.N); err != nil {
		l.sayOnce(waitKey, "SUPERVISOR WAIT %s: %v", act.URL, err)
		return "", errLoopLater
	}
	repairStage := act.Key + " repair"
	l.mu.Lock()
	repairing, reason := l.stages[repairStage], l.stageDetail[repairStage]
	l.mu.Unlock()
	if repairing {
		return l.repairPull(ctx, st, act, reason)
	}
	p, err := readLoopPull(ctx, act.Ref)
	if err != nil {
		return "", err
	}
	if p.State == superloop.Merged && (p.Head == act.Head || l.baseMerged(ctx, p, act.Head)) {
		closed, closeErr := closeMergedPullIssues(ctx, st, p)
		l.recordClosedPullIssues(closed)
		if closeErr != nil {
			return "", closeErr
		}
		return fmt.Sprintf("merged %s at %s", act.URL, p.Head), nil
	}
	if p.State != superloop.Open || p.Head != act.Head || !p.Green {
		return "", refused("merge head moved or current CI is not green")
	}
	if err := ensureCombinedPullCloses(ctx, st, &p); err != nil {
		return "", err
	}
	if latestReviewRequestsChanges(p) || reviewOvertookDecision(p, act) {
		return "", refused("a review requested changes; new supervisor approval required")
	}
	repair := func(code int, reason string) (string, error) {
		_, _ = stopMerges(a.store, loopParty, func(m state.Merge) bool {
			return m.Repo == ref.Repo && m.PR == ref.N && m.Phase == state.Waiting
		}, false, func(state.Merge) string {
			return reason
		})
		if err := a.store.Update(func(current *state.State) ([]state.Event, error) {
			merge.Retry(current, ref.Repo, ref.N)
			return nil, nil
		}); err != nil {
			return "", err
		}
		return l.repairPull(ctx, st, act, fmt.Sprintf("(exit %d): %s", code, oneLine(reason)))
	}
	if p.Draft || strings.EqualFold(p.Mergeable, mergeableConflicting) || strings.EqualFold(p.MergeState, github.DirtyState) {
		return repair(github.SquashNotApplicable, "branch is draft or conflicts with its base")
	}
	// A detached gate owns the queued/running mutation. Observe its result
	// on later passes; do not submit another command for the same PR.
	if slices.ContainsFunc(st.Merges, func(m state.Merge) bool {
		return m.Repo == ref.Repo && m.PR == ref.N && (m.Phase == state.Waiting || m.Phase == state.Running) && merge.Runs(m, proc.Alive)
	}) {
		return "", errLoopLater
	}
	for _, f := range st.Failed {
		if f.Repo != ref.Repo || f.PR != ref.N || f.By.Name != loopParty.Name {
			continue
		}
		if f.Head != act.Head || staleMergeResultFailure(f.Reason, act.Head) || ghMergePolicyRefused(f.Reason) || github.RateLimited(errors.New(f.Reason)) {
			// A different (or unknown legacy) head supplies no failure proof
			// for this approved commit. Let the gate validate it again.
			if err := a.store.Update(func(current *state.State) ([]state.Event, error) {
				merge.Retry(current, ref.Repo, ref.N)
				return nil, nil
			}); err != nil {
				return "", err
			}
			if f.Head != act.Head || staleMergeResultFailure(f.Reason, act.Head) {
				if err := l.logStaleMergeFailure(act.Ref, act.Head); err != nil {
					return "", err
				}
			}
			continue
		}
		if f.Exit == github.SquashNotApplicable || f.Exit == github.SquashRed {
			return repair(f.Exit, cmp.Or(f.Reason, "merge failed in lane"))
		}
	}
	var execRef *execution.Ref
	for _, worker := range st.Agents {
		if worker.Name == act.Agent && worker.Execution != nil {
			execRef = worker.Execution
			break
		}
	}
	if execRef == nil {
		if rp, ok := st.ReviewParkFor(act.Ref); ok && rp.Execution != nil {
			execRef = rp.Execution
		}
	}
	if execRef != nil {
		use := execution.MergeUse{Ref: *execRef, Repository: ref.Repo, PR: ref.N, Head: act.Head}
		if err := a.executionMergeDecisionLive(ctx, st, use, true); err != nil {
			return "", err
		}
	}
	if err := l.claim(act, act.Agent); err != nil {
		return "", err
	}
	if err := a.store.Record(about(event(loopParty, "merge.authorized", "%s at %s: the supervisor loop authorizes the reviewed green fixes", act.URL, act.Head), act.Ref)); err != nil {
		return "", err
	}
	if err := a.store.Record(about(event(loopParty, "merge.start", "merging %s at %s", act.URL, act.Head), act.Ref)); err != nil {
		return "", err
	}
	// Every submitted attempt has an outcome in this pass, including waits
	// and unreadable confirmation. A pending outcome keeps the claim open;
	// it never grants a worker permission to bypass the gate.
	failed := ""
	pending := "awaiting merge confirmation"
	defer func() {
		verb, detail := "merge.finished", result
		switch {
		case failed != "":
			verb, detail = loopMergeFailed, failed
		case errors.Is(resultErr, errLoopLater):
			verb, detail = "merge.pending", pending
		case resultErr != nil:
			verb, detail = loopMergeFailed, resultErr.Error()
		}
		if err := a.store.Record(about(event(loopParty, verb, "%s at %s: %s", act.URL, act.Head, oneLine(detail)), act.Ref)); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	gateArgs := []string{gateCmdName, "--wait", "0s", "--limit", "0", "--reviewed-head", act.Head, "--", devctlBinary, "pr", superloop.KindMerge, ref.Repo, fmt.Sprint(ref.N)}
	out, code, err := beekeeperRun(ctx, l.as(gateArgs...)...)
	if err != nil && ctx.Err() == nil {
		if saveErr := a.operationOutcome(runMerge, "merge-command-unavailable", act.URL, false); saveErr != nil {
			return "", saveErr
		}
	}
	// Read after every outcome, including a lost response: a restart never
	// repeats a mutation already observed merged.
	p, readErr := readLoopPull(ctx, act.Ref)
	if readErr != nil {
		return "", readErr
	}
	l.cachePull(act.Ref, p)
	if p.State == superloop.Merged {
		// Where the base requires up-to-date branches and the desk merges
		// without --admin, the merge brings a behind branch up to date
		// first: a merged head that only merges the base into the reviewed
		// one is the reviewed change.
		if p.Head != act.Head && !l.baseMerged(ctx, p, act.Head) {
			return "", refused("merged head differs from the reviewed head; reconciliation required")
		}
		closed, closeErr := closeMergedPullIssues(ctx, st, p)
		l.recordClosedPullIssues(closed)
		if closeErr != nil {
			return "", closeErr
		}
		return fmt.Sprintf("merged %s at %s (merge commit %s)", act.URL, p.Head, p.MergeCommit), nil
	}
	var reply github.SquashDoc
	mutationResult := err == nil && json.NewDecoder(strings.NewReader(out)).Decode(&reply) == nil && reply.Route == reviewedMergeRoute && reply.Repo == ref.Repo && reply.Number == ref.N && reply.HeadSha == act.Head
	policyReason := out
	if mutationResult {
		policyReason = reply.Reason
	}
	if ghMergePolicyRefused(policyReason) {
		pending = "repository merge policy refused; retrying next round: " + oneLine(policyReason)
		stage := act.Key + " admin configuration"
		l.mu.Lock()
		reported := l.stages[stage]
		l.mu.Unlock()
		if !reported {
			guidance := "repository merge policy remains unmet"
			if strings.Contains(policyReason, "--admin") {
				guidance = "check merge.admin for " + ref.Repo
			}
			message := fmt.Sprintf("SUPERVISOR MERGE POLICY REFUSED %s: %s; reviewDecision=%s; retrying next round: %s", act.URL, guidance, p.ReviewDecision, oneLine(policyReason))
			if err := a.store.Record(about(event(loopParty, loopStageVerb, "[%s] %s", stage, message), act.Ref)); err != nil {
				return "", err
			}
			l.mu.Lock()
			l.stages[stage] = true
			l.mu.Unlock()
			_, _ = fmt.Fprintln(a.out, message)
		}
		return "", errLoopLater
	}
	if err == nil && (code == ExitGateQueued || code == ExitGateDuplicate && !mutationResult) {
		pending = fmt.Sprintf("merge gate waiting (exit %d): %s", code, oneLine(out))
		return "", errLoopLater
	}
	// An unavailable read never authorizes a merge and is not a durable
	// policy refusal. Reconsider it on a budget-admitted pass, so quota
	// recovery does not wait for the generic exponential failure backoff.
	if err == nil && (code == ExitGateUnread || code == ExitGateRefused && github.RateLimited(errors.New(out))) {
		pending = "GitHub read unavailable; retrying when GitHub answers: " + oneLine(out)
		return "", errLoopLater
	}
	if err == nil && code == ExitGateRefused || mutationResult && code == github.SquashRefused {
		if mutationResult && reply.Reason != "" {
			out = reply.Reason
		}
		return "", refused("merge gate refused: %s", oneLine(out))
	}
	if code == 0 && mutationResult && reply.Verdict == reviewedMergeQueued {
		pending = "server-side merge queued; awaiting merge-result checks: " + oneLine(reply.Reason)
		return "", errLoopLater
	}
	if mutationResult && code == github.SquashTooling {
		if strings.HasPrefix(reply.Reason, reasonMergeValidationUnavailable) {
			return "", refused("%s", reply.Reason)
		}
		return "", refused("%s: %s", reasonMergeValidationUnavailable, reply.Reason)
	}
	if p.State != superloop.Open || p.Head != act.Head || !p.Green {
		return "", refused("merge head moved or current CI is not green")
	}
	if latestReviewRequestsChanges(p) || reviewOvertookDecision(p, act) {
		return "", refused("a review requested changes; new supervisor approval required")
	}
	if mutationResult && (code == github.SquashNotApplicable || code == github.SquashRed) {
		if reply.Reason != "" {
			out = reply.Reason
		}
		if staleMergeResultFailure(out, act.Head) {
			pending = "stale merge-result failure dropped; awaiting fresh validation"
			return repair(code, out)
		}
		failed = fmt.Sprintf("merge failed (exit %d): %s; branch repair requested", code, oneLine(out))
		return repair(code, out)
	}
	// An unreadable gate result supplies no merge-result proof either.
	// Never grant a direct worker merge as a fallback around validation.

	if err != nil {
		return "", err
	}
	if code == 0 {
		return "", refused("merge command ended without a confirmed merge")
	}
	if mutationResult && reply.Reason != "" {
		out = reply.Reason
	}
	return "", refused("merge command failed (exit %d): %s", code, oneLine(out))
}

// Older repair records have only the checker's diagnostic. Its head must
// agree with the decision even if a surrounding gate record names that decision.
// Check every diagnostic, since a tool response can contain several lines.
func staleMergeResultFailure(reason, head string) bool {
	for {
		_, rest, found := strings.Cut(reason, "merge result (")
		if !found {
			return false
		}
		identity, tail, closed := strings.Cut(rest, ")")
		_, tested, paired := strings.Cut(identity, " + ")
		if !closed || !paired || tested == "" || head == "" || (tested != head && tested != shortSHA(head)) {
			return true
		}
		reason = tail
	}
}

func (l *supervisorLoop) logStaleMergeFailure(ref, head string) error {
	message := fmt.Sprintf("SUPERVISOR MERGE DROPPED stale failure for %s at %s; rechecking through the merge gate", ref, head)
	if err := l.a.store.Record(about(event(loopParty, loopMergeFailureDropped, "%s", message), ref)); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(l.a.out, message)
	return nil
}

// dropInvalidMergeRepairs also retires persisted repairs whose merge-result head
// contradicts the decision. It undoes a branch repair an older loop requested
// for a merge refused by base branch policy: no branch
// change repairs that refusal. Its repair stage and the go it completed are
// closed durably, so the unchanged approved head is merged again on this
// pass without a new review or the worker's action. A repair stage for any
// other reason stands.
func (l *supervisorLoop) dropInvalidMergeRepairs(taken []superloop.Taken, stages map[string]bool, stageDetail map[string]string) ([]superloop.Taken, error) {
	for stage, detail := range stageDetail {
		key, ok := strings.CutSuffix(stage, " repair")
		if !ok || !stages[stage] || !strings.HasPrefix(key, superloop.KindGo+" ") {
			continue
		}
		ref := strings.TrimPrefix(key, superloop.KindGo+" ")
		head := ""
		if i := strings.LastIndex(ref, "@"); i >= 0 {
			head, _, _ = strings.Cut(ref[i+1:], " ")
			ref = ref[:i]
		}
		stale := staleMergeResultFailure(detail, head)
		if !stale && !ghMergePolicyRefused(detail) {
			continue
		}
		why := "base branch policy refusal requires coordinator policy resolution"
		if stale {
			why = "stale merge-result failure does not describe the reviewed head"
		}
		// A preview reconciles only its in-memory plan. Leave the durable
		// records for the next real pass, which holds the loop lock.
		if !l.dryRun {
			if err := l.a.store.Record(about(event(loopParty, loopUnstageVerb, "[%s] %s", stage, why), ref)); err != nil {
				return taken, err
			}
		}
		delete(stages, stage)
		delete(stageDetail, stage)
		if slices.ContainsFunc(taken, func(t superloop.Taken) bool { return t.Key == key }) {
			if !l.dryRun {
				if err := l.a.store.Record(about(event(loopParty, loopClaimEndVerb, "[%s] %s", key, why), ref)); err != nil {
					return taken, err
				}
			}
			taken = slices.DeleteFunc(taken, func(t superloop.Taken) bool { return t.Key == key })
		}
		if stale && !l.dryRun {
			if err := l.logStaleMergeFailure(ref, head); err != nil {
				return taken, err
			}
		} else if !l.dryRun {
			_, _ = fmt.Fprintf(l.a.out, "SUPERVISOR MERGE RETRY %s: %s; retrying with configured merge identity and policy\n", key, why)
		}
	}
	return taken, nil
}

// repairPull hands an open pull request whose merge was not applicable or red
// to its worker once, recording the repair stage before waking or starting a
// fixer so capacity waits never retry the merge command each pass.
func (l *supervisorLoop) repairPull(ctx context.Context, st *state.State, act superloop.Action, reason string) (string, error) {
	if staleMergeResultFailure(reason, act.Head) {
		if err := l.logStaleMergeFailure(act.Ref, act.Head); err != nil {
			return "", err
		}
		return "", errLoopLater
	}
	repairStage := act.Key + " repair"
	l.mu.Lock()
	repairing := l.stages[repairStage]
	l.mu.Unlock()
	if !repairing {
		if err := l.a.store.Record(about(event(loopParty, loopStageVerb, "[%s] %s", repairStage, reason), act.Ref)); err != nil {
			return "", err
		}
		l.mu.Lock()
		if l.stages == nil {
			l.stages = map[string]bool{}
		}
		if l.stageDetail == nil {
			l.stageDetail = map[string]string{}
		}
		l.stages[repairStage] = true
		l.stageDetail[repairStage] = reason
		l.mu.Unlock()
	}
	act.Message = fmt.Sprintf("The supervisor could not merge %s at reviewed head %s %s. Repair the branch, push all fixes together without local tests or lints and immediately park for the loop to run CI; if the new head only rebases onto or merges the base branch to resolve conflicts, its earlier approval still covers it and the supervisor merges once CI is green without a new review.", act.URL, act.Head, reason)
	if err := l.wakeOrStartFixer(ctx, st, act); err != nil {
		return "", err
	}
	return "merge failed; branch repair requested", nil
}

// endMerged closes tasks that explicitly declared no post-merge work and
// wakes only tasks with a remaining release, rollout or proof. Older parks
// retain the existing release-containment and retirement behavior.
func (l *supervisorLoop) endMerged(ctx context.Context, st *state.State, act superloop.Action) (string, error) {
	if latest, err := l.a.store.Read(); err == nil {
		st = latest
	}
	if by, ok := strings.CutPrefix(act.Message, "absorbed by "); ok {
		return l.endAbsorbed(ctx, st, act, strings.TrimSpace(by))
	}
	if act.Message != strings.ToLower(superloop.Closed) {
		l.mu.Lock()
		mp, ok := l.merged[act.Ref]
		l.mu.Unlock()
		if !ok {
			var readErr error
			mp, readErr = l.readPull(ctx, act.Ref)
			if readErr != nil {
				return "", readErr
			}
		}
		mp.Ref = cmp.Or(mp.Ref, act.Ref)
		mp.URL = cmp.Or(mp.URL, act.URL)
		closed, closeErr := closeMergedPullIssues(ctx, st, mp)
		l.recordClosedPullIssues(closed)
		if closeErr != nil {
			return "", closeErr
		}
	}
	i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool {
		return ag.Park != nil && ag.Park.On == act.Ref && ag.Park.ReviewHead != "" && ag.Park.PostMerge != nil
	})
	if i < 0 {
		rp, ok := st.ReviewParkFor(act.Ref)
		if !ok || rp.PostMerge == nil {
			_ = l.a.store.Update(func(current *state.State) ([]state.Event, error) {
				current.ReviewParks = slices.DeleteFunc(current.ReviewParks, func(p state.ReviewPark) bool { return strings.EqualFold(p.On, act.Ref) })
				return nil, nil
			})
			return fmt.Sprintf("%s %s: tracking ends", act.URL, act.Message), nil
		}
		if err := l.claim(act, rp.Agent); err != nil {
			return "", err
		}
		clearReviewPark := func() {
			_ = l.a.store.Update(func(current *state.State) ([]state.Event, error) {
				current.ReviewParks = slices.DeleteFunc(current.ReviewParks, func(p state.ReviewPark) bool { return strings.EqualFold(p.On, act.Ref) })
				return nil, nil
			})
		}
		if act.Message == strings.ToLower(superloop.Closed) {
			if strings.TrimSpace(*rp.PostMerge) != "" || (rp.Serves != "" && !strings.EqualFold(rp.Serves, act.Ref) && l.open(ctx, rp.Serves)) {
				act.Agent = rp.Agent
				act.Message = fmt.Sprintf("%s closed without merging. Continue or finish the owning task, and report what remains open.", act.URL)
				if err := l.wakeOrStartFixer(ctx, st, act); err != nil {
					return "", err
				}
				clearReviewPark()
				return "closed; worker told to continue its task", nil
			}
			err := l.a.store.Update(func(current *state.State) ([]state.Event, error) {
				current.ReviewParks = slices.DeleteFunc(current.ReviewParks, func(p state.ReviewPark) bool { return strings.EqualFold(p.On, act.Ref) })
				for i := range current.Agents {
					worker := &current.Agents[i]
					if worker.Is(rp.Party) && worker.Park != nil && worker.Park.ReviewHead != "" {
						reportIdle(worker, l.a.timeNow())
						worker.Done = true
					}
				}
				endNotes(current, rp.Party, l.a.timeNow())
				clearRecords(current, rp.Party)
				return nil, nil
			})
			return "closed; task already settled", err
		}
		if work := strings.TrimSpace(*rp.PostMerge); work != "" {
			act.Agent = rp.Agent
			act.Message = fmt.Sprintf("The supervisor merged %s. Complete the remaining post-merge work: %s. Report the version, proof, live facts and open items with agents idle --done --report and --problem.", act.URL, work)
			if err := l.wakeOrStartFixer(ctx, st, act); err != nil {
				return "", err
			}
			clearReviewPark()
			return "merged; post-merge work requested", nil
		}
		if l.otherReviewParks(rp.Party, act.Ref) {
			clearReviewPark()
			return "merged; other pull requests still await review or merge", nil
		}
		// A wake queues the continuation without clearing the current park.
		// Its review record may already be gone after an earlier merge, so
		// the last saved request cannot prove that the owner has finished.
		if slices.ContainsFunc(st.Agents, func(worker state.Agent) bool {
			p := worker.Park
			return worker.Is(rp.Party) && !worker.Done && p != nil &&
				!strings.EqualFold(p.On, act.Ref) && p.ReviewHead != "" &&
				p.PostMerge != nil && strings.TrimSpace(*p.PostMerge) != ""
		}) {
			clearReviewPark()
			return "merged; worker still has post-merge work", nil
		}
		if rp.Serves != "" && rp.Serves != act.Ref && l.open(ctx, rp.Serves) {
			act.Agent = rp.Agent
			act.Message = fmt.Sprintf("The supervisor merged %s. The task at %s is still open or its closure could not be confirmed. Continue its remaining work, or verify it is complete and report done.", act.URL, issueURL(rp.Serves))
			if err := l.wakeOrStartFixer(ctx, st, act); err != nil {
				return "", err
			}
			clearReviewPark()
			return "merged; worker woken for remaining open work", nil
		}
		if err := l.moveServedDone(ctx, rp.Serves, act.Ref); err != nil {
			return "", err
		}
		err := l.a.store.Update(func(current *state.State) ([]state.Event, error) {
			current.ReviewParks = slices.DeleteFunc(current.ReviewParks, func(p state.ReviewPark) bool { return strings.EqualFold(p.On, act.Ref) })
			// The last completed ref may differ from the worker's current
			// park when its PRs merge out of order. Finish that parked owner too.
			for i := range current.Agents {
				worker := &current.Agents[i]
				if worker.Is(rp.Party) && worker.Park != nil && worker.Park.ReviewHead != "" {
					reportIdle(worker, l.a.timeNow())
					worker.Done = true
				}
			}
			endNotes(current, rp.Party, l.a.timeNow())
			clearRecords(current, rp.Party)
			report := fmt.Sprintf("%s merged at %s; no post-merge work declared; open items: none", act.URL, l.merged[act.Ref].MergeCommit)
			task := cmp.Or(rp.Serves, act.Ref)
			current.WorkerReports = append(current.WorkerReports, state.WorkerReport{Finished: true, By: rp.Party, At: l.a.timeNow(), Task: task, Text: report})
			return []state.Event{event(rp.Party, superloop.VerbDone, "%s done: %s", rp.Agent, task), event(rp.Party, superloop.VerbReport, "%s: %s", rp.Agent, report)}, nil
		})
		return "merged; task closed without waking worker", err
	}
	ag := st.Agents[i]
	if err := l.claim(act, ag.Name); err != nil {
		return "", err
	}
	defer func() {
		_ = l.a.store.Update(func(current *state.State) ([]state.Event, error) {
			current.ReviewParks = slices.DeleteFunc(current.ReviewParks, func(p state.ReviewPark) bool { return strings.EqualFold(p.On, act.Ref) })
			return nil, nil
		})
	}()
	if act.Message == strings.ToLower(superloop.Closed) {
		serves := cmp.Or(loopServes(st, ag), loopTaskServes(ag, act.Ref))
		if strings.TrimSpace(*ag.Park.PostMerge) == "" && (serves == "" || strings.EqualFold(serves, act.Ref) || !l.open(ctx, serves)) {
			err := l.a.store.Update(func(current *state.State) ([]state.Event, error) {
				i := slices.IndexFunc(current.Agents, func(other state.Agent) bool { return other.Is(ag.Party) })
				if i < 0 || current.Agents[i].Done {
					return nil, nil
				}
				worker := &current.Agents[i]
				reportIdle(worker, time.Now())
				worker.Done = true
				endNotes(current, worker.Party, l.a.timeNow())
				clearRecords(current, worker.Party)
				return []state.Event{event(worker.Party, superloop.VerbDone, "%s done: %s", worker.Name, worker.LastTask)}, nil
			})
			return "closed; task already settled", err
		}
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
	if l.otherReviewParks(ag.Party, act.Ref) {
		return "merged; other pull requests still await review or merge", nil
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
	if err := l.moveServedDone(ctx, serves, act.Ref); err != nil {
		return "", err
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
		endNotes(current, worker.Party, l.a.timeNow())
		clearRecords(current, worker.Party)
		current.WorkerReports = append(current.WorkerReports, state.WorkerReport{Finished: true, By: worker.Party, At: l.a.timeNow(), Task: worker.LastTask, Text: report})
		return []state.Event{event(worker.Party, superloop.VerbDone, "%s done: %s", worker.Name, worker.LastTask), event(worker.Party, superloop.VerbReport, "%s: %s", worker.Name, report)}, nil
	})
	return "merged; task closed without waking worker", err
}

func (l *supervisorLoop) endAbsorbed(ctx context.Context, st *state.State, act superloop.Action, by string) (string, error) {
	i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool {
		return ag.Park != nil && strings.EqualFold(ag.Park.On, act.Ref) && ag.Park.ReviewHead != "" && ag.Park.PostMerge != nil
	})
	rp, hasRP := st.ReviewParkFor(act.Ref)
	var (
		targetAgent string
		targetParty state.Party
		postMerge   string
		serves      string
	)
	l.mu.Lock()
	byPull := l.merged[by]
	l.mu.Unlock()
	if byPull.Ref == "" {
		readBy, err := l.readPull(ctx, by)
		if err != nil {
			return "", err
		}
		byPull = readBy
	}
	byPull.Ref = cmp.Or(byPull.Ref, by)
	if !slices.ContainsFunc(byPull.Absorbs, func(a string) bool { return strings.EqualFold(a, act.Ref) }) {
		byPull.Absorbs = append(byPull.Absorbs, act.Ref)
	}
	closed, closeErr := closeMergedPullIssues(ctx, st, byPull)
	l.recordClosedPullIssues(closed)
	if closeErr != nil {
		return "", closeErr
	}
	switch {
	case i >= 0:
		ag := st.Agents[i]
		targetAgent = ag.Name
		targetParty = ag.Party
		postMerge = strings.TrimSpace(*ag.Park.PostMerge)
		serves = cmp.Or(loopServes(st, ag), loopTaskServes(ag, act.Ref))
	case hasRP && rp.PostMerge != nil:
		targetAgent = rp.Agent
		targetParty = rp.Party
		postMerge = strings.TrimSpace(*rp.PostMerge)
		serves = rp.Serves
	default:
		_ = l.a.store.Update(func(current *state.State) ([]state.Event, error) {
			current.ReviewParks = slices.DeleteFunc(current.ReviewParks, func(p state.ReviewPark) bool { return strings.EqualFold(p.On, act.Ref) })
			return nil, nil
		})
		return fmt.Sprintf("%s %s: tracking ends", act.URL, act.Message), nil
	}
	if err := l.claim(act, targetAgent); err != nil {
		return "", err
	}
	isRescue := strings.EqualFold(targetAgent, superloop.PullWorkerName(act.Ref))
	serveOpen := serves != "" && !strings.EqualFold(serves, act.Ref) && l.open(ctx, serves)
	reconciledToCombining := false
	err := l.a.store.Update(func(current *state.State) ([]state.Event, error) {
		mergePostMerge := func(existing *string, add string) *string {
			cur := ""
			if existing != nil {
				cur = strings.TrimSpace(*existing)
			}
			merged := add
			if cur != "" && !strings.Contains(cur, add) {
				merged = cur + "; " + add
			} else if cur != "" {
				merged = cur
			}
			return &merged
		}
		for j := range current.ReviewParks {
			if strings.EqualFold(current.ReviewParks[j].On, by) {
				reconciledToCombining = true
				if postMerge != "" {
					current.ReviewParks[j].PostMerge = mergePostMerge(current.ReviewParks[j].PostMerge, postMerge)
				}
				if current.ReviewParks[j].Serves == "" && serves != "" && !strings.EqualFold(serves, act.Ref) {
					current.ReviewParks[j].Serves = serves
				}
			}
		}
		for j := range current.Agents {
			ag := &current.Agents[j]
			if ag.Park != nil && strings.EqualFold(ag.Park.On, by) && ag.Park.ReviewHead != "" {
				reconciledToCombining = true
				if postMerge != "" {
					ag.Park.PostMerge = mergePostMerge(ag.Park.PostMerge, postMerge)
				}
			}
		}
		current.ReviewParks = slices.DeleteFunc(current.ReviewParks, func(p state.ReviewPark) bool {
			return strings.EqualFold(p.On, act.Ref)
		})
		if isRescue || reconciledToCombining || (postMerge == "" && !serveOpen) {
			for j := range current.Agents {
				worker := &current.Agents[j]
				if worker.Is(targetParty) && (isRescue || (worker.Park != nil && worker.Park.ReviewHead != "")) {
					reportIdle(worker, l.a.timeNow())
					worker.Done = true
				}
			}
			endNotes(current, targetParty, l.a.timeNow())
			clearRecords(current, targetParty)
		}
		return nil, nil
	})
	if err != nil {
		return "", err
	}
	if !isRescue && !reconciledToCombining {
		origURL := cmp.Or(act.URL, pullRefURL(act.Ref))
		byURL := pullRefURL(by)
		if postMerge != "" {
			act.Ref = by
			act.URL = byURL
			act.Agent = targetAgent
			act.Message = fmt.Sprintf("Combining pull request %s merged and absorbed %s. Complete the remaining post-merge work: %s. Report the version, proof, live facts and open items with agents idle --done --report and --problem.", byURL, origURL, postMerge)
			if err := l.wakeOrStartFixer(ctx, st, act); err != nil {
				return "", err
			}
			return fmt.Sprintf("absorbed by %s; post-merge work reconciled with combining pull request", by), nil
		}
		if serveOpen {
			act.Ref = by
			act.URL = byURL
			act.Agent = targetAgent
			act.Message = fmt.Sprintf("Combining pull request %s merged and absorbed %s. The task at %s is still open or its closure could not be confirmed. Continue its remaining work, or verify it is complete and report done.", byURL, origURL, issueURL(serves))
			if err := l.wakeOrStartFixer(ctx, st, act); err != nil {
				return "", err
			}
			return fmt.Sprintf("absorbed by %s; worker woken for remaining open work", by), nil
		}
	}
	return fmt.Sprintf("absorbed by %s; rescue task withdrawn", by), nil
}

func pullRefURL(ref string) string {
	if r, ok := parseRef(ref); ok {
		return fmt.Sprintf("https://github.com/%s/pull/%d", r.Repo, r.N)
	}
	return ref
}

var closePullIssue = func(ctx context.Context, issue github.PR, pullURL string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	_, err := github.RunGH(github.ActingOn(ctx, issue.Repo), "issue", "close", strconv.Itoa(issue.N), "--repo", issue.Repo, "--reason", "completed", "--comment", fmt.Sprintf("Closed by %s.", pullURL))
	return err
}

var editPullBody = func(ctx context.Context, pull github.PR, body string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	_, err := github.RunGH(github.ActingOn(ctx, pull.Repo), "pr", "edit", strconv.Itoa(pull.N), "--repo", pull.Repo, "--body", body)
	return err
}

func (l *supervisorLoop) recordClosedPullIssues(closed []string) {
	if l == nil || len(closed) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.opens == nil {
		l.opens = map[string]loopOpen{}
	}
	now := l.a.timeNow()
	for _, ref := range closed {
		l.opens[ref] = loopOpen{open: false, at: now}
	}
}

func absorbedClosingIssues(ctx context.Context, st *state.State, absorbs []string) ([]string, error) {
	var out []string
	add := func(raw string) {
		r, ok := parseRef(raw)
		if !ok {
			return
		}
		name := refName(r)
		if !slices.ContainsFunc(out, func(e string) bool { return strings.EqualFold(e, name) }) {
			out = append(out, name)
		}
	}
	for _, orig := range absorbs {
		if st != nil {
			if rp, ok := st.ReviewParkFor(orig); ok && rp.Serves != "" && !strings.EqualFold(rp.Serves, orig) {
				add(rp.Serves)
			}
			for _, ag := range st.Agents {
				if ag.Park != nil && strings.EqualFold(ag.Park.On, orig) {
					if s := cmp.Or(loopServes(st, ag), loopTaskServes(ag, orig)); s != "" && !strings.EqualFold(s, orig) {
						add(s)
					}
				}
			}
		}
		origPull, err := readLoopContextPull(ctx, orig)
		if err != nil {
			return nil, err
		}
		for _, c := range origPull.Closes {
			add(c)
		}
		for _, c := range superloop.ParseClosingIssues(orig, origPull.Title+"\n"+origPull.Body) {
			add(c)
		}
	}
	return out, nil
}

func ensureCombinedPullCloses(ctx context.Context, st *state.State, p *superloop.Pull) error {
	if p == nil || len(p.Absorbs) == 0 {
		return nil
	}
	absorbed, err := absorbedClosingIssues(ctx, st, p.Absorbs)
	if err != nil {
		return err
	}
	for _, issue := range absorbed {
		if !slices.ContainsFunc(p.Closes, func(e string) bool { return strings.EqualFold(e, issue) }) {
			p.Closes = append(p.Closes, issue)
		}
	}
	if p.State != superloop.Open || len(p.Closes) == 0 {
		return nil
	}
	updated := superloop.AppendClosingKeywords(p.Ref, p.Body, p.Closes)
	if updated == p.Body {
		return nil
	}
	pr, ok := parseRef(p.Ref)
	if !ok {
		return nil
	}
	if err := editPullBody(ctx, pr, updated); err != nil {
		return err
	}
	p.Body = updated
	return nil
}

func closeMergedPullIssues(ctx context.Context, st *state.State, p superloop.Pull) ([]string, error) {
	var candidates []string
	add := func(raw string) {
		r, ok := parseRef(raw)
		if !ok || strings.EqualFold(refName(r), p.Ref) {
			return
		}
		name := refName(r)
		if !slices.ContainsFunc(candidates, func(e string) bool { return strings.EqualFold(e, name) }) {
			candidates = append(candidates, name)
		}
	}
	for _, c := range p.Closes {
		add(c)
	}
	for _, c := range superloop.ParseClosingIssues(p.Ref, p.Title+"\n"+p.Body) {
		add(c)
	}
	absorbed, err := absorbedClosingIssues(ctx, st, p.Absorbs)
	if err != nil {
		return nil, err
	}
	for _, c := range absorbed {
		add(c)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	var refs []github.PR
	for _, c := range candidates {
		if r, ok := parseRef(c); ok {
			refs = append(refs, r)
		}
	}
	states, err := refStates(ctx, refs)
	if err != nil {
		return nil, err
	}
	pullURL := cmp.Or(p.URL, pullRefURL(p.Ref))
	var closed []string
	for _, r := range refs {
		s, ok := states[r]
		if !ok || s.Kind != github.RefIssue || (s.State != github.Open && s.State != github.Closed) {
			return closed, fmt.Errorf("issue closure for %s is unconfirmed", refName(r))
		}
		if s.State == github.Open {
			if err := closePullIssue(ctx, r, pullURL); err != nil {
				return closed, err
			}
		}
		closed = append(closed, refName(r))
	}
	return closed, nil
}

// moveServedDone moves the task ref served to Done. A saved Serves can name a
// pull request, which the board refuses as no issue: that must not fail the end.
func (l *supervisorLoop) moveServedDone(ctx context.Context, serves, ref string) error {
	if serves == "" || serves == ref || l.a.cfg.Board.Owner == "" {
		return nil
	}
	_, err := l.run(ctx, loopBoard, "move", serves, "Done")
	if err != nil && Code(err) == ExitRefused && strings.Contains(err.Error(), " is no issue:") {
		return nil
	}
	return err
}

// The reasons a reviewed merge stops for desk policy, before and after a
// branch update alike.
const (
	reasonDeskStopped      = "the desk is stopped"
	reasonMergeHeld        = "merge held by desk policy"
	reasonMergeHeadChanged = "merge head moved or current CI is not green"
	mergeableConflicting   = "CONFLICTING"
	mergeMethodRebase      = "rebase"
	mergeMethodSquash      = "squash"
)

// reviewedMerge merges the reviewed green head as it stands. A behind head
// must pass the merge-result check against the current base. Repository
// protection may refuse it, but the gate never updates the branch.
func (a *app) reviewedMerge(ctx context.Context, repo string, number int, head string) github.SquashDoc {
	doc := github.SquashDoc{Repo: repo, Number: number, HeadSha: head, SchemaVersion: 1, Command: "pr merge", Route: reviewedMergeRoute, ExitCode: github.SquashNotApplicable}
	method := a.cfg.Merge.Methods[repo]
	if method == "" {
		method = mergeMethodSquash
	}
	if method != mergeMethodSquash && method != mergeMethodRebase && method != superloop.KindMerge {
		doc.ExitCode = github.SquashTooling
		doc.Reason = "invalid configured merge method"
		return doc
	}
	ref := fmt.Sprintf("%s#%d", repo, number)
	p, err := readLoopPull(ctx, ref)
	if err != nil {
		doc.ExitCode = github.SquashTooling
		doc.Reason = err.Error()
		return doc
	}
	if p.State == superloop.Merged && p.Head == head {
		st, _ := a.store.Read()
		if _, err := closeMergedPullIssues(ctx, st, p); err != nil {
			doc.ExitCode = github.SquashTooling
			doc.Reason = err.Error()
			return doc
		}
		doc.ExitCode = 0
		doc.MergeCommitSha = p.MergeCommit
		return doc
	}
	if p.State != superloop.Open || p.Head != head || !p.Green {
		doc.ExitCode = github.SquashRefused
		doc.Reason = reasonMergeHeadChanged
		return doc
	}
	if latestReviewRequestsChanges(p) {
		doc.ExitCode = github.SquashRefused
		doc.Reason = "a review requested changes; new supervisor approval required"
		return doc
	}
	if p.Draft || p.Mergeable == mergeableConflicting {
		doc.Reason = "branch is draft or conflicts with its base"
		return doc
	}
	st, err := a.store.Read()
	if err != nil {
		doc.ExitCode = github.SquashTooling
		doc.Reason = err.Error()
		return doc
	}
	evs, logErr := a.store.Events(0, func(e state.Event) bool { return e.By == loopParty && e.Subject == ref })
	if logErr != nil {
		doc.ExitCode = github.SquashTooling
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
		key = superloop.BaseActionKey(key)
		// The sweep's go again for a head whose go was never carried out
		// is the same authority, keyed by its stretch without an owner.
		if key == "go "+ref+"@"+head || superloop.IsSweep(key) && strings.HasPrefix(key, "go "+ref+"@"+head+" ") {
			if e.Verb == loopClaimVerb {
				claimed = true
				claimAgent = rest
			}
			if e.Verb == loopMergeVerb || e.Verb == loopGoVerb {
				claimed = false
			}
		}
		if e.Verb == loopEndVerb && key == "end "+ref {
			claimed = false
		}
	}
	if !claimed {
		doc.ExitCode = github.SquashTooling
		doc.Reason = "no pending exact-head supervisor merge claim"
		return doc
	}
	var execRef *execution.Ref
	for _, worker := range st.Agents {
		if worker.Name == claimAgent && worker.Execution != nil {
			execRef = worker.Execution
			break
		}
	}
	if execRef == nil {
		if rp, ok := st.ReviewParkFor(ref); ok && rp.Execution != nil {
			execRef = rp.Execution
		}
	}
	if execRef == nil {
		for i := len(st.Starts) - 1; i >= 0; i-- {
			if st.Starts[i].Name == claimAgent && st.Starts[i].Execution != nil {
				execRef = &st.Starts[i].Execution.Handle.Ref
				break
			}
		}
	}
	if execRef != nil {
		use := execution.MergeUse{Ref: *execRef, Repository: repo, PR: number, Head: head}
		record, _ := executionRecord(st, *execRef)
		if record == nil {
			doc.ExitCode = github.SquashRefused
			doc.Reason = "execution stopped"
			return doc
		}
		if err := a.authenticateReviewResult(ctx, st, use, p, true); err != nil {
			doc.ExitCode = github.SquashRefused
			doc.Reason = err.Error()
			return doc
		}
	}
	if claimAgent != "" {
		doc.RequestedBy = fmt.Sprintf("%s (for %s)", loopParty.Name, claimAgent)
	} else if a.as != "" {
		doc.RequestedBy = a.as
	} else {
		doc.RequestedBy = loopParty.Name
	}
	if why, _, authErr := a.checkMergeHeadAuthority(ctx, st, p, head); authErr != nil {
		doc.ExitCode = github.SquashTooling
		doc.Reason = authErr.Error()
		return doc
	} else if why != "" {
		doc.ExitCode = github.SquashRefused
		doc.Reason = why
		return doc
	}
	if st.Stop != nil {
		doc.ExitCode = github.SquashRefused
		doc.Reason = reasonDeskStopped
		return doc
	}
	if _, held := merge.BlockingFor(st, time.Now(), loopParty.Name, repo, number, a.cfg.LaneOf(repo)); held {
		doc.ExitCode = github.SquashRefused
		doc.Reason = reasonMergeHeld
		return doc
	}
	merging := head
	reason, code := a.checkReviewedMergeResult(ctx, p, head)
	if code != 0 {
		doc.ExitCode, doc.Reason = code, reason
		return doc
	}
	note := reason
	queue, err := readReviewedMergeQueue(ctx, p)
	if err != nil {
		doc.ExitCode, doc.Reason = github.SquashTooling, err.Error()
		return doc
	}
	// Validation waits can outlive the review that admitted
	// this run. Refresh authority after all checks, immediately before the
	// mutation; --match-head-commit alone cannot detect a withdrawn approval.
	p, err = readLoopPull(ctx, ref)
	if err != nil {
		doc.ExitCode, doc.Reason = github.SquashTooling, err.Error()
		return doc
	}
	if p.State != superloop.Open || p.Head != merging || !p.Green {
		doc.ExitCode, doc.Reason = github.SquashRefused, reasonMergeHeadChanged
		return doc
	}
	if p.Draft || p.Mergeable == mergeableConflicting {
		doc.ExitCode, doc.Reason = github.SquashRefused, "branch became draft or conflicts with its base"
		return doc
	}
	st, err = a.store.Read()
	if err != nil {
		doc.ExitCode, doc.Reason = github.SquashTooling, err.Error()
		return doc
	}
	if why, _, authErr := a.checkMergeHeadAuthority(ctx, st, p, merging); authErr != nil {
		doc.ExitCode, doc.Reason = github.SquashTooling, authErr.Error()
		return doc
	} else if why != "" {
		doc.ExitCode, doc.Reason = github.SquashRefused, why
		return doc
	}
	if st.Stop != nil {
		doc.ExitCode, doc.Reason = github.SquashRefused, reasonDeskStopped
		return doc
	}
	if _, held := merge.BlockingFor(st, time.Now(), loopParty.Name, repo, number, a.cfg.LaneOf(repo)); held {
		doc.ExitCode, doc.Reason = github.SquashRefused, reasonMergeHeld
		return doc
	}
	if err := ensureCombinedPullCloses(ctx, st, &p); err != nil {
		doc.ExitCode, doc.Reason = github.SquashTooling, err.Error()
		return doc
	}
	argv := []string{"gh", "pr", superloop.KindMerge, fmt.Sprint(number), "--repo", repo, "--" + method, "--delete-branch", "--match-head-commit", merging}
	// A merge queue retests the merge group when the base advances; --admin
	// would bypass it. Without a queue the lane's own merge-result check
	// above is the validation, and the desk's admin merge passes the base's
	// review requirement as before the check.
	if !queue && a.cfg.Merge.AdminMerge(repo) {
		argv = append(argv, "--admin")
	}
	// Use the configured owner's devctl identity without letting pr merge
	// follow a new head during its wait. All gates above still govern this
	// exact-head mutation; no extra admin or auto flag is granted.
	if a.cfg.Merge.DevctlServes(repo) {
		argv = append([]string{devctlBinary, "auth", "exec", "--"}, argv...)
	}
	if a.cfg != nil {
		github.SetDevctlSkipVersionCheck(!a.cfg.Hooks.GuardsEnabled())
	}
	out, stderr, code, runErr := loopCommand(ctx, "", argv)
	doc.Reason = oneLine(out + stderr)
	if runErr != nil {
		doc.Reason = runErr.Error()
	}
	if note != "" {
		doc.Reason = strings.TrimSuffix(note+"; "+doc.Reason, "; ")
	}
	p, err = readLoopPull(ctx, ref)
	if err != nil {
		doc.ExitCode = github.SquashTooling
		doc.Reason = err.Error()
		return doc
	}
	switch {
	case p.State == superloop.Merged && p.Head == merging:
		if _, err := closeMergedPullIssues(ctx, st, p); err != nil {
			doc.ExitCode = github.SquashTooling
			doc.Reason = err.Error()
			return doc
		}
		doc.ExitCode = 0
		doc.MergeCommitSha = p.MergeCommit
		doc.Verdict = "merged"
	case runErr == nil && code == 0 && p.State == superloop.Open && p.Head == merging:
		doc.ExitCode, doc.Verdict = 0, reviewedMergeQueued
		doc.Reason = "merge queued; awaiting server-side merge-result checks"
	case p.State != superloop.Open || p.Head != merging || !p.Green:
		doc.ExitCode = github.SquashRefused
		if doc.Reason == "" {
			doc.Reason = reasonMergeHeadChanged
		}
	case ghMergePolicyRefused(doc.Reason):
		doc.ExitCode = github.SquashRefused
		doc.Reason = fmt.Sprintf("repository merge policy refused; branch updates are disabled; the repository owner must resolve any up-to-date requirement (reviewDecision=%s): %s", p.ReviewDecision, doc.Reason)
	case pullNeedsBranchRepair(p) || runErr == nil && code != 0 && ghMergeNeedsBranchRepair(doc.Reason):
		doc.ExitCode = github.SquashNotApplicable
		if doc.Reason == "" {
			doc.Reason = "branch is draft or conflicts with its base"
		}
	default:
		doc.ExitCode = github.SquashTooling
		if code == 0 && runErr == nil && doc.Reason == "" {
			doc.Reason = "merge not confirmed"
		}
	}
	return doc
}

// readReviewedBehind reads how many commits of its base p's head lacks.
// Emergency authority checks use this independently of the merge lane.
var readReviewedBehind = func(ctx context.Context, p superloop.Pull) (int, error) {
	if p.Base == "" {
		return 0, nil
	}
	ref, ok := parseRef(p.Ref)
	if !ok {
		return 0, fmt.Errorf("invalid pull request")
	}
	out, err := github.RunGH(github.ActingOn(ctx, ref.Repo), "api", "repos/"+ref.Repo+"/compare/"+url.PathEscape(p.Base)+"..."+url.PathEscape(p.Head), "--jq", ".behind_by")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// reviewOvertookDecision prevents a fresh exact-head authorization from
// concealing a change request that arrived after the planner's review snapshot.
func reviewOvertookDecision(p superloop.Pull, act superloop.Action) bool {
	var latest *superloop.Review
	for i := range p.Reviews {
		r := &p.Reviews[i]
		if r.Verdict != "" && (latest == nil || !r.At.Before(latest.At)) {
			latest = r
		}
	}
	return latest != nil && latest.Verdict == superloop.ChangesRequested &&
		(latest.ID != act.ReviewID || act.ReviewVerdict != superloop.ChangesRequested)
}

// latestReviewRequestsChanges reports whether the latest review verdict on p
// requests changes on its current head, overriding any earlier approval.
// A different head still needs the loop's exact-head decision; the executor's
// authority check rejects any go overtaken by a later change request.
func latestReviewRequestsChanges(p superloop.Pull) bool {
	var latest superloop.Review
	var found bool
	for _, r := range p.Reviews {
		if r.Verdict == "" {
			continue
		}
		if !found || r.At.IsZero() || latest.At.IsZero() || !r.At.Before(latest.At) {
			latest = r
			found = true
		}
	}
	return found && latest.Verdict == superloop.ChangesRequested && sameCommit(cmp.Or(latest.Head, latest.Commit), p.Head)
}

// checkMergeHeadAuthority verifies that merging p at targetHead is backed by
// an approved desk review covering that head (or a base merge into it) or an
// explicit exact-head supervisor go: the loop's own go for that head
// (supervisor.go), merge.authorized,
// or execution.merge-issued. A verdict counts
// only from a login the desk trusts (trustedReview): on a public repository
// anyone can post "Review: approved".
func (a *app) checkMergeHeadAuthority(ctx context.Context, st *state.State, p superloop.Pull, targetHead string) (why string, hasExplicitGo bool, err error) {
	if ref, ok := parseRef(p.Ref); ok {
		if err := a.fatalPublicationBarrier(ctx, st, ref.Repo, ref.N); err != nil {
			return err.Error(), false, nil
		}
	}
	head := cmp.Or(targetHead, p.Head)
	if head == "" {
		return "pull request has no head commit", false, nil
	}
	if p.Head != "" && targetHead != "" && !sameCommit(p.Head, targetHead) {
		return fmt.Sprintf("pull request head is %s, not %s", shortSHA(p.Head), shortSHA(targetHead)), false, nil
	}
	reviews, err := a.trustedReviews(ctx, p)
	if err != nil {
		return "", false, err
	}
	var latest *superloop.Review
	for i := range reviews {
		r := &reviews[i]
		if latest == nil || !r.At.Before(latest.At) {
			latest = r
		}
	}
	if a != nil && a.cfg != nil && a.cfg.Merge.RequireReview {
		first := ""
		if latest != nil {
			first, _, _ = strings.Cut(strings.TrimSpace(latest.Body), "\n")
		}
		if first != deskReviewApproved && first != "Review: approved" {
			return "merge.requireReview requires an approved desk review comment on the head", false, nil
		}
	}
	approvedHead := ""
	if latest != nil && latest.Verdict == superloop.Approved {
		approvedHead = cmp.Or(latest.Head, latest.Commit)
		if sameCommit(approvedHead, head) || st.ReviewNitsChecked(p.Ref, head, approvedHead) {
			return "", false, nil
		}
	}

	// A base-only update preserves the reviewed change even when review is
	// mandatory. Check it before refusing an approval on an older head.
	if approvedHead != "" && a != nil && a.cfg != nil && a.cfg.Merge.RequireReview {
		ok, err := readBaseMerge(ctx, p, approvedHead)
		if err != nil {
			return "", false, err
		}
		if ok {
			return "", false, nil
		}
	}

	if a != nil && a.cfg != nil && a.cfg.Merge.RequireReview {
		return fmt.Sprintf("head %s requires an approved independent review; an exact-head go alone is insufficient", shortSHA(head)), false, nil
	}

	type goRecord struct {
		head string
		at   time.Time
	}
	var goes []goRecord
	if a != nil && a.store != nil {
		// execution.merge-issued is logged without a subject: its detail
		// names the pull request's URL.
		issued := ""
		if r, ok := parseRef(p.Ref); ok {
			issued = strings.ToLower(fmt.Sprintf(" for https://github.com/%s/pull/%d at ", r.Repo, r.N))
		}
		forPull := func(e state.Event) bool {
			if e.Verb == "execution.merge-issued" {
				return issued != "" && strings.Contains(strings.ToLower(e.Detail), issued)
			}
			return strings.EqualFold(e.Subject, p.Ref)
		}
		evs, err := a.store.Events(0, func(e state.Event) bool {
			return forPull(e)
		})
		if err != nil {
			return "", false, err
		}
		activeGo := map[string]goRecord{}
		for _, e := range evs {
			if e.By == loopParty && strings.EqualFold(e.Subject, p.Ref) {
				if key, _, ok := keyed(e.Detail); ok {
					key = superloop.BaseActionKey(key)
					if after, ok := strings.CutPrefix(key, "go "+p.Ref+"@"); ok && after != "" {
						switch e.Verb {
						case loopGoVerb:
							activeGo[after] = goRecord{head: after, at: e.At}
						case loopMergeVerb, loopClaimEndVerb:
							delete(activeGo, after)
						}
					}
					if e.Verb == loopEndVerb && strings.EqualFold(key, "end "+p.Ref) {
						clear(activeGo)
					}
				}
			}
			if (e.Verb == "merge.authorized" || e.Verb == "execution.merge-issued") && forPull(e) {
				if h := headAfterAt(e.Detail); h != "" {
					goes = append(goes, goRecord{head: h, at: e.At})
				}
			}
		}
		for _, g := range activeGo {
			goes = append(goes, g)
		}
	}
	goes = slices.DeleteFunc(goes, func(g goRecord) bool {
		return g.head == "" || slices.ContainsFunc(reviews, func(r superloop.Review) bool {
			if r.Verdict != superloop.ChangesRequested {
				return false
			}
			revHead := cmp.Or(r.Head, r.Commit)
			return sameCommit(revHead, head) || (!g.at.IsZero() && !r.At.Before(g.at))
		})
	})
	// An exact head first: only then are base merges read, one GitHub
	// compare each.
	if slices.ContainsFunc(goes, func(g goRecord) bool { return sameCommit(g.head, head) }) {
		return "", true, nil
	}
	if approvedHead != "" {
		ok, err := readBaseMerge(ctx, p, approvedHead)
		if err != nil {
			return "", false, err
		}
		if ok {
			return "", false, nil
		}
	}
	for _, g := range goes {
		ok, err := readBaseMerge(ctx, p, g.head)
		if err != nil {
			return "", false, err
		}
		if ok {
			return "", true, nil
		}
	}

	if latest != nil && latest.Verdict == superloop.ChangesRequested {
		return fmt.Sprintf("head %s has no approved desk review (latest desk review on %s requested changes) and no supervisor go for that exact head", shortSHA(head), shortSHA(cmp.Or(latest.Head, latest.Commit))), false, nil
	}
	if latest != nil && latest.Verdict == superloop.Approved {
		return fmt.Sprintf("head %s moved past approved head %s without a new approved desk review or supervisor go for %s", shortSHA(head), shortSHA(cmp.Or(latest.Head, latest.Commit)), shortSHA(head)), false, nil
	}
	return fmt.Sprintf("head %s has no approved desk review and no supervisor go for that exact head", shortSHA(head)), false, nil
}

// trustedReviews are p's reviews with a verdict the desk trusts
// (deskVerdicts): the authors are supervisor.ownership.authors, else the
// login gh acts as, read only when a verdict comes from a non-member.
func (a *app) trustedReviews(ctx context.Context, p superloop.Pull) ([]superloop.Review, error) {
	var authors []string
	if a != nil && a.cfg != nil {
		authors = a.cfg.Supervisor.Ownership.Authors
	}
	if len(authors) == 0 && slices.ContainsFunc(p.Reviews, func(r superloop.Review) bool { return r.Verdict != "" && !r.Member }) {
		login, err := readDeskLogin(ctx)
		if err != nil {
			return nil, err
		}
		authors = []string{login}
	}
	var out []superloop.Review
	for _, r := range deskVerdicts(p.Reviews, authors) {
		if r.Verdict != "" {
			out = append(out, r)
		}
	}
	return out, nil
}

// deskVerdicts is reviews with the verdict of each one the desk does not
// trust cleared: a verdict counts only from a member or collaborator of the
// repository's owner or from one of authors, the logins the desk opens and
// reviews its pull requests as. On a public repository anyone can post
// "Review: approved": the planner and the merge gate both read verdicts
// through this, so neither merges nor holds on an outsider's word.
func deskVerdicts(reviews []superloop.Review, authors []string) []superloop.Review {
	out := slices.Clone(reviews)
	for i := range out {
		if !out[i].Member && !board.ContainsFold(authors, out[i].Author) {
			out[i].Verdict = ""
		}
	}
	return out
}

// gateMergeHeadAuthority checks merge head authority for a gated pull request
// merge across all gate routes.
func gateMergeHeadAuthority(ctx context.Context, a *app, repo string, pr int, targetHead string) (why string, hasExplicitGo bool, err error) {
	if pr <= 0 {
		return "", false, nil
	}
	ref := fmt.Sprintf("%s#%d", repo, pr)
	p, err := readLoopPull(ctx, ref)
	if err != nil {
		return "", false, err
	}
	var st *state.State
	if a != nil && a.store != nil {
		st, err = a.store.Read()
		if err != nil {
			return "", false, err
		}
	}
	return a.checkMergeHeadAuthority(ctx, st, p, targetHead)
}

// headAfterAt is the commit an exact-head go names right after " at "
// (optionally " at head "): execution.merge-issued's "<grant> for <url> at
// <head>", merge.authorized's "<url> at <head>: …" and an escalation's
// "Merge <url> at head <head> …". "" when no 7 to 40 hex digits stand
// there: a grant id or timestamp elsewhere in the text is never a head.
func headAfterAt(text string) string {
	_, rest, ok := strings.Cut(text, " at ")
	if !ok {
		return ""
	}
	rest = strings.TrimPrefix(rest, "head ")
	n := 0
	for n < len(rest) && isHexDigit(rest[n]) {
		n++
	}
	if n < 7 || n > 40 || n < len(rest) && (rest[n] == '-' || rest[n] == '_' || 'a' <= rest[n]|0x20 && rest[n]|0x20 <= 'z') {
		return ""
	}
	return strings.ToLower(rest[:n])
}

func isHexDigit(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func sameCommit(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// readReviewedMergeQueue reads whether p's base branch has a GitHub merge
// queue. With one, the queue retests the merge group if any writer advances
// the base after the lane's local check; without one, the lane's check
// stands. Only an unreadable answer refuses.
var readReviewedMergeQueue = func(ctx context.Context, p superloop.Pull) (bool, error) {
	ref, ok := parseRef(p.Ref)
	if !ok || p.Base == "" {
		return false, fmt.Errorf("merge queue: invalid base")
	}
	ctx = github.ActingOn(ctx, ref.Repo)
	owner, name, _ := strings.Cut(ref.Repo, "/")
	out, err := github.RunGH(ctx, "api", "graphql", "-f", "query=query($owner:String!,$name:String!,$branch:String!){repository(owner:$owner,name:$name){mergeQueue(branch:$branch){id}}}", "-f", "owner="+owner, "-f", "name="+name, "-f", "branch="+p.Base)
	if err != nil {
		if queue, restErr := readReviewedMergeQueueREST(ctx, ref.Repo, p.Base); restErr == nil {
			return queue, nil
		}
		return false, fmt.Errorf("merge queue unavailable: %w", err)
	}
	var result struct {
		Data struct {
			Repository *struct{ MergeQueue *struct{ ID string } }
		}
		Errors []json.RawMessage
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return false, fmt.Errorf("reading merge queue: %w", err)
	}
	if len(result.Errors) != 0 || result.Data.Repository == nil {
		if strings.Contains(string(out), "Could not resolve to a Repository") {
			if queue, restErr := readReviewedMergeQueueREST(ctx, ref.Repo, p.Base); restErr == nil {
				return queue, nil
			}
		}
		return false, fmt.Errorf("reading merge queue of %s: %s", ref.Repo, oneLine(string(out)))
	}
	return result.Data.Repository.MergeQueue != nil && result.Data.Repository.MergeQueue.ID != "", nil
}

// readReviewedMergeQueueREST reads whether branch of repo has a merge queue
// through the REST API when GraphQL cannot resolve the repository (for
// example, a private user-owned repository with a token that reaches REST
// only). Branch rules name a merge_queue rule where one is active; a
// user-owned repository has no merge queue on GitHub.
func readReviewedMergeQueueREST(ctx context.Context, repo, branch string) (bool, error) {
	if out, err := github.RunGH(ctx, "api", "repos/"+repo+"/rules/branches/"+url.PathEscape(branch)); err == nil {
		var rules []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(out, &rules) == nil {
			return slices.ContainsFunc(rules, func(r struct {
				Type string `json:"type"`
			}) bool {
				return r.Type == "merge_queue"
			}), nil
		}
	}
	out, err := github.RunGH(ctx, "api", "repos/"+repo)
	if err != nil {
		return false, err
	}
	var meta struct {
		Owner struct {
			Type string `json:"type"`
		} `json:"owner"`
	}
	if err := json.Unmarshal(out, &meta); err != nil {
		return false, err
	}
	if strings.EqualFold(meta.Owner.Type, "User") {
		return false, nil
	}
	return false, fmt.Errorf("owner type %q may have a merge queue", meta.Owner.Type)
}
