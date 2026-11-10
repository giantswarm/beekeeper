package cmd

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// Optional remote observations must leave time for planning and cancellation.
const loopPlanningObservationTimeout = 20 * time.Second

const deliveryReleaseEvery = 5 * time.Minute
const deliveryReleaseReads = 2

// readDeliveryRelease uses REST only. The newest tag may be a prerelease;
// existence alone is insufficient: compare must prove it contains this merge.
var readDeliveryRelease = func(ctx context.Context, p superloop.Pull) (string, string, error) {
	r, ok := parseRef(p.Ref)
	if !ok || p.MergeCommit == "" || p.Base == "" {
		return "", "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	branch, err := github.ReadBranchRelease(ctx, github.RunGH, r.Repo, p.Base)
	if err != nil {
		return "", "", err
	}
	if !branch.Auto && !branch.Plugins && !branch.ReleasePR {
		// Missing workflow files and an unreadable repository both return 404.
		// Prove the branch is readable before treating it as a non-releasing one.
		raw, err := github.RunGH(ctx, "api", "repos/"+r.Repo+"/branches/"+url.PathEscape(p.Base), "--jq", ".name")
		if err != nil {
			return "", "", err
		}
		if strings.TrimSpace(string(raw)) != p.Base {
			return "", "", fmt.Errorf("release branch observation is incomplete")
		}
		return state.DeliveryNoRelease, "", nil
	}
	raw, err := github.RunGH(ctx, "api", "repos/"+r.Repo+"/tags?per_page=1", "--jq", ".[0].name // empty")
	if err != nil {
		return "", "", err
	}
	tag := strings.TrimSpace(string(raw))
	if tag == "" {
		return "", "", nil
	}
	contains, err := releaseTagContains(ctx, p.Ref, p.MergeCommit, tag)
	if err != nil || !contains {
		return "", "", err
	}
	return verdictAvailable, tag, nil
}

type deliveryReleaseResult struct {
	verdict, tag string
	at           time.Time
}

func (l *supervisorLoop) observeDeliveries(ctx context.Context, in superloop.Input, unread map[string]bool) error {
	ctx, cancel := context.WithTimeout(ctx, loopPlanningObservationTimeout)
	defer cancel()
	l.mu.Lock()
	in.OpenPulls = slices.Clone(l.openPulls)
	l.mu.Unlock()
	st, err := l.a.store.Read()
	if err != nil {
		return err
	}
	in.ClosedIssues = map[string]bool{}
	// Reuse successful issue observations and the existing fail-open issue reader.
	// An unreadable issue is never evidence that its delivery ended.
	l.mu.Lock()
	for ref, o := range l.opens {
		if !o.open && in.Now.Sub(o.at) < loopOpenEvery {
			in.ClosedIssues[ref] = true
		}
	}
	l.mu.Unlock()
	// Leave half the planning allowance for release probes. Otherwise slow
	// issue reads can consume the whole context before any release gets a
	// turn, retaining completed deliveries in WIP indefinitely.
	issueCtx, cancelIssues := context.WithTimeout(ctx, loopPlanningObservationTimeout/2)
	defer cancelIssues()
	issueReads := 0
	for _, d := range st.Deliveries {
		if issueCtx.Err() != nil || issueReads >= sweepReadsPerPass {
			break
		}
		if d.Active() && len(d.Pulls) == 0 {
			issueReads++
		}
		if d.Active() && len(d.Pulls) == 0 && !l.open(issueCtx, d.Ref) {
			in.ClosedIssues[d.Ref] = true
		}
	}
	cancelIssues()
	preview := superloop.Deliveries(in, st.Deliveries)
	hasReleaseProbe := ctx.Err() == nil && slices.ContainsFunc(preview, func(d state.Delivery) bool {
		if d.Step != state.DeliveryReleasing || (!d.ReleaseChecked.IsZero() && in.Now.Sub(d.ReleaseChecked) < deliveryReleaseEvery) {
			return false
		}
		return slices.ContainsFunc(d.Pulls, func(ref string) bool {
			p, ok := in.Pulls[ref]
			return ok && p.State == superloop.Merged
		})
	})
	results := map[string]deliveryReleaseResult{}
	if hasReleaseProbe {
		// Commit discovered identities before any release probe so its reservation
		// and step event survive a crash even for a newly observed external merge.
		if err := l.a.store.Update(func(current *state.State) ([]state.Event, error) {
			next := superloop.Deliveries(in, current.Deliveries)
			events := deliveryObservationEvents(current.Deliveries, next, in.Now)
			current.Deliveries = next
			return events, nil
		}); err != nil {
			return err
		}
		st, err = l.a.store.Read()
		if err != nil {
			return err
		}
		next := slices.Clone(st.Deliveries)
		// Old failures must not repeatedly consume the bounded probe allowance.
		// With a backlog, a pass can outlast deliveryReleaseEvery: iterating in
		// ledger order then retries its first entries forever while later
		// published releases keep occupying WIP. Oldest attempts go first;
		// zero timestamps naturally put never-probed releases ahead of retries.
		slices.SortStableFunc(next, func(a, b state.Delivery) int {
			return a.ReleaseChecked.Compare(b.ReleaseChecked)
		})
		// One bounded attempt per pull, shared by all its issues, persisted even on
		// error so process restarts cannot turn an outage into a request storm.
		for _, d := range next {
			if d.Step != state.DeliveryReleasing {
				continue
			}
			for _, ref := range d.Pulls {
				if ctx.Err() != nil || len(results) >= deliveryReleaseReads {
					break
				}
				if _, ok := results[ref]; ok {
					continue
				}
				if !d.ReleaseChecked.IsZero() && in.Now.Sub(d.ReleaseChecked) < deliveryReleaseEvery {
					continue
				}
				p, ok := in.Pulls[ref]
				if !ok || p.State != superloop.Merged {
					continue
				}
				results[ref] = deliveryReleaseResult{at: in.Now}
				if err := l.a.store.Update(func(current *state.State) ([]state.Event, error) {
					for i := range current.Deliveries {
						if slices.Contains(current.Deliveries[i].Pulls, ref) {
							current.Deliveries[i].ReleaseChecked = in.Now
						}
					}
					return nil, nil
				}); err != nil {
					return err
				}
				verdict, tag, err := readDeliveryRelease(ctx, p)
				if err != nil {
					l.sayOnce("delivery release "+ref, "SUPERVISOR delivery release unread for %s: %v", ref, err)
				}
				results[ref] = deliveryReleaseResult{verdict: verdict, tag: tag, at: in.Now}
			}
		}
	}
	return l.a.store.Update(func(current *state.State) ([]state.Event, error) {
		// Derive again under the lock: concurrent starts and gate verdicts survive.
		next := superloop.Deliveries(in, current.Deliveries)
		for i := range next {
			for _, ref := range next[i].Pulls {
				if r, ok := results[ref]; ok {
					next[i].ReleaseChecked = r.at
					if r.verdict != "" && next[i].Step == state.DeliveryReleasing && (next[i].Verdict != state.DeliveryReleaseFailed || r.tag != next[i].Release) {
						applyDeliveryVerdict(&next[i], r.verdict, r.tag, in.Now)
					}
				}
			}
		}
		events := deliveryObservationEvents(current.Deliveries, next, in.Now)
		current.Deliveries = next
		complete := len(unread) == 0 && !l.sweepIncomplete
		for _, ref := range in.OpenPulls {
			if reason, _ := in.Desk.Unread(ref); reason != "" {
				continue
			}
			if _, ok := in.Pulls[ref]; !ok {
				complete = false
			}
		}
		if complete {
			current.DeliveriesAt = in.Now
		}
		return events, nil
	})
}

func applyDeliveryVerdict(d *state.Delivery, verdict, tag string, now time.Time) {
	d.Verdict, d.Release = verdict, tag
	if verdict == verdictAvailable || verdict == state.DeliveryNoRelease {
		if d.Step != state.DeliveryDone {
			d.Since = now
		}
		d.Step = state.DeliveryDone
	} else {
		if d.Step != state.DeliveryReleasing {
			d.Since = now
		}
		d.Step = state.DeliveryReleasing
	}
}

// recordDeliveryRelease is called before the lane can forget its merge.
func recordDeliveryRelease(st *state.State, repo string, pr int, out merge.Outcome, handCut string, now time.Time) []state.Event {
	if !out.Merged || pr == 0 {
		return nil
	}
	ref := fmt.Sprintf("%s#%d", repo, pr)
	prev := slices.Clone(st.Deliveries)
	verdict := verdictUnconfirmed
	switch {
	case out.ReleaseFailed:
		verdict = state.DeliveryReleaseFailed
	case out.Unconfirmed:
	case out.NoRelease || handCut != "":
		verdict = state.DeliveryNoRelease
	case out.Release != "":
		verdict = verdictAvailable
	}
	for i := range st.Deliveries {
		d := &st.Deliveries[i]
		if slices.Contains(d.Pulls, ref) || strings.EqualFold(d.Ref, ref) {
			applyDeliveryVerdict(d, verdict, out.Release, now)
		}
	}
	return state.DeliveryEvents(prev, st.Deliveries, loopParty)
}

// A PR seed resolved to issues (or later found outside ownership) disappears
// from the list. Close its observation interval in the event history as well.
func deliveryObservationEvents(prev, next []state.Delivery, now time.Time) []state.Event {
	eventNext := slices.Clone(next)
	for _, old := range prev {
		if old.Active() && !slices.ContainsFunc(next, func(d state.Delivery) bool { return strings.EqualFold(d.Ref, old.Ref) }) {
			old.Step, old.Since, old.Waits = state.DeliveryDropped, now, "observation identity resolved or outside desk ownership"
			eventNext = append(eventNext, old)
		}
	}
	return state.DeliveryEvents(prev, eventNext, loopParty)
}
