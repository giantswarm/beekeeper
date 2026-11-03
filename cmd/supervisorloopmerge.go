package cmd

import (
	"cmp"
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

const loopWorkerMergeGo = "merge go given"
const reviewedMergeQueued = "queued"

func pullNeedsBranchRepair(p superloop.Pull) bool {
	return p.Draft || p.Mergeable == "CONFLICTING" || p.MergeState == "BEHIND"
}

func ghMergeNeedsBranchRepair(reason string) bool {
	lower := strings.ToLower(reason)
	return strings.Contains(lower, "conflict") || strings.Contains(lower, "behind") || strings.Contains(lower, "out of date") || strings.Contains(lower, "out-of-date") || strings.Contains(lower, "cannot be cleanly created")
}

// An admin suggestion is a desk configuration refusal, even when GitHub's
// merge state also says BEHIND. It cannot be repaired by changing the branch.
func ghMergeNeedsAdmin(reason string) bool {
	lower := strings.ToLower(reason)
	return strings.Contains(lower, "base branch policy") && strings.Contains(lower, "--admin")
}

// mergePull executes the planner's exact-head decision in the coordinator,
// including for workers on an execution host. Failed or unreadable validation
// never delegates a direct merge around the required server-side queue.
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
	if boardBudgetLow(st.Budget, a.cfg.GitHub.Floor, a.timeNow()) || a.resetFirst(st) {
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
		return fmt.Sprintf("merged %s at %s", act.URL, p.Head), nil
	}
	if p.State != superloop.Open || p.Head != act.Head || !p.Green {
		return "", refused("merge head moved or current CI is not green")
	}
	repair := func(code int, reason string) (string, error) {
		_, _ = stopMerges(a.store, loopParty, func(m state.Merge) bool {
			return m.Repo == ref.Repo && m.PR == ref.N && m.Phase == state.Waiting
		}, false, func(state.Merge) string {
			return reason
		})
		_ = a.store.Update(func(current *state.State) ([]state.Event, error) {
			merge.Retry(current, ref.Repo, ref.N)
			return nil, nil
		})
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
		if f.Repo == ref.Repo && f.PR == ref.N && f.By.Name == loopParty.Name && ghMergeNeedsAdmin(f.Reason) {
			// An older gate may have recorded this as a branch failure.
			// Drop only that failed attempt so the unchanged head can rejoin.
			if err := a.store.Update(func(current *state.State) ([]state.Event, error) {
				merge.Retry(current, ref.Repo, ref.N)
				return nil, nil
			}); err != nil {
				return "", err
			}
			continue
		}
		if f.Repo == ref.Repo && f.PR == ref.N && f.By.Name == loopParty.Name && (f.Exit == github.SquashNotApplicable || f.Exit == github.SquashRed) {
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
		if err := a.executionMergeLive(ctx, st, use); err != nil {
			return "", err
		}
	}
	if err := l.claim(act, act.Agent); err != nil {
		return "", err
	}
	if err := a.store.Record(about(event(loopParty, "merge.start", "merging %s at %s", act.URL, act.Head), act.Ref)); err != nil {
		return "", err
	}
	gateArgs := []string{gateCmdName, "--wait", "0s", "--limit", "0", "--reviewed-head", act.Head}
	if act.Priority > 0 {
		gateArgs = append(gateArgs, "--priority", strconv.Itoa(act.Priority))
	}
	gateArgs = append(gateArgs, "--", "devctl", "pr", superloop.KindMerge, ref.Repo, fmt.Sprint(ref.N))
	out, code, err := beekeeperRun(ctx, l.as(gateArgs...)...)
	// Read after every outcome, including a lost response: a restart never
	// repeats a mutation already observed merged.
	p, readErr := readLoopPull(ctx, act.Ref)
	if readErr != nil {
		return "", readErr
	}
	if p.State == superloop.Merged {
		// Where the base requires up-to-date branches and the desk merges
		// without --admin, the merge brings a behind branch up to date
		// first: a merged head that only merges the base into the reviewed
		// one is the reviewed change.
		if p.Head != act.Head && !l.baseMerged(ctx, p, act.Head) {
			return "", refused("merged head differs from the reviewed head; reconciliation required")
		}
		return fmt.Sprintf("merged %s at %s (merge commit %s)", act.URL, p.Head, p.MergeCommit), nil
	}
	var reply github.SquashDoc
	mutationResult := err == nil && json.NewDecoder(strings.NewReader(out)).Decode(&reply) == nil && reply.Route == reviewedMergeRoute && reply.Repo == ref.Repo && reply.Number == ref.N && reply.HeadSha == act.Head
	policyReason := out
	if mutationResult {
		policyReason = reply.Reason
	}
	if ghMergeNeedsAdmin(policyReason) {
		stage := act.Key + " admin configuration"
		l.mu.Lock()
		reported := l.stages[stage]
		l.mu.Unlock()
		if !reported {
			message := fmt.Sprintf("SUPERVISOR MERGE CONFIGURATION REFUSED %s: check merge.admin for %s; retrying next round: %s", act.URL, ref.Repo, oneLine(policyReason))
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
		return "", errLoopLater
	}
	if err == nil && (code == ExitGateUnread || code == ExitGateRefused) || mutationResult && code == github.SquashRefused {
		if mutationResult && reply.Reason != "" {
			out = reply.Reason
		}
		return "", refused("merge gate refused: %s", oneLine(out))
	}
	if code == 0 && mutationResult && reply.Verdict == reviewedMergeQueued {
		return "", errLoopLater
	}
	if mutationResult && code == github.SquashTooling {
		return "", refused("merge-result validation unavailable: %s", reply.Reason)
	}
	if p.State != superloop.Open || p.Head != act.Head || !p.Green {
		return "", refused("merge head moved or current CI is not green")
	}
	if mutationResult && (code == github.SquashNotApplicable || code == github.SquashRed) {
		if reply.Reason != "" {
			out = reply.Reason
		}
		return l.repairPull(ctx, st, act, fmt.Sprintf("(exit %d): %s", code, oneLine(out)))
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

// dropAdminRepairs undoes a branch repair an older loop requested for a
// merge refused by base branch policy with an admin suggestion: no branch
// change repairs that refusal. Its repair stage and the go it completed are
// closed durably, so the unchanged approved head is merged again on this
// pass without a new review or the worker's action. A repair stage for any
// other reason stands.
func (l *supervisorLoop) dropAdminRepairs(taken []superloop.Taken, stages map[string]bool, stageDetail map[string]string) ([]superloop.Taken, error) {
	for stage, detail := range stageDetail {
		key, ok := strings.CutSuffix(stage, " repair")
		if !ok || !stages[stage] || !strings.HasPrefix(key, superloop.KindGo+" ") || !ghMergeNeedsAdmin(detail) {
			continue
		}
		ref := strings.TrimPrefix(key, superloop.KindGo+" ")
		if i := strings.LastIndex(ref, "@"); i >= 0 {
			ref = ref[:i]
		}
		why := "base branch policy refusal is merge configuration, not branch repair"
		if err := l.a.store.Record(about(event(loopParty, loopUnstageVerb, "[%s] %s", stage, why), ref)); err != nil {
			return taken, err
		}
		delete(stages, stage)
		delete(stageDetail, stage)
		if slices.ContainsFunc(taken, func(t superloop.Taken) bool { return t.Key == key }) {
			if err := l.a.store.Record(about(event(loopParty, loopClaimEndVerb, "[%s] %s", key, why), ref)); err != nil {
				return taken, err
			}
			taken = slices.DeleteFunc(taken, func(t superloop.Taken) bool { return t.Key == key })
		}
		_, _ = fmt.Fprintf(l.a.out, "SUPERVISOR MERGE RETRY %s: %s; check merge.admin for its repository\n", key, why)
	}
	return taken, nil
}

// repairPull hands an open pull request whose merge was not applicable or red
// to its worker once, recording the repair stage before waking or starting a
// fixer so capacity waits never retry the merge command each pass.
func (l *supervisorLoop) repairPull(ctx context.Context, st *state.State, act superloop.Action, reason string) (string, error) {
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
	act.Message = fmt.Sprintf("The supervisor could not merge %s at reviewed head %s %s. Repair the branch, push all fixes together, wait for green CI and park for desk review again; if the new head only rebases onto or merges the base branch to resolve conflicts, its earlier approval still covers it and the supervisor merges once CI is green without a new review.", act.URL, act.Head, reason)
	if err := l.wakeOrStartFixer(ctx, st, act); err != nil {
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
			act.Agent = rp.Agent
			act.Message = fmt.Sprintf("%s closed without merging. Continue or finish the owning task, and report what remains open.", act.URL)
			if err := l.wakeOrStartFixer(ctx, st, act); err != nil {
				return "", err
			}
			clearReviewPark()
			return "closed; worker told to continue its task", nil
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
		if rp.Serves != "" && rp.Serves != act.Ref && l.open(ctx, rp.Serves) {
			act.Agent = rp.Agent
			act.Message = fmt.Sprintf("The supervisor merged %s. The task at %s is still open or its closure could not be confirmed. Continue its remaining work, or verify it is complete and report done.", act.URL, issueURL(rp.Serves))
			if err := l.wakeOrStartFixer(ctx, st, act); err != nil {
				return "", err
			}
			clearReviewPark()
			return "merged; worker woken for remaining open work", nil
		}
		if rp.Serves != "" && rp.Serves != act.Ref && l.a.cfg.Board.Owner != "" {
			if _, err := l.run(ctx, loopBoard, "move", rp.Serves, "Done"); err != nil {
				return "", err
			}
		}
		err := l.a.store.Update(func(current *state.State) ([]state.Event, error) {
			current.ReviewParks = slices.DeleteFunc(current.ReviewParks, func(p state.ReviewPark) bool { return strings.EqualFold(p.On, act.Ref) })
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
		endNotes(current, worker.Party, l.a.timeNow())
		clearRecords(current, worker.Party)
		current.WorkerReports = append(current.WorkerReports, state.WorkerReport{Finished: true, By: worker.Party, At: l.a.timeNow(), Task: worker.LastTask, Text: report})
		return []state.Event{event(worker.Party, superloop.VerbDone, "%s done: %s", worker.Name, worker.LastTask), event(worker.Party, superloop.VerbReport, "%s: %s", worker.Name, report)}, nil
	})
	return "merged; task closed without waking worker", err
}

// The reasons a reviewed merge stops for desk policy, before and after a
// branch update alike.
const (
	reasonDeskStopped    = "the desk is stopped"
	reasonMergeHeld      = "merge held by desk policy"
	mergeableConflicting = "CONFLICTING"
	mergeMethodRebase    = "rebase"
)

// reviewedMerge is the gate's mutation child. A branch behind its base it
// merges at the reviewed head, as it stands. Only where the base requires
// up-to-date branches and the desk merges without --admin does it update the
// branch once, as the gate's --update-branch would, and merge the updated
// head once its checks are green and the approval covers it
// (updateReviewedBranch); it never waits for a head anyone else pushed, or
// retries a mutation. The gate retains its
// ordinary holds, queue, readiness, review and release settlement policy.
func (a *app) reviewedMerge(ctx context.Context, repo string, number int, head string) github.SquashDoc {
	doc := github.SquashDoc{Repo: repo, Number: number, HeadSha: head, SchemaVersion: 1, Command: "pr merge", Route: reviewedMergeRoute, ExitCode: github.SquashNotApplicable}
	method := a.cfg.Merge.Methods[repo]
	if method == "" {
		method = "squash"
	}
	if method != "squash" && method != mergeMethodRebase && method != superloop.KindMerge {
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
		doc.ExitCode = 0
		doc.MergeCommitSha = p.MergeCommit
		return doc
	}
	if p.State != superloop.Open || p.Head != head || !p.Green {
		doc.ExitCode = github.SquashRefused
		doc.Reason = "merge head moved or current CI is not green"
		return doc
	}
	if p.Draft || p.Mergeable == mergeableConflicting {
		doc.Reason = "branch is draft or conflicts with its base"
		return doc
	}
	// A branch only behind its base merges as it stands: GitHub merges the
	// reviewed head onto the current base, with no branch update and no
	// second CI run. Only a base that requires up-to-date branches refuses
	// that merge, which GitHub reports as the merge state BEHIND: the desk's
	// admin merge (merge.admin) passes the requirement; without it the branch
	// is brought up to date below, once the merge is authorized, by merging
	// the base, which a rebase merge cannot take.
	update := p.MergeState == mergeStateBehind && !a.cfg.Merge.AdminMerge(repo)
	if update && method == mergeMethodRebase {
		doc.Reason = "branch is behind a base that requires up-to-date branches"
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
		if err := a.authenticateExecutionReview(ctx, st, use, p); err != nil {
			doc.ExitCode = github.SquashRefused
			doc.Reason = err.Error()
			return doc
		}
	}
	if !claimed {
		doc.ExitCode = github.SquashTooling
		doc.Reason = "no pending exact-head supervisor merge claim"
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
	if update {
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
			doc.ExitCode = github.SquashRefused
			doc.Reason = reasonDeskStopped
			return doc
		}
		if _, held := merge.BlockingFor(st, time.Now(), loopParty.Name, repo, number, a.cfg.LaneOf(repo)); held {
			doc.ExitCode = github.SquashRefused
			doc.Reason = reasonMergeHeld
			return doc
		}
		p, merging = up, up.Head
	}
	if merging == head {
		if reason, code := a.checkReviewedMergeResult(ctx, p, head); code != 0 {
			doc.ExitCode, doc.Reason = code, reason
			return doc
		}
	}
	if err := requireReviewedMergeQueue(ctx, p); err != nil {
		doc.ExitCode, doc.Reason = github.SquashTooling, err.Error()
		return doc
	}
	argv := []string{"gh", "pr", superloop.KindMerge, fmt.Sprint(number), "--repo", repo, "--" + method, "--delete-branch", "--match-head-commit", merging}
	// Never pass --admin: it bypasses the server-side merge queue.
	out, stderr, code, runErr := loopCommand(ctx, "", argv)
	doc.Reason = oneLine(out + stderr)
	if runErr != nil {
		doc.Reason = runErr.Error()
	}
	p, err = readLoopPull(ctx, ref)
	if err != nil {
		doc.ExitCode = github.SquashTooling
		doc.Reason = err.Error()
		return doc
	}
	switch {
	case p.State == superloop.Merged && p.Head == merging:
		doc.ExitCode = 0
		doc.MergeCommitSha = p.MergeCommit
		doc.Verdict = "merged"
	case runErr == nil && code == 0 && p.State == superloop.Open && p.Head == merging:
		doc.ExitCode, doc.Verdict = 0, reviewedMergeQueued
		doc.Reason = "merge queued; awaiting server-side merge-result checks"
	case p.State != superloop.Open || p.Head != merging || !p.Green:
		doc.ExitCode = github.SquashRefused
		if doc.Reason == "" {
			doc.Reason = "merge head moved or current CI is not green"
		}
	case ghMergeNeedsAdmin(doc.Reason):
		doc.ExitCode = github.SquashRefused
	case pullNeedsBranchRepair(p) || runErr == nil && code != 0 && ghMergeNeedsBranchRepair(doc.Reason):
		doc.ExitCode = github.SquashNotApplicable
		if doc.Reason == "" {
			doc.Reason = "branch is draft, behind or conflicts with its base"
		}
	default:
		doc.ExitCode = github.SquashTooling
		if code == 0 && runErr == nil && doc.Reason == "" {
			doc.Reason = "merge not confirmed"
		}
	}
	return doc
}

// readReviewedBehind is how many commits of its base p's head lacks, read
// from GitHub's comparison: the update of a branch whose base requires
// up-to-date branches (updateReviewedBranch) reads it of the head it made,
// whose merge state GitHub may not have computed yet.
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

// A local validation cannot atomically pin GitHub's base branch. Require the
// server queue to retest its merge group if any writer advances the base.
var requireReviewedMergeQueue = func(ctx context.Context, p superloop.Pull) error {
	ref, ok := parseRef(p.Ref)
	if !ok || p.Base == "" {
		return fmt.Errorf("merge queue: invalid base")
	}
	owner, name, _ := strings.Cut(ref.Repo, "/")
	out, err := github.RunGH(ctx, "api", "graphql", "-f", "query=query($owner:String!,$name:String!,$branch:String!){repository(owner:$owner,name:$name){mergeQueue(branch:$branch){id}}}", "-f", "owner="+owner, "-f", "name="+name, "-f", "branch="+p.Base)
	if err != nil {
		return fmt.Errorf("merge queue unavailable: %w", err)
	}
	var result struct {
		Data struct {
			Repository struct{ MergeQueue *struct{ ID string } }
		}
		Errors []json.RawMessage
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return fmt.Errorf("reading merge queue: %w", err)
	}
	if len(result.Errors) != 0 || result.Data.Repository.MergeQueue == nil || result.Data.Repository.MergeQueue.ID == "" {
		return fmt.Errorf("merge queue required for atomic merge-result validation; configure required merge-group CI before merging")
	}
	return nil
}
