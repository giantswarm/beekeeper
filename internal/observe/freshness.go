package observe

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/forge"
)

// Transition is what a check did to a source's freshness.
type Transition int

// Transitions.
const (
	Unchanged Transition = iota
	Degraded
	Fresh
)

// Source is the freshness of one observation source (contract §1.3). Verdict, when it was last
// judged, is kept in memory: an idle check writes no object.
type Source struct {
	Name              string
	State             string
	Reason            string
	Since             time.Time
	Polling           bool
	LastGapAt         time.Time
	DeliveredSinceGap bool
	Cursor            string
	Verdict           time.Time
}

// NewSource returns a fresh source, or the one an Installation's status recorded.
func NewSource(name string, st *v1alpha1.SourceStatus, now time.Time) *Source {
	s := &Source{Name: name, State: v1alpha1.SourceFresh, Since: now}
	if st == nil {
		return s
	}
	if st.State != "" {
		s.State, s.Reason, s.Polling = st.State, st.Reason, st.Polling
	}
	if st.Since != nil {
		s.Since = st.Since.Time
	}
	if st.LastGapAt != nil {
		s.LastGapAt = st.LastGapAt.Time
	}
	s.DeliveredSinceGap, s.Cursor = st.DeliveredSinceGap, st.Cursor
	return s
}

// Degraded reports whether the source is degraded.
func (s *Source) Degraded() bool { return s.State == v1alpha1.SourceDegraded }

// Status is the source as the Installation's status shows it.
func (s *Source) Status() v1alpha1.SourceStatus {
	st := v1alpha1.SourceStatus{State: s.State, Reason: s.Reason, Polling: s.Polling,
		DeliveredSinceGap: s.DeliveredSinceGap, Cursor: s.Cursor}
	if !s.Since.IsZero() {
		t := metav1.NewTime(s.Since)
		st.Since = &t
	}
	if !s.LastGapAt.IsZero() {
		t := metav1.NewTime(s.LastGapAt)
		st.LastGapAt = &t
	}
	return st
}

// Gap records a scheduled check that failed, was deferred, or found a gap: the source degrades at
// once with reason, and polls from now on. A source that is degraded already stays so, under the
// latest reason.
func (s *Source) Gap(now time.Time, reason string) Transition {
	s.Verdict, s.LastGapAt, s.DeliveredSinceGap = now, now, false
	if s.Degraded() {
		s.Reason = reason
		return Unchanged
	}
	s.State, s.Reason, s.Since, s.Polling = v1alpha1.SourceDegraded, reason, now, true
	return Degraded
}

// Clean records a scheduled check that found no gap: a degraded source is fresh again after a full
// check interval without a new gap and, when needDelivery, at least one successful delivery since.
func (s *Source) Clean(now time.Time, interval time.Duration, needDelivery bool) Transition {
	s.Verdict = now
	if !s.Degraded() || now.Sub(s.LastGapAt) < interval || (needDelivery && !s.DeliveredSinceGap) {
		return Unchanged
	}
	return s.recover(now)
}

// Resolve makes the source fresh at once (the cause of its degradation is gone, e.g. a valid filter).
func (s *Source) Resolve(now time.Time) Transition {
	s.Verdict = now
	if !s.Degraded() {
		return Unchanged
	}
	return s.recover(now)
}

func (s *Source) recover(now time.Time) Transition {
	s.State, s.Reason, s.Since, s.Polling = v1alpha1.SourceFresh, "", now, false
	return Fresh
}

// LogCheck judges the webhooks source from the deliveries GitHub logged since the last check (oldest
// first). A failed delivery is a gap; its payload names what to re-observe. A delivery that succeeded
// after the last failure counts toward recovery.
func (s *Source) LogCheck(now time.Time, interval time.Duration, deliveries []forge.Delivery) ([]forge.Delivery, Transition) {
	var failed []forge.Delivery
	deliveredAfter := false
	for _, d := range deliveries {
		if d.Failed() {
			failed = append(failed, d)
			deliveredAfter = false
		} else {
			deliveredAfter = true
		}
	}
	if len(failed) > 0 {
		tr := s.Gap(now, v1alpha1.SourceReasonDeliveriesFailed)
		s.DeliveredSinceGap = deliveredAfter
		return failed, tr
	}
	if deliveredAfter {
		s.DeliveredSinceGap = true
	}
	return nil, s.Clean(now, interval, true)
}

// Probe judges the board's freshness for one team from its most recently updated items: an update
// newer than the last probe's and than anything the board webhooks reported for that item is a gap.
type Probe struct {
	primed bool
	cursor time.Time
	known  map[string]time.Time
}

// Reported records a board item update the engine read because a webhook announced it.
func (p *Probe) Reported(node string, at time.Time) {
	if at.IsZero() {
		return
	}
	if p.known == nil {
		p.known = map[string]time.Time{}
	}
	if at.After(p.known[node]) {
		p.known[node] = at
	}
}

// Judge returns the board items whose updates no webhook reported. Only issues count: the engine
// tracks no draft issues or PRs on the board, so it never reads (or reports) theirs. The first probe
// only sets the mark: what changed before the engine started was read by the startup resync. Reported
// updates the mark has passed can't matter any more and are forgotten.
func (p *Probe) Judge(items []forge.ProbeItem) []forge.ProbeItem {
	var gaps []forge.ProbeItem
	newest := p.cursor
	for _, it := range items {
		if !it.IsIssue && it.IssueRef == "" {
			continue
		}
		if p.primed && it.UpdatedAt.After(p.cursor) && it.UpdatedAt.After(p.known[it.NodeID]) {
			gaps = append(gaps, it)
		}
		if it.UpdatedAt.After(newest) {
			newest = it.UpdatedAt
		}
	}
	p.primed, p.cursor = true, newest
	for node, at := range p.known {
		if !at.After(p.cursor) {
			delete(p.known, node)
		}
	}
	return gaps
}

// Since is the oldest update the next probe needs to see: the newest it has judged (zero before the
// first probe, when the last day is asked for).
func (p *Probe) Since() time.Time { return p.cursor }
