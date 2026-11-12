package cmd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const LoopWorkerMergeGo = "merge go given"

const ReviewedMergeQueued = "queued"

const LoopMergeFailed = "merge.failed"

const LoopMergeFailureDropped = "merge.failure-dropped"

func PullNeedsBranchRepair(p superloop.Pull) bool {
	return p.Draft || p.Mergeable == "CONFLICTING"
}

// A branch policy refusal belongs to the coordinator, even when GitHub's
// merge state also says BEHIND. A branch edit cannot supply missing review.
func GhMergePolicyRefused(reason string) bool {
	lower := strings.ToLower(reason)
	return strings.Contains(lower, "behind") || strings.Contains(lower, "up to date") || strings.Contains(lower, "out of date") || strings.Contains(lower, "out-of-date") || strings.Contains(lower, "base branch policy") || strings.Contains(lower, "review_required") || strings.Contains(lower, "required approving review")
}

// MergePull executes the planner's exact-head decision in the coordinator,
// including for workers on an execution host. Failed or unreadable validation
// never delegates a direct merge around the required server-side queue.
func (l *Loop) MergePull(ctx context.Context, _ *state.State, act superloop.Action) (result string, resultErr error) {
	a := l.Host
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
	// the go, entering the gate or starting failure backoff. Every wait
	// names what it waits on: the stall alarm reads it (checkFlowWaits).
	waitKey := act.Key + " hold"
	if h, held := merge.BlockingFor(st, a.timeNow(), LoopParty.Name, ref.Repo, ref.N, a.cfg.LaneOf(ref.Repo)); held {
		l.SayOnce(waitKey, "SUPERVISOR WAIT %s on hold:%s", act.URL, h.Target)
		return "", MergeHeld(WaitHold, "it is held by hold:%s (beekeeper hold lists it); the merge starts once the hold is lifted", h.Target)
	}
	l.ClearSaid(waitKey)
	if BoardBudgetLow(st.Budget, a.cfg.GitHub.Floor, a.timeNow()) {
		return "", MergeHeld(waitBudget, "the GitHub budget is under github.floor (%d): no merge starts until its reset", a.cfg.GitHub.Floor)
	}
	if a.resetFirst(st) {
		return "", MergeHeld(waitBudget, "merges queued under github.floor for the GitHub budget's reset go first (merge.resetFirst)")
	}
	if err := a.fatalPublicationBarrier(ctx, st, ref.Repo, ref.N); err != nil {
		l.SayOnce(waitKey, "SUPERVISOR WAIT %s: %v", act.URL, err)
		return "", MergeHeld(waitOutage, "an outage of its repository's default branch holds it: %s", oneLine(err.Error()))
	}
	repairStage := act.Key + " repair"
	l.Mu.Lock()
	repairing, reason := l.Stages[repairStage], l.StageDetail[repairStage]
	l.Mu.Unlock()
	if repairing {
		return l.repairPull(ctx, st, act, reason)
	}
	p, err := ReadLoopPull(ctx, act.Ref)
	if err != nil {
		return "", namedWait(err, WaitUnread, "its pull request is unread")
	}
	if p.State == superloop.Merged && (p.Head == act.Head || l.baseMerged(ctx, p, act.Head)) {
		closed, closeErr := CloseMergedPullIssues(ctx, st, p)
		l.recordClosedPullIssues(closed)
		if closeErr != nil {
			return "", closeErr
		}
		return fmt.Sprintf("merged %s at %s", act.URL, p.Head), nil
	}
	if p.State != superloop.Open || p.Head != act.Head || !p.Green {
		return "", refused("merge head moved or current CI is not green")
	}
	if err := EnsureCombinedPullCloses(ctx, st, &p); err != nil {
		return "", err
	}
	if LatestReviewRequestsChanges(p) || reviewOvertookDecision(p, act) {
		return "", refused("a review requested changes; new supervisor approval required")
	}
	repair := func(code int, reason string) (string, error) {
		_, _ = stopMerges(a.store, LoopParty, func(m state.Merge) bool {
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
	if p.Draft || strings.EqualFold(p.Mergeable, MergeableConflicting) || strings.EqualFold(p.MergeState, github.DirtyState) {
		return repair(github.SquashNotApplicable, "branch is draft or conflicts with its base")
	}
	// A detached gate owns the queued/running mutation. Observe its result
	// on later passes; do not submit another command for the same PR.
	if i := slices.IndexFunc(st.Merges, func(m state.Merge) bool {
		return m.Repo == ref.Repo && m.PR == ref.N && (m.Phase == state.Waiting || m.Phase == state.Running) && merge.Runs(m, proc.Alive)
	}); i >= 0 {
		m := st.Merges[i]
		since := m.Joined
		if m.Phase == state.Running && !m.Started.IsZero() {
			since = m.Started
		}
		return "", MergeHeld(waitGate, "the merge gate's own run has it %s in lane %s since %s (pid %d; beekeeper lanes shows it): its outcome settles the decision", m.Phase, m.Lane, since.UTC().Format(time.RFC3339), m.PID)
	}
	for _, f := range st.Failed {
		if f.Repo != ref.Repo || f.PR != ref.N || f.By.Name != LoopParty.Name {
			continue
		}
		if f.Head != act.Head || StaleMergeResultFailure(f.Reason, act.Head) || GhMergePolicyRefused(f.Reason) || github.RateLimited(errors.New(f.Reason)) {
			// A different (or unknown legacy) head supplies no failure proof
			// for this approved commit. Let the gate validate it again.
			if err := a.store.Update(func(current *state.State) ([]state.Event, error) {
				merge.Retry(current, ref.Repo, ref.N)
				return nil, nil
			}); err != nil {
				return "", err
			}
			if f.Head != act.Head || StaleMergeResultFailure(f.Reason, act.Head) {
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
	if err := a.store.Record(about(event(LoopParty, "merge.authorized", "%s at %s: the supervisor loop authorizes the reviewed green fixes", act.URL, act.Head), act.Ref)); err != nil {
		return "", err
	}
	if err := a.store.Record(about(event(LoopParty, "merge.start", "merging %s at %s", act.URL, act.Head), act.Ref)); err != nil {
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
			verb, detail = LoopMergeFailed, failed
		case errors.Is(resultErr, ErrLoopLater):
			verb, detail = "merge.pending", pending
		case resultErr != nil:
			verb, detail = LoopMergeFailed, resultErr.Error()
		}
		if err := a.store.Record(about(event(LoopParty, verb, "%s at %s: %s", act.URL, act.Head, oneLine(detail)), act.Ref)); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	gateArgs := []string{gateCmdName, "--wait", "0s", "--limit", "0", "--reviewed-head", act.Head, "--", devctlBinary, "pr", superloop.KindMerge, ref.Repo, fmt.Sprint(ref.N)}
	out, code, err := BeekeeperRun(ctx, l.As(gateArgs...)...)
	if err != nil && ctx.Err() == nil {
		if saveErr := a.operationOutcome(runMerge, "merge-command-unavailable", act.URL, false); saveErr != nil {
			return "", saveErr
		}
	}
	// Read after every outcome, including a lost response: a restart never
	// repeats a mutation already observed merged.
	p, readErr := ReadLoopPull(ctx, act.Ref)
	if readErr != nil {
		return "", namedWait(readErr, WaitUnread, "its pull request is unread after the merge command")
	}
	l.CachePull(act.Ref, p)
	if p.State == superloop.Merged {
		// Where the base requires up-to-date branches and the desk merges
		// without --admin, the merge brings a behind branch up to date
		// first: a merged head that only merges the base into the reviewed
		// one is the reviewed change.
		if p.Head != act.Head && !l.baseMerged(ctx, p, act.Head) {
			return "", refused("merged head differs from the reviewed head; reconciliation required")
		}
		closed, closeErr := CloseMergedPullIssues(ctx, st, p)
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
	if GhMergePolicyRefused(policyReason) {
		pending = "repository merge policy refused; retrying next round: " + oneLine(policyReason)
		stage := act.Key + " admin configuration"
		l.Mu.Lock()
		reported := l.Stages[stage]
		l.Mu.Unlock()
		if !reported {
			guidance := "repository merge policy remains unmet"
			if strings.Contains(policyReason, "--admin") {
				guidance = "check merge.admin for " + ref.Repo
			}
			message := fmt.Sprintf("SUPERVISOR MERGE POLICY REFUSED %s: %s; reviewDecision=%s; retrying next round: %s", act.URL, guidance, p.ReviewDecision, oneLine(policyReason))
			if err := a.store.Record(about(event(LoopParty, LoopStageVerb, "[%s] %s", stage, message), act.Ref)); err != nil {
				return "", err
			}
			l.Mu.Lock()
			l.Stages[stage] = true
			l.Mu.Unlock()
			_, _ = fmt.Fprintln(a.out, message)
		}
		return "", MergeHeld(waitPolicy, "%s", pending)
	}
	if err == nil && (code == ExitGateQueued || code == ExitGateDuplicate && !mutationResult) {
		pending = fmt.Sprintf("merge gate waiting (exit %d): %s", code, oneLine(out))
		return "", MergeHeld(waitGate, "%s", pending)
	}
	// An unavailable read never authorizes a merge and is not a durable
	// policy refusal. Reconsider it on a budget-admitted pass, so quota
	// recovery does not wait for the generic exponential failure backoff.
	if err == nil && (code == ExitGateUnread || code == ExitGateRefused && github.RateLimited(errors.New(out))) {
		pending = "GitHub read unavailable; retrying when GitHub answers: " + oneLine(out)
		return "", MergeHeld(WaitUnread, "%s", pending)
	}
	if err == nil && code == ExitGateRefused || mutationResult && code == github.SquashRefused {
		if mutationResult && reply.Reason != "" {
			out = reply.Reason
		}
		return "", refused("merge gate refused: %s", oneLine(out))
	}
	if code == 0 && mutationResult && reply.Verdict == ReviewedMergeQueued {
		pending = "server-side merge queued; awaiting merge-result checks: " + oneLine(reply.Reason)
		return "", MergeHeld(waitQueue, "%s", pending)
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
	if LatestReviewRequestsChanges(p) || reviewOvertookDecision(p, act) {
		return "", refused("a review requested changes; new supervisor approval required")
	}
	if mutationResult && (code == github.SquashNotApplicable || code == github.SquashRed) {
		if reply.Reason != "" {
			out = reply.Reason
		}
		if StaleMergeResultFailure(out, act.Head) {
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
func StaleMergeResultFailure(reason, head string) bool {
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

func (l *Loop) logStaleMergeFailure(ref, head string) error {
	message := fmt.Sprintf("SUPERVISOR MERGE DROPPED stale failure for %s at %s; rechecking through the merge gate", ref, head)
	if err := l.Host.store.Record(about(event(LoopParty, LoopMergeFailureDropped, "%s", message), ref)); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(l.Host.out, message)
	return nil
}

// DropInvalidMergeRepairs also retires persisted repairs whose merge-result head
// contradicts the decision. It undoes a branch repair an older loop requested
// for a merge refused by base branch policy: no branch
// change repairs that refusal. Its repair stage and the go it completed are
// closed durably, so the unchanged approved head is merged again on this
// pass without a new review or the worker's action. A repair stage for any
// other reason stands.
func (l *Loop) DropInvalidMergeRepairs(taken []superloop.Taken, stages map[string]bool, stageDetail map[string]string) ([]superloop.Taken, error) {
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
		stale := StaleMergeResultFailure(detail, head)
		if !stale && !GhMergePolicyRefused(detail) {
			continue
		}
		why := "base branch policy refusal requires coordinator policy resolution"
		if stale {
			why = "stale merge-result failure does not describe the reviewed head"
		}
		// A preview reconciles only its in-memory plan. Leave the durable
		// records for the next real pass, which holds the loop lock.
		if !l.DryRun {
			if err := l.Host.store.Record(about(event(LoopParty, LoopUnstageVerb, "[%s] %s", stage, why), ref)); err != nil {
				return taken, err
			}
		}
		delete(stages, stage)
		delete(stageDetail, stage)
		if slices.ContainsFunc(taken, func(t superloop.Taken) bool { return t.Key == key }) {
			if !l.DryRun {
				if err := l.Host.store.Record(about(event(LoopParty, LoopClaimEndVerb, "[%s] %s", key, why), ref)); err != nil {
					return taken, err
				}
			}
			taken = slices.DeleteFunc(taken, func(t superloop.Taken) bool { return t.Key == key })
		}
		if stale && !l.DryRun {
			if err := l.logStaleMergeFailure(ref, head); err != nil {
				return taken, err
			}
		} else if !l.DryRun {
			_, _ = fmt.Fprintf(l.Host.out, "SUPERVISOR MERGE RETRY %s: %s; retrying with configured merge identity and policy\n", key, why)
		}
	}
	return taken, nil
}

// repairPull hands an open pull request whose merge was not applicable or red
// to its worker once, recording the repair stage before waking or starting a
// fixer so capacity waits never retry the merge command each pass.
func (l *Loop) repairPull(ctx context.Context, st *state.State, act superloop.Action, reason string) (string, error) {
	if StaleMergeResultFailure(reason, act.Head) {
		if err := l.logStaleMergeFailure(act.Ref, act.Head); err != nil {
			return "", err
		}
		return "", MergeHeld(waitGate, "a stale merge-result failure was dropped; the merge gate validates the reviewed head again on the next pass")
	}
	repairStage := act.Key + " repair"
	l.Mu.Lock()
	repairing := l.Stages[repairStage]
	l.Mu.Unlock()
	if !repairing {
		if err := l.Host.store.Record(about(event(LoopParty, LoopStageVerb, "[%s] %s", repairStage, reason), act.Ref)); err != nil {
			return "", err
		}
		l.Mu.Lock()
		if l.Stages == nil {
			l.Stages = map[string]bool{}
		}
		if l.StageDetail == nil {
			l.StageDetail = map[string]string{}
		}
		l.Stages[repairStage] = true
		l.StageDetail[repairStage] = reason
		l.Mu.Unlock()
	}
	act.Message = fmt.Sprintf("The supervisor could not merge %s at reviewed head %s %s. Repair the branch, push all fixes together without local tests or lints and immediately park for the loop to run CI; if the new head only rebases onto or merges the base branch to resolve conflicts, its earlier approval still covers it and the supervisor merges once CI is green without a new review.", act.URL, act.Head, reason)
	if err := l.WakeOrStartFixer(ctx, st, act); err != nil {
		// The merge is over; the decision stays pending until a worker has
		// the repair. Say both, or the wait reads as a merge that never ran.
		return "", namedWait(err, waitRepair, fmt.Sprintf("its merge failed %s; the branch repair it requested waits for a worker", truncate(oneLine(reason), 300)))
	}
	return "merge failed; branch repair requested", nil
}

// EndMerged closes tasks that explicitly declared no post-merge work and
// wakes only tasks with a remaining release, rollout or proof. Older parks
// retain the existing release-containment and retirement behavior.
func (l *Loop) EndMerged(ctx context.Context, st *state.State, act superloop.Action) (string, error) {
	if latest, err := l.Host.store.Read(); err == nil {
		st = latest
	}
	if by, ok := strings.CutPrefix(act.Message, "absorbed by "); ok {
		return l.endAbsorbed(ctx, st, act, strings.TrimSpace(by))
	}
	if act.Message != strings.ToLower(superloop.Closed) {
		l.Mu.Lock()
		mp, ok := l.Merged[act.Ref]
		l.Mu.Unlock()
		if !ok {
			var readErr error
			mp, readErr = l.readPull(ctx, act.Ref)
			if readErr != nil {
				return "", readErr
			}
		}
		mp.Ref = cmp.Or(mp.Ref, act.Ref)
		mp.URL = cmp.Or(mp.URL, act.URL)
		closed, closeErr := CloseMergedPullIssues(ctx, st, mp)
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
			_ = l.Host.store.Update(func(current *state.State) ([]state.Event, error) {
				current.ReviewParks = slices.DeleteFunc(current.ReviewParks, func(p state.ReviewPark) bool { return strings.EqualFold(p.On, act.Ref) })
				return nil, nil
			})
			return fmt.Sprintf("%s %s: tracking ends", act.URL, act.Message), nil
		}
		if err := l.claim(act, rp.Agent); err != nil {
			return "", err
		}
		clearReviewPark := func() {
			_ = l.Host.store.Update(func(current *state.State) ([]state.Event, error) {
				current.ReviewParks = slices.DeleteFunc(current.ReviewParks, func(p state.ReviewPark) bool { return strings.EqualFold(p.On, act.Ref) })
				return nil, nil
			})
		}
		if act.Message == strings.ToLower(superloop.Closed) {
			if strings.TrimSpace(*rp.PostMerge) != "" || (rp.Serves != "" && !strings.EqualFold(rp.Serves, act.Ref) && l.open(ctx, rp.Serves)) {
				act.Agent = rp.Agent
				act.Message = fmt.Sprintf("%s closed without merging. Continue or finish the owning task, and report what remains open.", act.URL)
				if err := l.WakeOrStartFixer(ctx, st, act); err != nil {
					return "", err
				}
				clearReviewPark()
				return "closed; worker told to continue its task", nil
			}
			err := l.Host.store.Update(func(current *state.State) ([]state.Event, error) {
				current.ReviewParks = slices.DeleteFunc(current.ReviewParks, func(p state.ReviewPark) bool { return strings.EqualFold(p.On, act.Ref) })
				for i := range current.Agents {
					worker := &current.Agents[i]
					if worker.Is(rp.Party) && worker.Park != nil && worker.Park.ReviewHead != "" {
						reportIdle(worker, l.Host.timeNow())
						worker.Done = true
					}
				}
				endNotes(current, rp.Party, l.Host.timeNow())
				clearRecords(current, rp.Party)
				return nil, nil
			})
			return "closed; task already settled", err
		}
		if work := strings.TrimSpace(*rp.PostMerge); work != "" {
			act.Agent = rp.Agent
			act.Message = fmt.Sprintf("The supervisor merged %s. Complete the remaining post-merge work: %s. Report the version, proof, live facts and open items with agents idle --done --report and --problem.", act.URL, work)
			if err := l.WakeOrStartFixer(ctx, st, act); err != nil {
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
			if err := l.WakeOrStartFixer(ctx, st, act); err != nil {
				return "", err
			}
			clearReviewPark()
			return "merged; worker woken for remaining open work", nil
		}
		if err := l.moveServedDone(ctx, rp.Serves, act.Ref); err != nil {
			return "", err
		}
		err := l.Host.store.Update(func(current *state.State) ([]state.Event, error) {
			current.ReviewParks = slices.DeleteFunc(current.ReviewParks, func(p state.ReviewPark) bool { return strings.EqualFold(p.On, act.Ref) })
			// The last completed ref may differ from the worker's current
			// park when its PRs merge out of order. Finish that parked owner too.
			for i := range current.Agents {
				worker := &current.Agents[i]
				if worker.Is(rp.Party) && worker.Park != nil && worker.Park.ReviewHead != "" {
					reportIdle(worker, l.Host.timeNow())
					worker.Done = true
				}
			}
			endNotes(current, rp.Party, l.Host.timeNow())
			clearRecords(current, rp.Party)
			report := fmt.Sprintf("%s merged at %s; no post-merge work declared; open items: none", act.URL, l.Merged[act.Ref].MergeCommit)
			task := cmp.Or(rp.Serves, act.Ref)
			current.WorkerReports = append(current.WorkerReports, state.WorkerReport{Finished: true, By: rp.Party, At: l.Host.timeNow(), Task: task, Text: report})
			return []state.Event{event(rp.Party, superloop.VerbDone, "%s done: %s", rp.Agent, task), event(rp.Party, superloop.VerbReport, "%s: %s", rp.Agent, report)}, nil
		})
		return "merged; task closed without waking worker", err
	}
	ag := st.Agents[i]
	if err := l.claim(act, ag.Name); err != nil {
		return "", err
	}
	defer func() {
		_ = l.Host.store.Update(func(current *state.State) ([]state.Event, error) {
			current.ReviewParks = slices.DeleteFunc(current.ReviewParks, func(p state.ReviewPark) bool { return strings.EqualFold(p.On, act.Ref) })
			return nil, nil
		})
	}()
	if act.Message == strings.ToLower(superloop.Closed) {
		serves := cmp.Or(LoopServes(st, ag), LoopTaskServes(ag, act.Ref))
		if strings.TrimSpace(*ag.Park.PostMerge) == "" && (serves == "" || strings.EqualFold(serves, act.Ref) || !l.open(ctx, serves)) {
			err := l.Host.store.Update(func(current *state.State) ([]state.Event, error) {
				i := slices.IndexFunc(current.Agents, func(other state.Agent) bool { return other.Is(ag.Party) })
				if i < 0 || current.Agents[i].Done {
					return nil, nil
				}
				worker := &current.Agents[i]
				reportIdle(worker, clk.Now())
				worker.Done = true
				endNotes(current, worker.Party, l.Host.timeNow())
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
	serves := LoopServes(st, ag)
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
	err := l.Host.store.Update(func(current *state.State) ([]state.Event, error) {
		i := slices.IndexFunc(current.Agents, func(other state.Agent) bool { return other.Is(ag.Party) })
		if i < 0 || current.Agents[i].Done {
			return nil, nil
		}
		worker := &current.Agents[i]
		report := fmt.Sprintf("%s merged at %s; no post-merge work declared; open items: none", act.URL, l.Merged[act.Ref].MergeCommit)
		reportIdle(worker, clk.Now())
		worker.Done = true
		endNotes(current, worker.Party, l.Host.timeNow())
		clearRecords(current, worker.Party)
		current.WorkerReports = append(current.WorkerReports, state.WorkerReport{Finished: true, By: worker.Party, At: l.Host.timeNow(), Task: worker.LastTask, Text: report})
		return []state.Event{event(worker.Party, superloop.VerbDone, "%s done: %s", worker.Name, worker.LastTask), event(worker.Party, superloop.VerbReport, "%s: %s", worker.Name, report)}, nil
	})
	return "merged; task closed without waking worker", err
}

func (l *Loop) endAbsorbed(ctx context.Context, st *state.State, act superloop.Action, by string) (string, error) {
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
	l.Mu.Lock()
	byPull := l.Merged[by]
	l.Mu.Unlock()
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
	closed, closeErr := CloseMergedPullIssues(ctx, st, byPull)
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
		serves = cmp.Or(LoopServes(st, ag), LoopTaskServes(ag, act.Ref))
	case hasRP && rp.PostMerge != nil:
		targetAgent = rp.Agent
		targetParty = rp.Party
		postMerge = strings.TrimSpace(*rp.PostMerge)
		serves = rp.Serves
	default:
		_ = l.Host.store.Update(func(current *state.State) ([]state.Event, error) {
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
	err := l.Host.store.Update(func(current *state.State) ([]state.Event, error) {
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
					reportIdle(worker, l.Host.timeNow())
					worker.Done = true
				}
			}
			endNotes(current, targetParty, l.Host.timeNow())
			clearRecords(current, targetParty)
		}
		return nil, nil
	})
	if err != nil {
		return "", err
	}
	if !isRescue && !reconciledToCombining {
		origURL := cmp.Or(act.URL, PullRefURL(act.Ref))
		byURL := PullRefURL(by)
		if postMerge != "" {
			act.Ref = by
			act.URL = byURL
			act.Agent = targetAgent
			act.Message = fmt.Sprintf("Combining pull request %s merged and absorbed %s. Complete the remaining post-merge work: %s. Report the version, proof, live facts and open items with agents idle --done --report and --problem.", byURL, origURL, postMerge)
			if err := l.WakeOrStartFixer(ctx, st, act); err != nil {
				return "", err
			}
			return fmt.Sprintf("absorbed by %s; post-merge work reconciled with combining pull request", by), nil
		}
		if serveOpen {
			act.Ref = by
			act.URL = byURL
			act.Agent = targetAgent
			act.Message = fmt.Sprintf("Combining pull request %s merged and absorbed %s. The task at %s is still open or its closure could not be confirmed. Continue its remaining work, or verify it is complete and report done.", byURL, origURL, issueURL(serves))
			if err := l.WakeOrStartFixer(ctx, st, act); err != nil {
				return "", err
			}
			return fmt.Sprintf("absorbed by %s; worker woken for remaining open work", by), nil
		}
	}
	return fmt.Sprintf("absorbed by %s; rescue task withdrawn", by), nil
}

func PullRefURL(ref string) string {
	if r, ok := parseRef(ref); ok {
		return fmt.Sprintf("https://github.com/%s/pull/%d", r.Repo, r.N)
	}
	return ref
}

var ClosePullIssue = func(ctx context.Context, issue github.PR, pullURL string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	_, err := github.RunGH(github.ActingOn(ctx, issue.Repo), "issue", "close", strconv.Itoa(issue.N), "--repo", issue.Repo, "--reason", "completed", "--comment", fmt.Sprintf("Closed by %s.", pullURL))
	return err
}

var EditPullBody = func(ctx context.Context, pull github.PR, body string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	_, err := github.RunGH(github.ActingOn(ctx, pull.Repo), "pr", "edit", strconv.Itoa(pull.N), "--repo", pull.Repo, "--body", body)
	return err
}

func (l *Loop) recordClosedPullIssues(closed []string) {
	if l == nil || len(closed) == 0 {
		return
	}
	l.Mu.Lock()
	defer l.Mu.Unlock()
	if l.opens == nil {
		l.opens = map[string]loopOpen{}
	}
	now := l.Host.timeNow()
	for _, ref := range closed {
		l.opens[ref] = loopOpen{open: false, at: now}
	}
}

func AbsorbedClosingIssues(ctx context.Context, st *state.State, absorbs []string) ([]string, error) {
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
					if s := cmp.Or(LoopServes(st, ag), LoopTaskServes(ag, orig)); s != "" && !strings.EqualFold(s, orig) {
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

func EnsureCombinedPullCloses(ctx context.Context, st *state.State, p *superloop.Pull) error {
	if p == nil || len(p.Absorbs) == 0 {
		return nil
	}
	absorbed, err := AbsorbedClosingIssues(ctx, st, p.Absorbs)
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
	if err := EditPullBody(ctx, pr, updated); err != nil {
		return err
	}
	p.Body = updated
	return nil
}

func CloseMergedPullIssues(ctx context.Context, st *state.State, p superloop.Pull) ([]string, error) {
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
	absorbed, err := AbsorbedClosingIssues(ctx, st, p.Absorbs)
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
	pullURL := cmp.Or(p.URL, PullRefURL(p.Ref))
	var closed []string
	for _, r := range refs {
		s, ok := states[r]
		if !ok || s.Kind != github.RefIssue || (s.State != github.Open && s.State != github.Closed) {
			return closed, fmt.Errorf("issue closure for %s is unconfirmed", refName(r))
		}
		if s.State == github.Open {
			if err := ClosePullIssue(ctx, r, pullURL); err != nil {
				return closed, err
			}
		}
		closed = append(closed, refName(r))
	}
	return closed, nil
}

// moveServedDone moves the task ref served to Done. A saved Serves can name a
// pull request, which the board refuses as no issue: that must not fail the end.
func (l *Loop) moveServedDone(ctx context.Context, serves, ref string) error {
	if serves == "" || serves == ref || l.Host.cfg.Board.Owner == "" {
		return nil
	}
	_, err := l.Run(ctx, LoopBoard, "move", serves, "Done")
	if err != nil && Code(err) == ExitRefused && strings.Contains(err.Error(), " is no issue:") {
		return nil
	}
	return err
}

const MergeableConflicting = "CONFLICTING"

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

// LatestReviewRequestsChanges reports whether the latest review verdict on p
// requests changes on its current head, overriding any earlier approval.
// A different head still needs the loop's exact-head decision; the executor's
// authority check rejects any go overtaken by a later change request.
func LatestReviewRequestsChanges(p superloop.Pull) bool {
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
	return found && latest.Verdict == superloop.ChangesRequested && SameCommit(cmp.Or(latest.Head, latest.Commit), p.Head)
}

// DeskVerdicts is reviews with the verdict of each one the desk does not
// trust cleared: a verdict counts only from a member or collaborator of the
// repository's owner or from one of authors, the logins the desk opens and
// reviews its pull requests as. On a public repository anyone can post
// "Review: approved": the planner and the merge gate both read verdicts
// through this, so neither merges nor holds on an outsider's word.
func DeskVerdicts(reviews []superloop.Review, authors []string) []superloop.Review {
	out := slices.Clone(reviews)
	for i := range out {
		if !out[i].Member && !board.ContainsFold(authors, out[i].Author) {
			out[i].Verdict = ""
		}
	}
	return out
}

func SameCommit(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}
