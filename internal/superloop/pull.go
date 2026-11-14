package superloop

import (
	"cmp"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
)

// MaxReviews is how many review rounds the loop starts for one pull
// request, whatever happens. A pull request gets one desk review: once a
// round gave a verdict, no further round starts
// (https://github.com/giantswarm/beekeeper/issues/3861). A second round
// starts only when the first ended without any verdict, so the one review
// still has to come. Every later head merges on the loop's go: the
// reviewer's fixes, the worker's fixes of findings the reviewer left, a
// merge of the base, a rebase, a conflict's resolution. The loop never asks
// a person whether to merge.
const MaxReviews = 2

// reviewed is the verdict of the pull request's one desk review: the
// latest one any of its rounds rs gave, and that round's number from 1; 0
// and nil before any round gave one. Once it is there, no review round
// starts again.
func (p Pull) reviewed(rs []round, pushed func(Review) bool) (int, *Review) {
	for i, rd := range slices.Backward(rs) {
		if v := p.verdict(rd, pushed); v != nil {
			return i + 1, v
		}
	}
	return 0, nil
}

// verdictSlack is how much earlier than its round's start a verdict may
// be stamped: GitHub's clock and the machine's differ a little.
const verdictSlack = 30 * time.Second

// verdict is round rd's verdict: the latest review with one on the round's
// head submitted after the round started; nil while the round runs. A
// worker that pushes while its round runs has the newer head reviewed: with
// none on the round's head, the latest verdict submitted before the next
// round started on a head no earlier round opened on is the round's, on its
// own head, when pushed says that head was pushed during the round. A
// verdict on any other head (one from before the round, a teammate's late
// review of an older head), of an earlier or a later round, or of nobody's
// round never counts, nor does one of a head its author had replaced when it
// came. A later round's head is that round's even when its review came a
// moment before the loop logged the round's start.
func (p Pull) verdict(rd round, pushed func(Review) bool) *Review {
	since := rd.at.Add(-verdictSlack)
	var onHead *Review
	for i := len(p.Reviews) - 1; i >= 0; i-- {
		if r := p.Reviews[i]; r.Verdict != "" && !r.Replaced && sameHead(r.Head, rd.head) && !r.At.Before(since) && (rd.until.IsZero() || r.At.Before(rd.until)) {
			onHead = &p.Reviews[i]
			break
		}
	}
	for i := len(p.Reviews) - 1; i >= 0; i-- {
		r := p.Reviews[i]
		if r.Verdict == "" || r.Replaced || r.Head == "" || r.At.Before(since) || !rd.until.IsZero() && !r.At.Before(rd.until) {
			continue
		}
		if onHead != nil && (r.At.Before(onHead.At) || r.Verdict != ChangesRequested) {
			continue
		}
		if !slices.ContainsFunc(rd.earlier, func(h string) bool { return sameHead(h, r.Head) }) && !slices.ContainsFunc(rd.later, func(h string) bool { return sameHead(h, r.Head) }) && pushed(r) {
			return &p.Reviews[i]
		}
	}
	return onHead
}

// baseMerged reports whether p's head moved past approved by bringing in
// the base alone.
func (in Input) baseMerged(p Pull, approved string) bool {
	return in.BaseMerged != nil && approved != "" && !sameHead(approved, p.Head) && in.BaseMerged(p, approved)
}

// Replaced reports whether r, a review of p, judged a head its author had
// replaced when r came: GitHub attached r to another head than the one its
// "Head reviewed:" line names, that head carried a change past the judged
// one, and so does p's head. Its round ended when the head moved, without a
// verdict. A head that only brings the base into the judged one (baseMerged;
// nil: none does) is still that head: a review GitHub attached to such a head
// stays a verdict also once a later head carries a change of its own.
func Replaced(p Pull, r Review, baseMerged func(Pull, string) bool) bool {
	if r.Verdict == "" || r.Commit == "" || r.Head == "" || sameHead(r.Commit, r.Head) || sameHead(r.Head, p.Head) {
		return false
	}
	if baseMerged == nil {
		return true
	}
	then := p
	then.Head = r.Commit
	return !baseMerged(then, r.Head) && (sameHead(r.Commit, p.Head) || !baseMerged(p, r.Head))
}

// replaced is Replaced with the input's BaseMerged, except when the loop
// already acted on r or its head (l.pushed): a verdict the loop acted on
// judged the head that was current then, and stays the pull request's one
// desk review once a repair or fix moves the head on.
func (in Input) replaced(l ledger, p Pull, r Review) bool {
	return !l.pushed(p)(r) && Replaced(p, r, in.BaseMerged)
}

// markReplaced marks the reviews of in.Pulls that judged a replaced head
// (replaced), on copies: the caller's pulls stay as they are.
func (in Input) markReplaced(l ledger) Input {
	var pulls map[string]Pull
	for ref, p := range in.Pulls {
		p.Ref = cmp.Or(p.Ref, ref)
		var reviews []Review
		for i, r := range p.Reviews {
			if r.Replaced || !in.replaced(l, p, r) {
				continue
			}
			if reviews == nil {
				reviews = slices.Clone(p.Reviews)
			}
			reviews[i].Replaced = true
		}
		if reviews == nil {
			continue
		}
		if pulls == nil {
			pulls = maps.Clone(in.Pulls)
		}
		p.Reviews = reviews
		pulls[ref] = p
	}
	if pulls != nil {
		in.Pulls = pulls
	}
	return in
}

// gated reports whether the gate's devctl pr merge serves repo. A
// repository in Admin merges with gh pr merge --admin instead.
func (in Input) gated(repo string) bool {
	return !in.admin(repo) && slices.ContainsFunc(in.DevctlOwners, func(o string) bool { return strings.EqualFold(o, ownerOf(repo)) })
}

// admin reports whether repo merges with gh pr merge --admin after the
// approved review.
func (in Input) admin(repo string) bool {
	return slices.ContainsFunc(in.Admin, func(r string) bool { return strings.EqualFold(r, repo) })
}

// ownerOf is owner/repo's owner.
func ownerOf(repo string) string {
	owner, _, _ := strings.Cut(repo, "/")
	return owner
}

func planPull(in Input, l ledger, ref string, p Pull, parked map[string]Worker, ask question, asked bool, leave func(string, string, string, string, string), add func(Action)) {
	w, isParked := parked[ref]
	if !isParked && slices.ContainsFunc(in.Workers, func(w Worker) bool { return w.Name == l.reviewer[ref] && w.ParkedOn == ref && !w.ReviewRequested }) {
		return
	}
	if early, _ := in.Desk.early(w.ParkedAt); isParked && early {
		return
	}
	agent := w.Name
	if agent == "" {
		agent = l.reviewer[ref]
	}
	if agent == "" || p.State != Open || p.Head == "" {
		return
	}
	base := Action{Ref: ref, URL: p.URL, Agent: agent, Head: p.Head}
	switch o, reason, why := in.Desk.owner(p, l.overridden[ref]); o {
	case nobodys:
		leave(ref, p.URL, agent, reason, why)
		return
	case theirs:
		leave(ref, p.URL, agent, reason, why)
		planRed(l, p, w, isParked, add)
		planTheirs(in, l, p, w, isParked, base, ask, asked, add)
		return
	case unknown:
		return
	}
	rs := l.rounds[ref]
	if n, v := p.reviewed(rs, l.pushed(p)); v != nil && v.Verdict == Approved {
		base.RepairAgent = ReviewerName(ref, n)
	}
	// Approved heads use the merge executor's durable repair handoff, which
	// retains the approval and retries a capacity-limited fixer only once.
	approved := false
	if len(rs) > 0 {
		v := p.verdict(rs[len(rs)-1], l.pushed(p))
		approved = v != nil && v.Verdict == Approved && sameHead(v.Head, p.Head)
	}
	if isParked && !approved && (p.Mergeable == mergeableConflicting || p.MergeState == mergeStateDirty) {
		if sameHead(w.ParkedHead, p.Head) {
			a := base
			a.Key = fmt.Sprintf("conflict %s@%s %s", ref, p.Head, w.ParkedAt.UTC().Format(time.RFC3339Nano))
			a.Kind = KindRework
			a.Agent = cmp.Or(a.RepairAgent, a.Agent)
			a.Message = fmt.Sprintf("%s at head %s conflicts with its base. Fetch origin, merge the base into the branch, resolve the conflicts without local tests or lints and push, then immediately park again: the loop waits for green CI, and a pull request that had its one desk review merges without a second one. Never force-push.", p.URL, short(p.Head))
			if !l.has(a.Key) {
				add(a)
			}
		}
		return
	}
	// The reviewer fixes its findings on the branch and approves the head
	// it pushed, before that head's CI ends. The worker's park still names
	// the head it pushed: use the approved head for CI, but send failures
	// to its reviewer, from the verdict on, once each.
	ciWorker := w
	if isParked && !sameHead(w.ParkedHead, p.Head) && len(rs) > 0 {
		if v := p.verdict(rs[len(rs)-1], l.pushed(p)); v != nil && v.Verdict == Approved && sameHead(v.Head, p.Head) {
			ciWorker.ParkedHead, ciWorker.ParkedAt = p.Head, v.At
		}
	}
	ciWorker.Name = cmp.Or(base.RepairAgent, ciWorker.Name)
	planRed(l, p, ciWorker, isParked, func(a Action) {
		a.RepairAgent = base.RepairAgent
		add(a)
	})
	if head := l.goHead[ref]; !in.RequireReview && p.Green && head != "" && !sameHead(head, p.Head) && !slices.ContainsFunc(p.Reviews, func(r Review) bool {
		return r.Verdict == ChangesRequested && (sameHead(r.Head, p.Head) || !r.At.Before(l.keys[goKey(ref, head)].At))
	}) && in.baseMerged(p, head) {
		add(goAction(in, base, fmt.Sprintf("head %s had the merge go; head %s only brings %s into it", short(head), short(p.Head), baseName(p))))
		return
	}
	review := func() {
		a := base
		a.Key, a.Kind, a.Round, a.SelfHosted = reviewKey(ref, p.Head), KindReview, len(rs)+1, w.SelfHosted
		add(a)
	}
	// handed reports whether the park hands p's head over for its review:
	// the head the park verified, one that only brings the base into it, or
	// any head of an older park that verified none. A head its author pushed
	// past the park is still being worked on: neither a review round nor a
	// merge go starts on it before the author parks on it.
	handed := func() bool {
		return isParked && (w.ParkedHead == "" || sameHead(w.ParkedHead, p.Head) || in.baseMerged(p, w.ParkedHead))
	}
	unhanded := func() {
		leave(ref, p.URL, agent, leftUnhanded(p.Head), fmt.Sprintf("head %s moved past head %s, which the park of %s handed over for the desk review: its author still works on it, and no review round or merge go starts before it parks on the head it pushed", short(p.Head), short(w.ParkedHead), w.Name))
	}
	if len(rs) == 0 {
		if !p.Green || !isParked {
			return
		}
		if !handed() {
			unhanded()
			return
		}
		review()
		return
	}
	last := rs[len(rs)-1]
	// A head past the last round's asks for a park since that round
	// opened that handed it over, or for a verified park of that very
	// head: a worker stopped on such a park cannot repeat it. A park of
	// any other head from before the round is stale.
	requested := sameHead(w.ParkedHead, p.Head) || w.ParkedAt.After(last.at) && handed()
	v := p.verdict(last, l.pushed(p))
	if v != nil && v.Verdict == Approved {
		for i := len(p.Reviews) - 1; i >= 0; i-- {
			r := p.Reviews[i]
			if !r.At.After(v.At) || r.Verdict == "" || r.Head == "" || r.Replaced {
				continue
			}
			if r.Verdict == ChangesRequested && !slices.ContainsFunc(last.earlier, func(h string) bool { return sameHead(h, r.Head) }) && (sameHead(r.Head, v.Head) || l.pushed(p)(r)) {
				v = &p.Reviews[i]
			}
			break
		}
	}
	if v == nil {
		// The round runs until its reviewer reports done. One done
		// without a verdict on the round's head or on the head now
		// reviewed a head the worker pushed in between: the newest
		// head gets its round. A reviewer that left the roster without
		// a verdict (its start failed, or it ended without one) reviews
		// nothing more: OrphanWait after the round opened, the head it
		// left gets a new round too, and that round's reservation
		// releases the gone reviewer's assignment of the head.
		reviewer := ReviewerName(ref, len(rs))
		done := slices.ContainsFunc(in.Events, func(e Event) bool {
			return e.Verb == VerbDone && e.Agent == reviewer && e.At.After(last.at)
		})
		gone := !in.Now.IsZero() && !in.Now.Before(last.at.Add(OrphanWait)) && !slices.ContainsFunc(in.Workers, func(o Worker) bool {
			return strings.EqualFold(o.Name, reviewer)
		})
		same := sameHead(p.Head, last.head)
		if !p.Green || !isParked {
			return
		}
		if !requested {
			// A park since the round opened whose head the author pushed
			// past: it still works on the pull request. A park from
			// before the round (a reviewer's own published fix moved the
			// head) is the round's, which runs on.
			if !same && w.ParkedAt.After(last.at) {
				unhanded()
			}
			return
		}
		// A head its author moved past the round's with a change of its
		// own ends the round early, without a verdict: the reviewer
		// judges a head that is no longer the one to merge, and what it
		// posts after the move is no verdict (Input.replaced). While one
		// of MaxReviews is left, the next round starts on the new head
		// once its author handed it over, without waiting for the old
		// reviewer's done; the loop retires that reviewer once its turn
		// ends. A head that only brings the base in is the round's still.
		moved := !same && (len(rs) < MaxReviews || in.RequireReview) && !in.baseMerged(p, last.head)
		if !gone && !moved && (!done || same) {
			return
		}
		// An earlier round's verdict was the pull request's one review:
		// the loop acts on it and starts no other review, never asking a
		// person and never merging without a verdict.
		if i, ev := p.reviewed(rs[:len(rs)-1], l.pushed(p)); ev != nil {
			if ev.Verdict == ChangesRequested && (sameHead(ev.Head, p.Head) || in.baseMerged(p, ev.Head)) {
				a := base
				a.Key, a.Kind = reworkKey(ref, p.Head, w.ParkedAt), KindRework
				a.Message = reworkMessage(p, ev.Head, in.RequireReview) + "\n\nThe review, word for word:\n\n" + ev.Body
				add(a)
				return
			}
			if !in.RequireReview || (ev.Verdict == Approved && in.approvalCovers(p, ev.Head)) {
				add(goAction(in, base, fmt.Sprintf("its one review, round %d, %s head %s and the last reviewer ended or left without a verdict; no second review", i, ev.Verdict, short(ev.Head))))
				return
			}
		}
		roundsOnHead := 0
		for _, rd := range rs {
			if sameHead(rd.head, p.Head) {
				roundsOnHead++
			}
		}
		// No round gave a covering verdict yet: the newest head gets its
		// review while a round is left.
		if len(rs) < MaxReviews || (in.RequireReview && (!same || roundsOnHead < MaxReviews)) {
			if !same {
				review()
				return
			}
			a := base
			a.Key, a.Kind, a.Round, a.SelfHosted = retryKey(ref, p.Head, len(rs)+1), KindReview, len(rs)+1, w.SelfHosted
			add(a)
			return
		}
		leave(ref, p.URL, agent, leftNoVerdict(p.Head), fmt.Sprintf("its %d review rounds ended and none gave a verdict: no merge go comes without a desk review's verdict", len(rs)))
		return
	}
	// reviewed is the head the verdict is on: the round's, or a newer
	// one the worker pushed while the round ran.
	reviewed, moved := v.Head, ""
	if !sameHead(reviewed, last.head) {
		moved = fmt.Sprintf(" (the round opened on head %s)", short(last.head))
	}
	switch v.Verdict {
	case Approved:
		if sameHead(reviewed, p.Head) {
			if p.Green {
				add(goAction(in, base, fmt.Sprintf("review round %d approved head %s%s", len(rs), short(p.Head), moved)))
			}
			return
		}
		// The head moved past the approval. The approval covers a head
		// that only brings the base into the approved one (a merge of
		// the base, a conflict's resolution, a rebase onto it): its go
		// needs no new park, it brings no change of the worker's. Any
		// other head asks for a park since the round opened, and then
		// gets the go too unless merge.requireReview asks for an
		// independent review on the new head.
		if !p.Green {
			return
		}
		if in.NitsChecked != nil && in.NitsChecked(p, reviewed) {
			add(goAction(in, base, fmt.Sprintf("supervisor checked approval nits at head %s against approved head %s", short(p.Head), short(reviewed))))
			return
		}
		if in.baseMerged(p, reviewed) {
			add(goAction(in, base, fmt.Sprintf("review round %d approved head %s; head %s only brings %s into it", len(rs), short(reviewed), short(p.Head), baseName(p))))
			return
		}
		if goHead := l.goHead[ref]; !in.RequireReview && goHead != "" && !sameHead(goHead, reviewed) && !sameHead(goHead, p.Head) && in.baseMerged(p, goHead) {
			add(goAction(in, base, fmt.Sprintf("head %s had the merge go; head %s only brings %s into it", short(goHead), short(p.Head), baseName(p))))
			return
		}
		if !isParked {
			return
		}
		if !requested && w.ParkedAt.After(last.at) {
			unhanded()
			return
		}
		if !requested {
			if in.RequireReview {
				leave(ref, p.URL, agent, leftUnreviewed(p.Head), fmt.Sprintf("head %s moved past head %s, which review round %d approved, and the park on it is from before that round: park on the pull request again for its review", short(p.Head), short(reviewed), len(rs)))
				return
			}
			leave(ref, p.URL, agent, leftUnreviewed(p.Head), fmt.Sprintf("head %s moved past head %s, which review round %d approved, and the park on it is from before that round: park on the pull request again for its merge go (no second review runs)", short(p.Head), short(reviewed), len(rs)))
			return
		}
		if in.RequireReview {
			review()
			return
		}
		// One review, then the fixes and the merge, as the desk's
		// standing rule says. No person is asked whether to merge.
		add(goAction(in, base, fmt.Sprintf("its one review, round %d, approved head %s; head %s merges without a second review", len(rs), short(reviewed), short(p.Head))))
	case ChangesRequested:
		fk := forwardKey(ref, v.ID)
		if !l.has(fk) {
			a := base
			a.Key, a.Kind = fk, KindForward
			a.Message = forwardMessage(p, *v, in.RequireReview)
			add(a)
			return
		}
		// The fixes count once the worker parked after the forward.
		if !isParked || !w.ParkedAt.After(l.keys[fk].At) {
			return
		}
		// A head that only merges the base into the head the findings
		// were about carries no fixes: it is that head, never a go.
		merged := p.Green && in.baseMerged(p, reviewed)
		if sameHead(p.Head, reviewed) || merged {
			// The findings stand unfixed: the worker reworks once per park.
			// Review and rework are the loop's; no merge question goes
			// to the person.
			a := base
			a.Key, a.Kind = reworkKey(ref, p.Head, w.ParkedAt), KindRework
			a.Message = reworkMessage(p, reviewed, in.RequireReview)
			add(a)
			return
		}
		if !p.Green {
			return
		}
		if !handed() {
			unhanded()
			return
		}
		if in.RequireReview {
			review()
			return
		}
		add(goAction(in, base, fmt.Sprintf("its one review, round %d, asked for changes on head %s; head %s carries the fixes and merges without a second review", len(rs), short(reviewed), short(p.Head))))
	}
}

// planRed wakes worker w, parked on p, when a check on the head it parked
// on concluded red: the loop reviews no head that is not green, so the
// worker would wait for nothing. One wake names every check red now that
// no earlier wake of this park named; a check is told once for each head
// and park (a check run again that fails again on the same head is told
// once the worker parked again), and none after the head's merge go.
func planRed(l ledger, p Pull, w Worker, isParked bool, add func(Action)) {
	if !isParked || p.State != Open || p.Head == "" || !sameHead(w.ParkedHead, p.Head) || l.has(goKey(p.Ref, p.Head)) {
		return
	}
	var names, lines []string
	for _, c := range p.Red {
		name := redName(c.Name)
		if name == "" || slices.Contains(names, name) || l.red[redKey(p.Ref, p.Head, w.ParkedAt, name)] {
			continue
		}
		names = append(names, name)
		line := name
		if c.URL != "" {
			line += " " + c.URL
		}
		lines = append(lines, line)
	}
	if len(names) == 0 {
		return
	}
	add(Action{Key: redKey(p.Ref, p.Head, w.ParkedAt, names...), Kind: KindRed, Ref: p.Ref, URL: p.URL, Agent: w.Name, Head: p.Head,
		Message: redMessage(p, lines)})
}

// redMessage reports failed checks to the author before review, and to
// the reviewer after approval.
func redMessage(p Pull, lines []string) string {
	return fmt.Sprintf("CI is red on %s at head %s; no review and no merge go comes until every check is green. Red: %s. "+
		"Read the failure, fix it and push (or rerun a check that failed for no reason of the change), then park again from the worktree: beekeeper agents park --on %s \"desk review\".",
		p.URL, short(p.Head), strings.Join(lines, "; "), p.Ref)
}

// planTheirs plans another team's pull request p: the person's answer about
// its ownership, the owning team's reviews told to the worker parked on it,
// and, once that team let the worker wait OwnerWait without a review, the
// person asked whether the desk takes it over.
func planTheirs(in Input, l ledger, p Pull, w Worker, isParked bool, base Action, ask question, asked bool, add func(Action)) {
	if asked && ask.answer != nil {
		an := *ask.answer
		a := base
		if Overrides(an.Text) {
			a.Key, a.Kind = overrideKey(p.Ref), KindOverride
			a.Message = fmt.Sprintf("%s overrode %s's ownership of %s, answering note #%d: %s; the desk reviews it from here",
				an.By, teamName(p.Team), p.URL, an.Note, oneLine(an.Text))
			add(a)
			return
		}
		if ak := answerKey(p.Ref, an.Note); !l.has(ak) {
			a.Key, a.Kind = ak, KindAnswer
			a.Message = ownershipAnswerMessage(p, an)
			add(a)
		}
	}
	if !isParked {
		return
	}
	reviews := ownerReviews(p, w.ParkedAt)
	for _, r := range reviews {
		a := base
		a.Key, a.Kind, a.Agent = relayKey(p.Ref, r.ID), KindRelay, w.Name
		a.Message = relayMessage(p, r, p.Team)
		add(a)
	}
	if asked || len(reviews) > 0 || ownerApproved(p) || in.Now.Sub(w.ParkedAt) < in.Desk.OwnerWait {
		return
	}
	a := base
	a.Key, a.Kind, a.Agent = ownershipKey(p.Ref), KindOwnership, w.Name
	a.Question, a.Options = OwnershipQuestion(p.URL, p.Team), OwnershipOptions(p.Team)
	a.Message = fmt.Sprintf("%s parked on %s at %s for %s's review, and none came in %s",
		w.Name, p.URL, w.ParkedAt.UTC().Format(time.RFC3339), teamName(p.Team), in.Desk.OwnerWait)
	add(a)
}

// goAction is the merge go for base's head, naming the merge command of
// its repository's path: the gate's devctl pr merge where
// devctl serves it, else gh pr merge
// --match-head-commit (--admin included where merge.admin lists the
// repository).
func goAction(in Input, base Action, why string) Action {
	a := base
	a.Key, a.Kind = goKey(base.Ref, base.Head), KindGo
	var latest *Review
	pull := in.Pulls[base.Ref]
	for i := range pull.Reviews {
		r := &pull.Reviews[i]
		if r.Verdict != "" && (latest == nil || !r.At.Before(latest.At)) {
			latest = r
		}
	}
	if latest != nil {
		a.ReviewID, a.ReviewVerdict = latest.ID, latest.Verdict
	}
	repo, n, _ := strings.Cut(base.Ref, "#")
	if in.gated(repo) {
		a.Message = fmt.Sprintf("Merge go for %s at head %s (%s): merge it now through the gate, `devctl pr merge %s %s`, once `gh pr view` shows head %s. Merge only this green head after the merge-result check passes; never update the branch.",
			base.URL, base.Head, why, repo, n+rebaseGoFlag(in.MergeMethods[repo]), short(base.Head))
		return a
	}
	admin := ""
	if in.admin(repo) {
		admin = " --admin"
	}
	a.Message = fmt.Sprintf("Merge go for %s at head %s (%s): merge it now with `gh pr merge %s --repo %s --%s%s --match-head-commit %s`, then report.", base.URL, base.Head, why, n, repo, cmp.Or(in.MergeMethods[repo], "squash"), admin, base.Head)
	return a
}

// baseName is p's base branch, "the base" when unknown.
func baseName(p Pull) string {
	if p.Base == "" {
		return "the base"
	}
	return p.Base
}

// reworkMessage tells the worker of p, parked again on reviewed, the head
// its one review asked changes on, or on a head that only merges the base
// into it, to fix them.
func (in Input) approvalCovers(p Pull, approved string) bool {
	return approved != "" && (sameHead(approved, p.Head) || (in.NitsChecked != nil && in.NitsChecked(p, approved)) || in.baseMerged(p, approved))
}

func reworkMessage(p Pull, reviewed string, requireReview bool) string {
	at := fmt.Sprintf("at head %s, the head its review asked for changes on", short(p.Head))
	if !sameHead(p.Head, reviewed) {
		at = fmt.Sprintf("at head %s, which only brings %s into head %s, the head its review asked for changes on", short(p.Head), baseName(p), short(reviewed))
	}
	afterPark := "no further review runs, the merge go for the new head follows"
	if requireReview {
		afterPark = "the loop reviews the new head and merges it once approved"
	}
	return fmt.Sprintf("You parked on %s again %s: those findings stand unfixed, and no merge go comes for this head. Nobody else decides it: fix every finding of that review (forwarded to you, and on the pull request), push, and once CI is green on the new head, run `cd \"<worktree>\" && beekeeper agents park --on <owner/repo#n> \"desk review\"` from the published worktree: %s.",
		p.URL, at, afterPark)
}

// oneLine is s on one line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// forwardMessage forwards review r of p word for word: the pull request's
// one review, so the fixes merge on the loop's go without another unless
// merge.requireReview asks for an exact-head review.
func forwardMessage(p Pull, r Review, requireReview bool) string {
	afterPark := "no further review runs, the merge go for the new head follows"
	if requireReview {
		afterPark = "the loop reviews the new head and merges it once approved"
	}
	next := fmt.Sprintf("Fix every finding, push, and once CI is green on the new head, run `cd \"<worktree>\" && beekeeper agents park --on <owner/repo#n> \"desk review\"` from the published worktree: %s.", afterPark)
	return fmt.Sprintf("The review of %s asks for changes on head %s. %s The review, word for word:\n\n%s", p.URL, r.Head, next, r.Body)
}

// short is a commit's short form.
func short(sha string) string { return sha[:min(len(sha), 9)] }

// Verdict is a review's verdict: GitHub's own (APPROVED, CHANGES_REQUESTED),
// else that of a commented review whose first line ends in ": approved" or
// ": changes requested" (a review posted from the author's own account
// cannot approve on GitHub). "" for a review without one.
func Verdict(state, body string) string {
	switch state {
	case "APPROVED":
		return Approved
	case "CHANGES_REQUESTED":
		return ChangesRequested
	case stateCommented:
		// The review guard reads the same line: one parser keeps them alike.
		return Verdict(github.DeskVerdict(body), "")
	}
	return ""
}

// stateCommented is a review GitHub records without a verdict.
const stateCommented = "COMMENTED"

// headLine is a review body's "Head reviewed: `<sha>`" line.
var headLine = regexp.MustCompile("(?mi)^head reviewed:\\s*`?([0-9a-f]{7,40})`?")

// ReviewedHead is the head a review judged: the commit its body names on a
// "Head reviewed:" line, else the commit GitHub recorded it on.
func ReviewedHead(body, commit string) string {
	if m := headLine.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return commit
}

// sameHead reports whether two commits, either of them possibly short, are
// one.
func sameHead(a, b string) bool {
	if len(a) < 7 || len(b) < 7 {
		return a == b && a != ""
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// rebaseGoFlag keeps devctl upstream-line merges on their configured method.
func rebaseGoFlag(method string) string {
	if method == "rebase" {
		return " --rebase"
	}
	return ""
}
