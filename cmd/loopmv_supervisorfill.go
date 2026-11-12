package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	loopFillKey             = "next capacity "
	loopFillStream          = "stream:workers"
	refillZeroFormat        = "SUPERVISOR REFILL started 0: %s\n"
	deliveryReservationsKey = "delivery reservations "
	// openPullRefusal is how agents start refuses an item whose pull
	// request is open: its delivery is in flight.
	openPullRefusal = "already has an open pull request"
	// staleNextAfter is the age past which a done event's next action is
	// left to the refill instead of starting a board item itself.
	staleNextAfter = time.Hour
	// staleNextWhat is what a stale next action is recorded as.
	staleNextWhat = "superseded by the capacity refill: the done event is stale"
)

// staleNext reports whether act is a next action for a done event older than
// staleNextAfter or than the start of this running loop. The refill fills
// every free slot without a done event, so such an action has nothing left
// to start; a backlog of them only kept the refill from running.
func (l *supervisorLoop) staleNext(act superloop.Action) bool {
	if act.Kind != superloop.KindNext || act.Event == nil || act.Event.At.IsZero() {
		return false
	}
	at := act.Event.At
	// A single pass (--once, --dry-run) is a loop of its own: its start
	// says nothing about the events before it.
	running := !l.once && !l.dryRun
	return running && at.Before(l.startedAt) || l.a.timeNow().Sub(at) > staleNextAfter
}

// settleStaleNext records the stale next actions as taken, in one write and
// without a board read or a start, so no later plan holds them, and says how
// many once.
func (l *supervisorLoop) settleStaleNext(stale []superloop.Action) error {
	if l.pauseForReexec() {
		return nil
	}
	var evs []state.Event
	var settled []superloop.Action
	for _, act := range stale {
		if l.isCompleted(act.Key) {
			continue
		}
		evs = append(evs, about(event(loopParty, "supervisor."+superloop.KindNext, "%s", superloop.Detail(act, staleNextWhat)), act.Ref))
		settled = append(settled, act)
	}
	if len(evs) == 0 {
		return nil
	}
	if err := l.a.store.Record(evs...); err != nil {
		return fmt.Errorf("recording stale next actions: %w", err)
	}
	for _, act := range settled {
		l.rememberAction(act)
	}
	_, err := fmt.Fprintf(l.a.out, "SUPERVISOR NEXT SKIPPED %d stale done events; the refill fills capacity\n", len(settled))
	return err
}

// nextFailed records err as act's failure: a next action that cannot settle
// waits for its retry, and the refill still runs in the same pass.
func (l *supervisorLoop) nextFailed(act superloop.Action, failures map[string]loopFailure, err error) error {
	if failures == nil {
		_, werr := fmt.Fprintf(l.a.out, "SUPERVISOR NEXT FAILED %s: %v\n", act.Key, err)
		return werr
	}
	return l.failed(act, failures, err)
}

// Command refusals and repository observation failures are item-specific;
// global admission gates remain pass-wide waits.
type refillStartError struct {
	cause  error
	output string
}

func (e *refillStartError) Error() string { return fmt.Sprintf("%v; %s", e.cause, e.output) }

func (e *refillStartError) Unwrap() error { return e.cause }

type fillRetry struct {
	until    time.Time
	failures int
}

func (l *supervisorLoop) fillSkipped() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var refs []string
	for ref, retry := range l.fillRetries {
		if l.a.timeNow().Before(retry.until) {
			refs = append(refs, ref)
		}
	}
	slices.Sort(refs)
	return refs
}

func (l *supervisorLoop) deferFill(ref string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fillRetries == nil {
		l.fillRetries = map[string]fillRetry{}
	}
	retry := l.fillRetries[ref]
	every := l.a.cfg.Supervisor.FillRetryEvery.Duration
	if every <= 0 {
		every = time.Minute
	}
	retry.until = l.a.timeNow().Add(every << min(retry.failures, fillBackoff))
	retry.failures++
	l.fillRetries[ref] = retry
}

// fillPause holds the refill's board reads after an unproductive one: the
// capacity limits it saw and the agents then live (on the roster, not done).
type fillPause struct {
	capacity string
	live     []string
	until    time.Time
}

// deliverySkips counts the deliveries in a reserving step that reserve no
// slot, by why: the loop makes no start for them in the next pass.
type deliverySkips struct {
	outside, held, requested, old int
	reserveFor                    time.Duration
}

// String is the skips as the reservation line names them, "" for none.
func (s deliverySkips) String() string {
	var parts []string
	for _, p := range []struct {
		n   int
		why string
	}{
		{s.outside, "outside the desk"},
		{s.held, "under a hold"},
		{s.requested, "review already requested"},
		{s.old, "older than " + shortDur(s.reserveFor)},
	} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.why))
		}
	}
	return strings.Join(parts, ", ")
}

// deliveryReservations identifies existing deliveries before new work competes with the
// independent review and transition streams. Read durable observations so a
// pending start from an earlier pass reserves room too. A delivery reserves
// only a start the loop will make: in a repository the desk works on and no
// hold stops, for a review not yet requested at its head, and only for
// supervisor.reserveFor after its step began. Older ones are stalled
// deliveries, not slots held from new work.
func (l *supervisorLoop) deliveryReservations(st *state.State) ([]string, deliverySkips) {
	now := l.a.timeNow()
	skips := deliverySkips{reserveFor: l.a.cfg.Supervisor.ReserveFor.Duration}
	if skips.reserveFor <= 0 {
		skips.reserveFor = config.DefaultReserveFor
	}
	refs := []string{}
	for _, d := range st.Deliveries {
		live := slices.ContainsFunc(st.Agents, func(ag state.Agent) bool {
			return !ag.Done && ag.Task != "" && strings.EqualFold(ag.Name, d.Agent)
		})
		if d.Step != state.DeliveryReviewQueued && (d.Step != state.DeliveryFixing || live) {
			continue
		}
		repos := append([]string{d.Ref}, d.Pulls...)
		switch {
		case slices.ContainsFunc(repos, func(ref string) bool { return l.unreadRepo(ref) != "" }):
			skips.outside++
		case classifyDelivery(st, d, now, l.a.deliveryPolicy()).Class != deliveryFlowing:
			skips.held++
		case slices.ContainsFunc(repos, func(ref string) bool { return deliveryRepoHeld(st, ref, now) }):
			skips.held++
		case d.Step == state.DeliveryReviewQueued && deliveryReviewAsked(st, d):
			skips.requested++
		case now.Sub(d.Since) > skips.reserveFor:
			skips.old++
		default:
			refs = append(refs, deliveryURL(d.Ref, slices.Contains(d.Pulls, d.Ref)))
		}
	}
	return refs, skips
}

// deliveryRepoHeld says whether an active hold of ref's repository is in st.
func deliveryRepoHeld(st *state.State, ref string, now time.Time) bool {
	r, ok := parseRef(ref)
	return ok && slices.ContainsFunc(st.Holds, func(h state.Hold) bool { return strings.EqualFold(h.Target, r.Repo) && h.Active(now) })
}

// deliveryReviewAsked says whether every pull of d already has a review
// request or a desk review at its current head (at any head when its head is
// unknown): the loop starts no reviewer for it.
func deliveryReviewAsked(st *state.State, d state.Delivery) bool {
	if len(d.Pulls) == 0 {
		return false
	}
	for _, pull := range d.Pulls {
		head := d.Heads[pull]
		at := func(ref, h string) bool { return strings.EqualFold(ref, pull) && (head == "" || h == head) }
		if !slices.ContainsFunc(st.ReviewRequests, func(r state.ReviewRequest) bool { return at(r.Ref, r.Head) }) &&
			!slices.ContainsFunc(st.DeskReviews, func(r state.DeskReview) bool { return at(r.Ref, r.Head) }) {
			return false
		}
	}
	return true
}

// Keep earlier reservations even after their starts register, and include
// observations committed by an older stream since this pass was planned.
func (l *supervisorLoop) deliveryReservedRefs(st *state.State, reserved ...[]string) ([]string, deliverySkips) {
	refs, skips := l.deliveryReservations(st)
	if len(reserved) > 0 {
		for _, ref := range reserved[0] {
			if !slices.Contains(refs, ref) {
				refs = append(refs, ref)
			}
		}
	}
	return refs, skips
}

// deliveryRoom is room less the slots the deliveries reserve, which are at
// most half of room: a pile of deliveries never stops every new start.
func (l *supervisorLoop) deliveryRoom(room int, st *state.State, reserved ...[]string) int {
	refs, skips := l.deliveryReservedRefs(st, reserved...)

	slices.Sort(refs)
	skipped := skips.String()
	key := deliveryReservationsKey + strings.Join(refs, ",") + "; " + skipped
	l.mu.Lock()
	for k := range l.said {
		if strings.HasPrefix(k, deliveryReservationsKey) && k != key {
			delete(l.said, k)
		}
	}
	l.mu.Unlock()
	if len(refs) > 0 || skipped != "" {
		line := fmt.Sprintf("SUPERVISOR NEXT RESERVES %d slots for deliveries, at most half the room", len(refs))
		if len(refs) > 0 {
			line += ": " + strings.Join(refs, ", ")
		}
		if skipped != "" {
			line += "; not reserved: " + skipped
		}
		l.sayOnce(key, "%s", line)
	}
	return max(0, room-min(len(refs), room/2))
}

// holds reports whether the pause still defers a board read: its interval
// runs and nothing it saw changed (changed).
func (p fillPause) holds(now time.Time, capacity string, live []string) bool {
	return now.Before(p.until) && !p.changed(capacity, live)
}

// changed reports whether capacity is not as it was or an agent the pause
// saw live has since been done or removed. An agent joining makes no room
// and changes nothing.
func (p fillPause) changed(capacity string, live []string) bool {
	if capacity != p.capacity {
		return true
	}
	for _, name := range p.live {
		if !slices.Contains(live, name) {
			return true
		}
	}
	return false
}

// fillBackoff is how many times the refill's pause doubles over
// consecutive unproductive board reads: at most 4 × supervisor.fillRetryEvery.
const fillBackoff = 2

// pauseFill defers the refill's next board read by supervisor.fillRetryEvery,
// doubled for each unproductive read before it since the last start or
// change (fillBackoff), so an empty board is walked less and less often.
func (l *supervisorLoop) pauseFill(capacity string, live []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	every := l.a.cfg.Supervisor.FillRetryEvery.Duration << min(l.fillEmpty, fillBackoff)
	l.fillEmpty++
	until := l.a.timeNow().Add(every)
	for _, retry := range l.fillRetries {
		if retry.until.After(l.a.timeNow()) && retry.until.Before(until) {
			until = retry.until
		}
	}
	l.fillPause = fillPause{capacity: capacity, live: live, until: until}
}

// previewFill runs in the command, which holds what it needs (loopPreviewFill).
func (l *supervisorLoop) previewFill(ctx context.Context) error {
	return loopPreviewFill(l, ctx)
}

// fill runs in the command, which holds what it needs (loopFill).
func (l *supervisorLoop) fill(ctx context.Context, st *state.State, claims map[string]loopClaim, taken []superloop.Taken, reserved ...[]string) (result error) {
	return loopFill(l, ctx, st, claims, taken, reserved...)
}
