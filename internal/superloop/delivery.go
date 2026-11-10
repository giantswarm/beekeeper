package superloop

import (
	"cmp"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/giantswarm/beekeeper/internal/state"
)

// Delivery is persisted by the caller; derivation has no I/O or planner effects.
type Delivery = state.Delivery

// Deliveries derives observations from the same ledger, pulls and workers as
// Plan. Missing reads retain the previous observation, including terminal state.
func Deliveries(in Input, prev []Delivery) []Delivery {
	l := newLedger(in.Taken, in.Answers)
	refs := make([]string, 0, len(in.Pulls))
	for ref := range in.Pulls {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	pullOwner := make(map[string]owner, len(refs))
	var absorbingRefs []string
	absorbed := map[string]bool{}
	for _, ref := range refs {
		p := in.Pulls[ref]
		o, _, _ := in.Desk.owner(p, l.overridden[ref])
		pullOwner[ref] = o
		if o == ours && len(p.Absorbs) > 0 {
			absorbingRefs = append(absorbingRefs, ref)
		}
		if (p.State == Open || p.State == Merged) && len(p.Absorbs) > 0 {
			baseOwner := o
			if l.overridden[ref] {
				baseOwner, _, _ = in.Desk.owner(p, false)
			}
			if baseOwner == ours {
				for _, r := range p.Absorbs {
					absorbed[r] = true
				}
			}
		}
	}
	out := slices.Clone(prev)
	out = slices.DeleteFunc(out, func(d Delivery) bool {
		if _, ok := in.Pulls[d.Ref]; ok && slices.Contains(d.Pulls, d.Ref) {
			o := pullOwner[d.Ref]
			return o == nobodys || o == theirs
		}
		return false
	})
	ensure := func(ref string) int {
		ref = strings.ToLower(ref)
		i := slices.IndexFunc(out, func(d Delivery) bool { return strings.EqualFold(d.Ref, ref) })
		if i < 0 {
			out = append(out, Delivery{Ref: ref, Pulls: []string{}, Step: state.DeliveryUnknown, Since: in.Now, Started: in.Now})
			i = len(out) - 1
		}
		return i
	}
	// OpenPulls is the already ownership-filtered sweep, including unread seeds.
	for _, ref := range in.OpenPulls {
		if reason, _ := in.Desk.unread(ref); reason != "" {
			continue
		}
		if _, ok := in.Pulls[ref]; ok {
			if pullOwner[ref] != ours {
				continue
			}
		}
		if slices.ContainsFunc(out, func(d Delivery) bool { return slices.Contains(d.Pulls, ref) }) {
			continue
		}
		i := ensure(ref)
		if len(out[i].Pulls) == 0 {
			out[i].Pulls = []string{ref}
		}
	}
	for _, ref := range refs {
		p := in.Pulls[ref]
		if pullOwner[ref] != ours {
			continue
		}
		issues := slices.Clone(p.Closes)
		for _, w := range in.Workers {
			if strings.EqualFold(w.ParkedOn, ref) && w.Serves != "" && !strings.EqualFold(w.Serves, ref) {
				issues = append(issues, w.Serves)
				issues = append(issues, w.Issues...)
			}
		}
		if len(issues) == 0 {
			issues = []string{ref}
		}
		for _, is := range issues {
			i := ensure(is)
			if seed := slices.IndexFunc(prev, func(d Delivery) bool { return strings.EqualFold(d.Ref, ref) }); seed >= 0 {
				old := prev[seed]
				if old.Started.Before(out[i].Started) {
					out[i].Started = old.Started
				}
				if old.DropReason != "" && out[i].DropReason == "" {
					out[i].Step, out[i].Since = old.Step, old.Since
					out[i].DropReason, out[i].DropBy, out[i].DropAt = old.DropReason, old.DropBy, old.DropAt
					out[i].DroppedHeads = maps.Clone(old.DroppedHeads)
				}
			}
			if !slices.Contains(out[i].Pulls, ref) {
				out[i].Pulls = append(slices.Clone(out[i].Pulls), ref)
			}
		}
		// Once the issue link is known the temporary PR seed is absorbed.
		if !slices.Contains(issues, ref) {
			out = slices.DeleteFunc(out, func(d Delivery) bool { return strings.EqualFold(d.Ref, ref) })
		}
	}
	for i := range out {
		d := &out[i]
		d.Pulls = slices.Clone(d.Pulls)
		before := d.Step
		if d.Outcome == state.OutcomeNoChange || d.Outcome == state.OutcomeBlocked || d.Outcome == state.OutcomeStalled {
			continue
		}
		// Preserve unknown ownership on partial reads; positive exclusion applies
		// to linked issue deliveries as well as temporary PR seeds.
		if reason, _ := in.Desk.unread(d.Ref); reason != "" {
			d.OutsideDesk = true
		}
		owned, excluded := false, len(d.Pulls) > 0
		for _, ref := range d.Pulls {
			if _, ok := in.Pulls[ref]; !ok {
				excluded = false
				continue
			}
			o := pullOwner[ref]
			if o == ours {
				owned = true
			}
			if o != nobodys && o != theirs {
				excluded = false
			}
		}
		if owned {
			d.OutsideDesk = false
		} else if excluded {
			d.OutsideDesk = true
		}
		if d.Since.After(in.Now) {
			continue
		}
		// A PR sweep establishes ownership, not intent to work. Migrate old
		// workerless PR seeds too; a new head or explicit loop action adopts it.
		if slices.Contains(d.Pulls, d.Ref) {
			p, read := in.Pulls[d.Ref]
			if read {
				changed := d.Heads[d.Ref] != "" && p.Head != "" && d.Heads[d.Ref] != p.Head
				engaged := d.Agent != "" || changed || len(l.rounds[d.Ref]) > 0 || l.goHead[d.Ref] != ""
				for _, w := range in.Workers {
					engaged = engaged || strings.EqualFold(w.Serves, d.Ref) || strings.EqualFold(w.ParkedOn, d.Ref) || slices.Contains(w.Issues, d.Ref)
				}
				if engaged {
					d.Origin = state.DeliveryOriginStarted
				} else if d.Origin == "" && (d.Step == state.DeliveryUnknown || d.Step == state.DeliveryImplementing) {
					d.Origin = state.DeliveryOriginObserved
				}
			}
		}
		d.Heads = maps.Clone(d.Heads)
		if d.Heads == nil {
			d.Heads = map[string]string{}
		}
		for _, ref := range d.Pulls {
			readAt := in.Now
			if in.PullReadAt != nil {
				readAt = in.PullReadAt[ref]
			}
			if p, ok := in.Pulls[ref]; ok && p.Head != "" && (d.DropReason == "" || !readAt.Before(d.DropAt)) {
				d.Heads[ref] = p.Head
			}
		}
		// A manual drop is tied to the observed heads. Missing observations do
		// not undo it; only positive evidence of a new head opens the delivery.
		if d.DropReason != "" {
			changed := false
			for _, ref := range d.Pulls {
				readAt := in.Now
				if in.PullReadAt != nil {
					readAt = in.PullReadAt[ref]
				}
				if p, ok := in.Pulls[ref]; ok && !readAt.Before(d.DropAt) && p.Head != "" && p.Head != d.DroppedHeads[ref] {
					changed = true
				}
			}
			if !changed {
				continue
			}
			d.DropReason, d.DroppedHeads = "", nil
		}
		var worker *Worker
		for j := range in.Workers {
			w := &in.Workers[j]
			if !w.Done && (strings.EqualFold(w.Serves, d.Ref) || slices.Contains(w.Issues, d.Ref) || slices.Contains(d.Pulls, w.ParkedOn)) {
				worker = w
				break
			}
		}
		if worker != nil {
			d.Agent, d.Waits = worker.Name, worker.Waits
		}
		// A released, ended worker's delivery stays terminal across pull sweeps.
		// A new assignment explicitly opens a fresh lifecycle.
		if d.Abandoned {
			if worker == nil || worker.Saved {
				continue
			}
			d.Abandoned = false
			d.Started = in.Now
		}
		// A combining pull carries deliveries of the original, even when GitHub
		// no longer includes their issue links on the superseded pull.
		for _, ref := range absorbingRefs {
			p := in.Pulls[ref]
			if slices.ContainsFunc(p.Absorbs, func(r string) bool { return slices.Contains(d.Pulls, r) }) && !slices.Contains(d.Pulls, ref) {
				d.Pulls = append(slices.Clone(d.Pulls), ref)
			}
		}
		var observed []Pull
		missing := false
		for _, ref := range d.Pulls {
			if absorbed[ref] {
				continue
			}
			if p, ok := in.Pulls[ref]; ok {
				observed = append(observed, p)
			} else {
				missing = true
			}
		}
		if len(d.Pulls) == 0 {
			if in.ClosedIssues[d.Ref] || deliveryWorkerDone(in, *d) {
				d.Step = state.DeliveryDropped
			} else if worker != nil && !worker.Saved && !deliveryWorkerDone(in, *d) {
				if !d.Active() {
					d.Step = state.DeliveryImplementing
					d.Started = in.Now
				}
			}
		} else {
			open := slices.IndexFunc(observed, func(p Pull) bool { return p.State == Open })
			merged := slices.ContainsFunc(observed, func(p Pull) bool { return p.State == Merged })
			switch {
			case open >= 0:
				d.Step = deliveryPullStep(in, l, observed[open], worker)
				if before == state.DeliveryDropped || before == state.DeliveryDone {
					d.Started = in.Now
					d.Release = ""
					d.Verdict = ""
				}
			case missing: // a failed read never proves an end
			case merged:
				d.Outcome = state.OutcomeMerged
				if d.Evidence == "" || before != state.DeliveryReleasing && before != state.DeliveryDone {
					d.OutcomeAt = in.Now
					for _, p := range observed {
						if p.State == Merged {
							d.Evidence = p.URL
							break
						}
					}
				}
				if d.Step != state.DeliveryDone {
					d.Step = state.DeliveryReleasing
				}
			case len(observed) > 0 && (worker == nil || worker.Saved || worker.Done || deliveryWorkerDone(in, *d)):
				d.Step = state.DeliveryDropped
			case worker != nil:
				d.Step = state.DeliveryImplementing
			}
		}
		if d.Step != before {
			d.Since = in.Now
		}
		sort.Strings(d.Pulls)
	}
	out = slices.DeleteFunc(out, func(d Delivery) bool {
		if !slices.Contains(d.Pulls, d.Ref) {
			return false
		}
		return slices.ContainsFunc(out, func(other Delivery) bool { return other.Ref != d.Ref && slices.Contains(other.Pulls, d.Ref) })
	})
	slices.SortFunc(out, func(a, b Delivery) int { return cmp.Compare(a.Ref, b.Ref) })
	return out
}

func deliveryWorkerDone(in Input, d Delivery) bool {
	if slices.ContainsFunc(in.Workers, func(w Worker) bool {
		return w.Done && (strings.EqualFold(w.Serves, d.Ref) || slices.Contains(w.Issues, d.Ref))
	}) {
		return true
	}
	return slices.ContainsFunc(in.Events, func(e Event) bool { return e.Agent == d.Agent && e.Verb == VerbDone && !e.At.Before(d.Started) })
}

func deliveryPullStep(in Input, l ledger, p Pull, w *Worker) string {
	if p.Mergeable == mergeableConflicting || p.MergeState == mergeStateDirty || len(p.Red) > 0 {
		return state.DeliveryFixing
	}
	if sameHead(l.goHead[p.Ref], p.Head) && l.goHead[p.Ref] != "" {
		return state.DeliveryMerging
	}
	requested := w != nil && w.ReviewRequested && (w.ParkedHead == "" || sameHead(w.ParkedHead, p.Head))
	rs := l.rounds[p.Ref]
	if len(rs) > 0 {
		rd := rs[len(rs)-1]
		if v := p.verdict(rd, l.pushed(p)); v != nil && v.Verdict == ChangesRequested && (sameHead(v.Head, p.Head) || !requested) {
			return state.DeliveryFixing
		}
		reviewerDone := slices.ContainsFunc(in.Events, func(e Event) bool {
			return e.Verb == VerbDone && e.Agent == ReviewerName(p.Ref, len(rs)) && e.At.After(rd.at)
		})
		if v := p.verdict(rd, l.pushed(p)); v != nil && v.Verdict == Approved && sameHead(v.Head, p.Head) && !p.Green {
			return state.DeliveryCI
		}
		if (!reviewerDone || sameHead(rd.head, p.Head)) && p.verdict(rd, l.pushed(p)) == nil {
			return state.DeliveryReviewing
		}
	}
	if !requested && len(rs) == 0 {
		return state.DeliveryImplementing
	}
	if !p.Green {
		return state.DeliveryCI
	}
	return state.DeliveryReviewQueued
}
