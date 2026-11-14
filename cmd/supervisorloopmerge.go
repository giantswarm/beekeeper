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

	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/gate"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func ghMergeNeedsBranchRepair(reason string) bool {
	lower := strings.ToLower(reason)
	return strings.Contains(lower, "conflict") || strings.Contains(lower, "cannot be cleanly created")
}

// The reasons a reviewed merge stops for desk policy, before and after a
// branch update alike.
const (
	reasonDeskStopped      = "the desk is stopped"
	reasonMergeHeld        = "merge held by desk policy"
	reasonMergeHeadChanged = "merge head moved or current CI is not green"
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
	p, err := loop.ReadLoopPull(ctx, ref)
	if err != nil {
		doc.ExitCode = github.SquashTooling
		doc.Reason = err.Error()
		return doc
	}
	if p.State == superloop.Merged && p.Head == head {
		st, _ := a.store.Read()
		if _, err := loop.CloseMergedPullIssues(ctx, st, p); err != nil {
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
	if loop.LatestReviewRequestsChanges(p) {
		doc.ExitCode = github.SquashRefused
		doc.Reason = "a review requested changes; new supervisor approval required"
		return doc
	}
	if p.Draft || p.Mergeable == loop.MergeableConflicting {
		doc.Reason = "branch is draft or conflicts with its base"
		return doc
	}
	st, err := a.store.Read()
	if err != nil {
		doc.ExitCode = github.SquashTooling
		doc.Reason = err.Error()
		return doc
	}
	evs, logErr := a.store.Events(0, func(e state.Event) bool { return e.By == loop.LoopParty && e.Subject == ref })
	if logErr != nil {
		doc.ExitCode = github.SquashTooling
		doc.Reason = "merge authority unreadable"
		return doc
	}
	claimed := false
	claimAgent := ""
	for _, e := range evs {
		key, rest, ok := loop.Keyed(e.Detail)
		if !ok {
			continue
		}
		key = superloop.BaseActionKey(key)
		// The sweep's go again for a head whose go was never carried out
		// is the same authority, keyed by its stretch without an owner.
		if key == "go "+ref+"@"+head || superloop.IsSweep(key) && strings.HasPrefix(key, "go "+ref+"@"+head+" ") {
			if e.Verb == loop.LoopClaimVerb {
				claimed = true
				claimAgent = rest
			}
			if e.Verb == loop.LoopMergeVerb || e.Verb == loop.LoopGoVerb {
				claimed = false
			}
		}
		if e.Verb == loop.LoopEndVerb && key == "end "+ref {
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
		doc.RequestedBy = fmt.Sprintf("%s (for %s)", loop.LoopParty.Name, claimAgent)
	} else if a.as != "" {
		doc.RequestedBy = a.as
	} else {
		doc.RequestedBy = loop.LoopParty.Name
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
	if _, held := merge.BlockingFor(st, clk.Now(), loop.LoopParty.Name, repo, number, a.cfg.LaneOf(repo)); held {
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
	p, err = loop.ReadLoopPull(ctx, ref)
	if err != nil {
		doc.ExitCode, doc.Reason = github.SquashTooling, err.Error()
		return doc
	}
	if p.State != superloop.Open || p.Head != merging || !p.Green {
		doc.ExitCode, doc.Reason = github.SquashRefused, reasonMergeHeadChanged
		return doc
	}
	if p.Draft || p.Mergeable == loop.MergeableConflicting {
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
	if _, held := merge.BlockingFor(st, clk.Now(), loop.LoopParty.Name, repo, number, a.cfg.LaneOf(repo)); held {
		doc.ExitCode, doc.Reason = github.SquashRefused, reasonMergeHeld
		return doc
	}
	if err := loop.EnsureCombinedPullCloses(ctx, st, &p); err != nil {
		doc.ExitCode, doc.Reason = github.SquashTooling, err.Error()
		return doc
	}
	argv := []string{"gh", "pr", superloop.KindMerge, fmt.Sprint(number), "--repo", repo, "--" + method, "--delete-branch", "--match-head-commit", merging}
	// A merge queue retests the merge group when the base advances; --admin
	// would bypass it. Without a queue the lane's own merge-result check
	// above is the validation, and the desk's admin merge passes the base's
	// review requirement as before the check.
	admin := !queue && a.cfg.Merge.AdminMerge(repo)
	if admin {
		// The admin exception belongs to the desk's gh login, not the
		// devctl App identity. Record its exact-head use before mutation.
		if err := a.store.Record(about(event(loop.LoopParty, "merge.admin", "https://github.com/%s/pull/%d at %s: admin review bypass using the desk gh login", repo, number, merging), ref)); err != nil {
			doc.ExitCode, doc.Reason = github.SquashTooling, err.Error()
			return doc
		}
		argv = append(argv, "--admin")
	}
	// Use the configured owner's devctl identity without letting pr merge
	// follow a new head during its wait. All gates above still govern this
	// exact-head mutation; no extra admin or auto flag is granted.
	if !admin && a.cfg.Merge.DevctlServes(repo) {
		argv = append([]string{devctlBinary, "auth", "exec", "--"}, argv...)
	}
	if a.cfg != nil {
		github.SetDevctlSkipVersionCheck(!a.cfg.Hooks.GuardsEnabled())
	}
	out, stderr, code, runErr := loop.LoopCommand(ctx, "", argv)
	doc.Reason = oneLine(out + stderr)
	if runErr != nil {
		doc.Reason = runErr.Error()
	}
	if note != "" {
		doc.Reason = strings.TrimSuffix(note+"; "+doc.Reason, "; ")
	}
	p, err = loop.ReadLoopPull(ctx, ref)
	if err != nil {
		doc.ExitCode = github.SquashTooling
		doc.Reason = err.Error()
		return doc
	}
	switch {
	case p.State == superloop.Merged && p.Head == merging:
		if _, err := loop.CloseMergedPullIssues(ctx, st, p); err != nil {
			doc.ExitCode = github.SquashTooling
			doc.Reason = err.Error()
			return doc
		}
		doc.ExitCode = 0
		doc.MergeCommitSha = p.MergeCommit
		doc.Verdict = "merged"
	case runErr == nil && code == 0 && p.State == superloop.Open && p.Head == merging:
		doc.ExitCode, doc.Verdict = 0, loop.ReviewedMergeQueued
		doc.Reason = "merge queued; awaiting server-side merge-result checks"
	case p.State != superloop.Open || p.Head != merging || !p.Green:
		doc.ExitCode = github.SquashRefused
		if doc.Reason == "" {
			doc.Reason = reasonMergeHeadChanged
		}
	case loop.GhMergePolicyRefused(doc.Reason):
		doc.ExitCode = github.SquashRefused
		doc.Reason = fmt.Sprintf("repository merge policy refused; branch updates are disabled; the repository owner must resolve any up-to-date requirement (reviewDecision=%s): %s", p.ReviewDecision, doc.Reason)
	case loop.PullNeedsBranchRepair(p) || runErr == nil && code != 0 && ghMergeNeedsBranchRepair(doc.Reason):
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
	if p.Head != "" && targetHead != "" && !loop.SameCommit(p.Head, targetHead) {
		return fmt.Sprintf("pull request head is %s, not %s", gate.ShortSHA(p.Head), gate.ShortSHA(targetHead)), false, nil
	}
	reviews, err := a.trustedReviews(ctx, p)
	if err != nil {
		return "", false, err
	}
	// A review of a head its author had replaced is no verdict
	// (loop.MarkReplaced): neither the latest review nor a request for
	// changes a go must come after.
	trusted := p
	trusted.Reviews = reviews
	reviews = loop.MarkReplaced(ctx, trusted).Reviews
	var latest *superloop.Review
	for i := range reviews {
		r := &reviews[i]
		if r.Replaced {
			continue
		}
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
		if loop.SameCommit(approvedHead, head) || st.ReviewNitsChecked(p.Ref, head, approvedHead) {
			return "", false, nil
		}
	}

	// A base-only update preserves the reviewed change even when review is
	// mandatory. Check it before refusing an approval on an older head.
	if approvedHead != "" && a != nil && a.cfg != nil && a.cfg.Merge.RequireReview {
		ok, err := loop.ReadBaseMerge(ctx, p, approvedHead)
		if err != nil {
			return "", false, err
		}
		if ok {
			return "", false, nil
		}
	}

	if a != nil && a.cfg != nil && a.cfg.Merge.RequireReview {
		return fmt.Sprintf("head %s requires an approved independent review; an exact-head go alone is insufficient", gate.ShortSHA(head)), false, nil
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
			if e.By == loop.LoopParty && strings.EqualFold(e.Subject, p.Ref) {
				if key, _, ok := loop.Keyed(e.Detail); ok {
					key = superloop.BaseActionKey(key)
					if after, ok := strings.CutPrefix(key, "go "+p.Ref+"@"); ok && after != "" {
						switch e.Verb {
						case loop.LoopGoVerb:
							activeGo[after] = goRecord{head: after, at: e.At}
						case loop.LoopMergeVerb, loop.LoopClaimEndVerb:
							delete(activeGo, after)
						}
					}
					if e.Verb == loop.LoopEndVerb && strings.EqualFold(key, "end "+p.Ref) {
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
			if r.Verdict != superloop.ChangesRequested || r.Replaced {
				return false
			}
			revHead := cmp.Or(r.Head, r.Commit)
			return loop.SameCommit(revHead, head) || (!g.at.IsZero() && !r.At.Before(g.at))
		})
	})
	// An exact head first: only then are base merges read, one GitHub
	// compare each.
	if slices.ContainsFunc(goes, func(g goRecord) bool { return loop.SameCommit(g.head, head) }) {
		return "", true, nil
	}
	if approvedHead != "" {
		ok, err := loop.ReadBaseMerge(ctx, p, approvedHead)
		if err != nil {
			return "", false, err
		}
		if ok {
			return "", false, nil
		}
	}
	for _, g := range goes {
		ok, err := loop.ReadBaseMerge(ctx, p, g.head)
		if err != nil {
			return "", false, err
		}
		if ok {
			return "", true, nil
		}
	}

	if latest != nil && latest.Verdict == superloop.ChangesRequested {
		return fmt.Sprintf("head %s has no approved desk review (latest desk review on %s requested changes) and no supervisor go for that exact head", gate.ShortSHA(head), gate.ShortSHA(cmp.Or(latest.Head, latest.Commit))), false, nil
	}
	if latest != nil && latest.Verdict == superloop.Approved {
		return fmt.Sprintf("head %s moved past approved head %s without a new approved desk review or supervisor go for %s", gate.ShortSHA(head), gate.ShortSHA(cmp.Or(latest.Head, latest.Commit)), gate.ShortSHA(head)), false, nil
	}
	return fmt.Sprintf("head %s has no approved desk review and no supervisor go for that exact head", gate.ShortSHA(head)), false, nil
}

// trustedReviews are p's reviews with a verdict the desk trusts
// (loop.DeskVerdicts): the authors are supervisor.ownership.authors, else the
// login gh acts as, read only when a verdict comes from a non-member.
func (a *app) trustedReviews(ctx context.Context, p superloop.Pull) ([]superloop.Review, error) {
	var authors []string
	if a != nil && a.cfg != nil {
		authors = a.cfg.Supervisor.Ownership.Authors
	}
	if len(authors) == 0 && slices.ContainsFunc(p.Reviews, func(r superloop.Review) bool { return r.Verdict != "" && !r.Member }) {
		login, err := loop.ReadDeskLogin(ctx)
		if err != nil {
			return nil, err
		}
		authors = []string{login}
	}
	var out []superloop.Review
	for _, r := range loop.DeskVerdicts(p.Reviews, authors) {
		if r.Verdict != "" {
			out = append(out, r)
		}
	}
	return out, nil
}

// gateMergeHeadAuthority checks merge head authority for a gated pull request
// merge across all gate routes.
func gateMergeHeadAuthority(ctx context.Context, a *app, repo string, pr int, targetHead string) (why string, hasExplicitGo bool, err error) {
	if pr <= 0 {
		return "", false, nil
	}
	ref := fmt.Sprintf("%s#%d", repo, pr)
	p, err := loop.ReadLoopPull(ctx, ref)
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
