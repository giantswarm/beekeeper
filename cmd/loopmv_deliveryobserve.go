package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
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

func (l *supervisorLoop) closedIssues(now time.Time) map[string]bool {
	closed := map[string]bool{}
	l.mu.Lock()
	for ref, o := range l.opens {
		if !o.open && now.Sub(o.at) < loopOpenEvery {
			closed[ref] = true
		}
	}
	l.mu.Unlock()
	return closed
}

func (l *supervisorLoop) observeDeliveries(ctx context.Context, in superloop.Input, unread map[string]bool) error {
	commitCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, loopPlanningObservationTimeout)
	defer cancel()
	l.mu.Lock()
	in.OpenPulls = slices.Clone(l.openPulls)
	l.mu.Unlock()
	st, err := l.a.store.Read()
	if err != nil {
		return err
	}
	// Reuse successful issue observations and the existing fail-open issue reader.
	// An unreadable issue is never evidence that its delivery ended.
	in.ClosedIssues = l.closedIssues(in.Now)
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
		if err := updateDeliveryObservation(ctx, l.a.store, in, nil, false); err != nil {
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
	complete := len(unread) == 0 && !l.sweepIncomplete
	for _, ref := range in.OpenPulls {
		if reason, _ := in.Desk.Unread(ref); reason != "" {
			continue
		}
		if _, ok := in.Pulls[ref]; !ok {
			complete = false
		}
	}
	return updateDeliveryObservation(commitCtx, l.a.store, in, results, complete)
}

func updateDeliveryObservation(ctx context.Context, store state.Store, in superloop.Input, results map[string]deliveryReleaseResult, complete bool) error {
	return commitDeliveryObservation(ctx, store, in.Now, complete, func(prev []state.Delivery) []state.Delivery {
		next := superloop.Deliveries(in, prev)
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
		return next
	})
}

var errDeliveryObservationChanged = errors.New("delivery records changed during observation")

// Ownership classification and event derivation can be expensive on a large
// desk. Do both without the exclusive state lock, then compare only the records
// they depended on. Other state writers (agents, timers, notes) never invalidate
// the result and their changes are retained by the commit. A concurrent delivery
// writer requires a fresh derivation, never an overwrite of its verdict/identity.
func commitDeliveryObservation(ctx context.Context, store state.Store, now time.Time, complete bool, derive func([]state.Delivery) []state.Delivery) error {
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		st, err := store.Read()
		if err != nil {
			return err
		}
		next := derive(st.Deliveries)
		events := deliveryObservationEvents(st.Deliveries, next, now)
		if err := ctx.Err(); err != nil {
			return err
		}
		err = store.Update(func(current *state.State) ([]state.Event, error) {
			if !reflect.DeepEqual(current.Deliveries, st.Deliveries) {
				return nil, errDeliveryObservationChanged
			}
			current.Deliveries = next
			if complete && current.DeliveriesAt.Before(now) {
				current.DeliveriesAt = now
			}
			return events, nil
		})
		if !errors.Is(err, errDeliveryObservationChanged) {
			return err
		}
	}
	return errDeliveryObservationChanged
}

// A PR seed resolved to issues (or later found outside ownership) disappears
// from the list. Close its observation interval in the event history as well.
func deliveryObservationEvents(prev, next []state.Delivery, now time.Time) []state.Event {
	eventNext := slices.Clone(next)
	for _, old := range prev {
		if old.Active() && !slices.ContainsFunc(next, func(d state.Delivery) bool { return strings.EqualFold(d.Ref, old.Ref) }) {
			old.Step, old.Since, old.Waits = state.DeliveryDropped, now, "observation identity resolved or outside desk ownership"
			old.Outcome, old.Evidence, old.OutcomeAt = state.DeliveryDropped, old.Waits, now
			eventNext = append(eventNext, old)
		}
	}
	return state.DeliveryEvents(prev, eventNext, loopParty)
}

// deliveryTrackedPulls adds the pull requests of active deliveries to the
// pass's tracked references. A pull request the desk leaves unread (outside
// github.allowedOrgs or in supervisor.ownership.never) stays out: the watcher
// never observes it, so tracking it would leave it unread in every pass and
// no delivery observation would ever complete ("wip stale").
func deliveryTrackedPulls(tracked []string, deliveries []state.Delivery, desk superloop.Desk) []string {
	for _, d := range deliveries {
		if !d.Active() {
			continue
		}
		for _, ref := range d.Pulls {
			if deliveryPullUnread(desk, ref) || slices.Contains(tracked, ref) {
				continue
			}
			tracked = append(tracked, ref)
		}
	}
	return tracked
}

// deliveryPullUnread reports whether desk leaves the pull request ref unread,
// judged on its normalized repository name as supervisorLoop.unreadRepo does.
func deliveryPullUnread(desk superloop.Desk, ref string) bool {
	r, ok := parseRef(ref)
	if !ok {
		return false
	}
	reason, _ := desk.Unread(refName(r))
	return reason != ""
}
