package superloop

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// OverlapWait is how long a worker may wait on an overlapping pull request
// with no progress before the loop gives that pull request an owner.
const OverlapWait = 30 * time.Minute

// HasOverlapWait reports whether any worker on the roster is waiting on
// another pull request.
func HasOverlapWait(workers []Worker, taken []Taken, answers []Answer) bool {
	l := newLedger(taken, answers)
	return slices.ContainsFunc(workers, func(w Worker) bool { return waitsOnOtherPull(w, l) })
}

func ownPullWait(waits string) bool {
	w := strings.ToLower(strings.TrimSpace(waits))
	return strings.HasPrefix(w, "desk review") || strings.HasPrefix(w, "ci pending")
}

// waitsOnOtherPull reports whether w is plain-parked on another worker's pull
// request rather than its own.
func waitsOnOtherPull(w Worker, l ledger) bool {
	ref := w.ParkedOn
	if !IsPull(ref) || w.ReviewRequested || strings.HasPrefix(w.Name, "Review ") {
		return false
	}
	if w.Waits != "" && ownPullWait(w.Waits) {
		return false
	}
	if owner := l.reviewer[ref]; owner != "" {
		return !strings.EqualFold(owner, w.Name)
	}
	if w.Serves != "" {
		return !strings.EqualFold(w.Serves, ref)
	}
	if w.Waits != "" {
		return true
	}
	return w.Name != "" && w.Name != "worker one" && !strings.EqualFold(w.Name, PullWorkerName(ref))
}

// planOverlap orders overlapping pull requests by board rank and ensures that
// the first pull request has a live owner when a worker parks on ref and ref's
// owner is gone or ref has made no progress for OverlapWait.
func planOverlap(in Input, l ledger, ref string, p Pull, parked map[string]Worker, hasPlanned func(string) bool, add func(Action)) {
	if p.State != Open || p.Head == "" {
		return
	}
	if o, _, _ := in.Desk.owner(p, l.overridden[ref]); o != ours {
		return
	}
	// Normalize issue-based serve records to the observed pull request. Keep
	// the PR identity for tie-breaking, action keys and progress tracking.
	pullRef := func(r string) string {
		if r == "" {
			return ""
		}
		if _, ok := in.Pulls[r]; ok {
			return r
		}
		for key := range in.Pulls {
			if strings.EqualFold(key, r) {
				return key
			}
		}
		var matches []string
		for key, pull := range in.Pulls {
			if slices.ContainsFunc(pull.Closes, func(issue string) bool { return strings.EqualFold(issue, r) }) {
				matches = append(matches, key)
			}
		}
		slices.Sort(matches)
		if len(matches) > 0 {
			return matches[0]
		}
		return r
	}
	// GitHub dependencies connect issues only. PRs without a closing issue
	// still need an owner, but cannot participate in a board dependency.
	issueRef := func(r string) string {
		for key, pull := range in.Pulls {
			if strings.EqualFold(key, r) {
				if len(pull.Closes) > 0 {
					return pull.Closes[0]
				}
				return ""
			}
		}
		return r
	}
	addOwner := func(a Action) {
		owner, blocked := issueRef(a.Ref), issueRef(a.Block)
		if owner != "" && blocked != "" && !strings.EqualFold(owner, blocked) {
			a.OwnerIssue, a.BlockIssue = owner, blocked
		}
		add(a)
	}
	w, isParked := parked[ref]
	if !isParked {
		for k, parkedWorker := range parked {
			if strings.EqualFold(k, ref) {
				w, isParked = parkedWorker, true
				break
			}
		}
	}
	rs := l.rounds[ref]
	for _, waiter := range in.Workers {
		if !strings.EqualFold(waiter.ParkedOn, ref) || waiter.ReviewRequested || !waitsOnOtherPull(waiter, l) {
			continue
		}
		if early, _ := in.Desk.early(waiter.ParkedAt); early {
			continue
		}
		serves := pullRef(waiter.Serves)
		if strings.EqualFold(serves, ref) {
			serves = ""
		}
		if serves != "" {
			mutual := slices.ContainsFunc(in.Workers, func(other Worker) bool {
				return !strings.EqualFold(other.Name, waiter.Name) &&
					strings.EqualFold(pullRef(other.ParkedOn), serves) && !other.ReviewRequested &&
					(strings.EqualFold(pullRef(other.Serves), ref) || strings.EqualFold(other.Name, l.reviewer[ref]) || strings.EqualFold(other.Name, PullWorkerName(ref)))
			})
			if mutual && strings.ToLower(serves) < strings.ToLower(ref) {
				if hasPlanned(serves) {
					continue
				}
				// The handoff changes the target: validate its observation and
				// ownership independently of the pull request being visited.
				sp, ok := in.Pulls[serves]
				if !ok || sp.State != Open || sp.Head == "" {
					continue
				}
				if o, _, _ := in.Desk.owner(sp, l.overridden[serves]); o != ours {
					continue
				}
				addOwner(Action{
					Key:     ownerKey(serves, sp.Head, waiter.ParkedAt),
					Kind:    KindOwner,
					Ref:     serves,
					Block:   ref,
					URL:     sp.URL,
					Agent:   waiter.Name,
					Head:    sp.Head,
					Message: fmt.Sprintf("%s goes ahead of %s: %s waits for %s; finish and merge %s first, then %s follows.", serves, ref, ref, serves, serves, ref),
				})
				continue
			}
		}
		if hasPlanned(ref) {
			continue
		}
		owner := w.Name
		if owner == "" || strings.EqualFold(owner, waiter.Name) {
			owner = l.reviewer[ref]
		}
		if owner == "" || strings.EqualFold(owner, waiter.Name) {
			owner = PullWorkerName(ref)
		}
		var liveOwner *Worker
		for i := range in.Workers {
			o := &in.Workers[i]
			if strings.EqualFold(o.Name, waiter.Name) {
				continue
			}
			if strings.EqualFold(o.Name, owner) || strings.EqualFold(o.Name, PullWorkerName(ref)) || strings.EqualFold(pullRef(o.Serves), ref) {
				liveOwner = o
				if owner == "" || strings.EqualFold(owner, PullWorkerName(ref)) {
					owner = o.Name
				}
				break
			}
		}
		roundRunning := false
		var lastVerdict *Review
		if len(rs) > 0 {
			last := rs[len(rs)-1]
			lastVerdict = p.verdict(last, l.pushed(p))
			if lastVerdict == nil {
				done := slices.ContainsFunc(in.Events, func(e Event) bool {
					return e.Verb == VerbDone && e.Agent == ReviewerName(ref, len(rs)) && e.At.After(last.at)
				})
				roundRunning = !done
			}
		}
		hasActiveUnparkedOwner := liveOwner != nil && liveOwner.ParkedOn == ""
		ownerGone := !hasActiveUnparkedOwner && (!isParked && !roundRunning ||
			p.Draft || p.Mergeable == mergeableConflicting ||
			(lastVerdict != nil && lastVerdict.Verdict == ChangesRequested && l.has(forwardKey(ref, lastVerdict.ID)) && (!isParked || !w.ParkedAt.After(l.keys[forwardKey(ref, lastVerdict.ID)].At))))
		stalled := false
		if !in.Now.IsZero() && !waiter.ParkedAt.IsZero() && !in.Now.Before(waiter.ParkedAt.Add(OverlapWait)) {
			last := pullProgress(p, l, w, isParked, liveOwner)
			stalled = last.IsZero() || !in.Now.Before(last.Add(OverlapWait))
		}
		if !ownerGone && !stalled {
			continue
		}
		addOwner(Action{
			Key:     ownerKey(ref, p.Head, waiter.ParkedAt),
			Kind:    KindOwner,
			Ref:     ref,
			Block:   serves,
			URL:     p.URL,
			Agent:   owner,
			Head:    p.Head,
			Message: ownerMessage(p, waiter, serves, stalled && !ownerGone),
		})
	}
}

func pullProgress(p Pull, l ledger, w Worker, isParked bool, liveOwner *Worker) time.Time {
	var last time.Time
	if isParked && w.ParkedAt.After(last) {
		last = w.ParkedAt
	}
	if liveOwner != nil && liveOwner.LastActive.After(last) {
		last = liveOwner.LastActive
	}
	for _, at := range l.acts[p.Ref] {
		if at.After(last) {
			last = at
		}
	}
	for _, r := range p.Reviews {
		if r.At.After(last) {
			last = r.At
		}
	}
	return last
}

func ownerMessage(p Pull, waiter Worker, blocked string, stalled bool) string {
	why := fmt.Sprintf("%s is parked on %s, which has no live owner", waiter.Name, p.URL)
	if stalled {
		why = fmt.Sprintf("%s has waited on %s for %s with no progress", waiter.Name, p.URL, OverlapWait)
	}
	if blocked != "" {
		why += fmt.Sprintf(" (%s waits for %s)", blocked, p.Ref)
	}
	return fmt.Sprintf("%s: take ownership of %s at head %s, bring it to green CI and request its review (or merge it once approved) so the waiting work can proceed.", why, p.URL, short(p.Head))
}
