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
	LoopFillKey             = "next capacity "
	LoopFillStream          = "stream:workers"
	RefillZeroFormat        = "SUPERVISOR REFILL started 0: %s\n"
	deliveryReservationsKey = "delivery reservations "
	// OpenPullRefusal is how agents start refuses an item whose pull
	// request is open: its delivery is in flight.
	OpenPullRefusal = "already has an open pull request"
	// StaleNextAfter is the age past which a done event's next action is
	// left to the refill instead of starting a board item itself.
	StaleNextAfter = time.Hour
	// StaleNextWhat is what a stale next action is recorded as.
	StaleNextWhat = "superseded by the capacity refill: the done event is stale"
)

// StaleNext reports whether act is a next action for a done event older than
// staleNextAfter or than the start of this running loop. The refill fills
// every free slot without a done event, so such an action has nothing left
// to start; a backlog of them only kept the refill from running.
func (l *Loop) StaleNext(act superloop.Action) bool {
	if act.Kind != superloop.KindNext || act.Event == nil || act.Event.At.IsZero() {
		return false
	}
	at := act.Event.At
	// A single pass (--once, --dry-run) is a loop of its own: its start
	// says nothing about the events before it.
	running := !l.Once && !l.DryRun
	return running && at.Before(l.StartedAt) || l.Host.TimeNow().Sub(at) > StaleNextAfter
}

// SettleStaleNext records the stale next actions as taken, in one write and
// without a board read or a start, so no later plan holds them, and says how
// many once.
func (l *Loop) SettleStaleNext(stale []superloop.Action) error {
	if l.PauseForReexec() {
		return nil
	}
	var evs []state.Event
	var settled []superloop.Action
	for _, act := range stale {
		if l.IsCompleted(act.Key) {
			continue
		}
		evs = append(evs, Cmd.About(Cmd.Event(LoopParty, "supervisor."+superloop.KindNext, "%s", superloop.Detail(act, StaleNextWhat)), act.Ref))
		settled = append(settled, act)
	}
	if len(evs) == 0 {
		return nil
	}
	if err := l.Host.Store().Record(evs...); err != nil {
		return fmt.Errorf("recording stale next actions: %w", err)
	}
	for _, act := range settled {
		l.rememberAction(act)
	}
	_, err := fmt.Fprintf(l.Host.Out(), "SUPERVISOR NEXT SKIPPED %d stale done events; the refill fills capacity\n", len(settled))
	return err
}

// nextFailed records err as act's failure: a next action that cannot settle
// waits for its retry, and the refill still runs in the same pass.
func (l *Loop) nextFailed(act superloop.Action, failures map[string]LoopFailure, err error) error {
	if failures == nil {
		_, werr := fmt.Fprintf(l.Host.Out(), "SUPERVISOR NEXT FAILED %s: %v\n", act.Key, err)
		return werr
	}
	return l.Failed(act, failures, err)
}

// Command refusals and repository observation failures are item-specific;
// global admission gates remain pass-wide waits.
type RefillStartError struct {
	cause  error
	output string
}

func (e *RefillStartError) Error() string { return fmt.Sprintf("%v; %s", e.cause, e.output) }

func (e *RefillStartError) Unwrap() error { return e.cause }

type fillRetry struct {
	Until    time.Time
	failures int
}

func (l *Loop) FillSkipped() []string {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	var refs []string
	for ref, retry := range l.FillRetries {
		if l.Host.TimeNow().Before(retry.Until) {
			refs = append(refs, ref)
		}
	}
	slices.Sort(refs)
	return refs
}

func (l *Loop) DeferFill(ref string) {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	if l.FillRetries == nil {
		l.FillRetries = map[string]fillRetry{}
	}
	retry := l.FillRetries[ref]
	every := l.Host.Cfg().Supervisor.FillRetryEvery.Duration
	if every <= 0 {
		every = time.Minute
	}
	retry.Until = l.Host.TimeNow().Add(every << min(retry.failures, fillBackoff))
	retry.failures++
	l.FillRetries[ref] = retry
}

// FillPause holds the refill's board reads after an unproductive one: the
// capacity limits it saw and the agents then live (on the roster, not done).
type FillPause struct {
	Capacity string
	live     []string
	Until    time.Time
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
		{s.old, "older than " + Cmd.ShortDur(s.reserveFor)},
	} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.why))
		}
	}
	return strings.Join(parts, ", ")
}

// DeliveryReservations identifies existing deliveries before new work competes with the
// independent review and transition streams. Read durable observations so a
// pending start from an earlier pass reserves room too. A delivery reserves
// only a start the loop will make: in a repository the desk works on and no
// hold stops, for a review not yet requested at its head, and only for
// supervisor.reserveFor after its step began. Older ones are stalled
// deliveries, not slots held from new work.
func (l *Loop) DeliveryReservations(st *state.State) ([]string, deliverySkips) {
	now := l.Host.TimeNow()
	skips := deliverySkips{reserveFor: l.Host.Cfg().Supervisor.ReserveFor.Duration}
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
		case Cmd.ClassifyDelivery(st, d, now, l.Host.DeliveryPolicy()).Class != deliveryFlowing:
			skips.held++
		case slices.ContainsFunc(repos, func(ref string) bool { return deliveryRepoHeld(st, ref, now) }):
			skips.held++
		case d.Step == state.DeliveryReviewQueued && deliveryReviewAsked(st, d):
			skips.requested++
		case now.Sub(d.Since) > skips.reserveFor:
			skips.old++
		default:
			refs = append(refs, Cmd.DeliveryURL(d.Ref, slices.Contains(d.Pulls, d.Ref)))
		}
	}
	return refs, skips
}

// deliveryRepoHeld says whether an active hold of ref's repository is in st.
func deliveryRepoHeld(st *state.State, ref string, now time.Time) bool {
	r, ok := Cmd.ParseRef(ref)
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
func (l *Loop) DeliveryReservedRefs(st *state.State, reserved ...[]string) ([]string, deliverySkips) {
	refs, skips := l.DeliveryReservations(st)
	if len(reserved) > 0 {
		for _, ref := range reserved[0] {
			if !slices.Contains(refs, ref) {
				refs = append(refs, ref)
			}
		}
	}
	return refs, skips
}

// DeliveryRoom is room less the slots the deliveries reserve, which are at
// most half of room: a pile of deliveries never stops every new start.
func (l *Loop) DeliveryRoom(room int, st *state.State, reserved ...[]string) int {
	refs, skips := l.DeliveryReservedRefs(st, reserved...)

	slices.Sort(refs)
	skipped := skips.String()
	key := deliveryReservationsKey + strings.Join(refs, ",") + "; " + skipped
	l.Mu.Lock()
	for k := range l.Said {
		if strings.HasPrefix(k, deliveryReservationsKey) && k != key {
			delete(l.Said, k)
		}
	}
	l.Mu.Unlock()
	if len(refs) > 0 || skipped != "" {
		line := fmt.Sprintf("SUPERVISOR NEXT RESERVES %d slots for deliveries, at most half the room", len(refs))
		if len(refs) > 0 {
			line += ": " + strings.Join(refs, ", ")
		}
		if skipped != "" {
			line += "; not reserved: " + skipped
		}
		l.SayOnce(key, "%s", line)
	}
	return max(0, room-min(len(refs), room/2))
}

// Holds reports whether the pause still defers a board read: its interval
// runs and nothing it saw changed (changed).
func (p FillPause) Holds(now time.Time, capacity string, live []string) bool {
	return now.Before(p.Until) && !p.Changed(capacity, live)
}

// Changed reports whether capacity is not as it was or an agent the pause
// saw live has since been done or removed. An agent joining makes no room
// and changes nothing.
func (p FillPause) Changed(capacity string, live []string) bool {
	if capacity != p.Capacity {
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

// PauseFill defers the refill's next board read by supervisor.fillRetryEvery,
// doubled for each unproductive read before it since the last start or
// change (fillBackoff), so an empty board is walked less and less often.
func (l *Loop) PauseFill(capacity string, live []string) {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	every := l.Host.Cfg().Supervisor.FillRetryEvery.Duration << min(l.FillEmpty, fillBackoff)
	l.FillEmpty++
	until := l.Host.TimeNow().Add(every)
	for _, retry := range l.FillRetries {
		if retry.Until.After(l.Host.TimeNow()) && retry.Until.Before(until) {
			until = retry.Until
		}
	}
	l.FillPause = FillPause{Capacity: capacity, live: live, Until: until}
}

// PreviewFill runs in the command, which holds what it needs (loopPreviewFill).
func (l *Loop) PreviewFill(ctx context.Context) error {
	return Cmd.LoopPreviewFill(l, ctx)
}

// Fill runs in the command, which holds what it needs (loopFill).
func (l *Loop) Fill(ctx context.Context, st *state.State, claims map[string]LoopClaim, taken []superloop.Taken, reserved ...[]string) (result error) {
	return Cmd.LoopFill(l, ctx, st, claims, taken, reserved...)
}
