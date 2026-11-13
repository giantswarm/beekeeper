package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/capacity"
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
// StaleNextAfter or than the start of this running loop. The refill fills
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
// request or a desk review not released at its current head (at any head
// when its head is unknown): the loop starts no reviewer for it.
func deliveryReviewAsked(st *state.State, d state.Delivery) bool {
	if len(d.Pulls) == 0 {
		return false
	}
	for _, pull := range d.Pulls {
		head := d.Heads[pull]
		at := func(ref, h string) bool { return strings.EqualFold(ref, pull) && (head == "" || h == head) }
		if !slices.ContainsFunc(st.ReviewRequests, func(r state.ReviewRequest) bool { return at(r.Ref, r.Head) }) &&
			!slices.ContainsFunc(st.DeskReviews, func(r state.DeskReview) bool { return r.Released.IsZero() && at(r.Ref, r.Head) }) {
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

// fillCapacityKey is the configured ceiling. Room and the busy count move
// with every park, resume and wait; none of them can turn an empty board
// answer into a pick, and no room already stops a read.
func fillCapacityKey(cap *capacity.View) string {
	return strconv.Itoa(cap.Ceiling)
}

// fillRepositoryHoldsKey invalidates an empty-board pause as soon as a
// repository hold is lifted, expires or changes its exception.
func fillRepositoryHoldsKey(st *state.State, now time.Time) string {
	var holds []string
	for _, h := range st.Holds {
		if _, ok := Cmd.ParseRef(h.Target + "#1"); ok && h.Active(now) {
			holds = append(holds, fmt.Sprintf("%q:%q", strings.ToLower(h.Target), strings.ToLower(h.Except)))
		}
	}
	slices.Sort(holds)
	return strings.Join(holds, ",")
}

// FillRoom is how many worker slots this pass can fill toward capacity.ceiling.
// Local room bounds the pass only when no start can be placed on an execution
// host; with supervisor.hosts, a hold across all providers (no choice picked
// and no local machine headroom block) also yields 0 so the pass does not
// query the board while every provider is held by pace, usage or its ceiling.
func (l *Loop) FillRoom(cap *capacity.View) int {
	if cap == nil || cap.Stopped != "" || cap.Ceiling <= len(cap.Busy) {
		return 0
	}
	room := cap.Ceiling - len(cap.Busy)
	if !l.PlacesRemotely() {
		return min(room, cap.Room)
	}
	if cap.Choice != nil && cap.Choice.Harness == "" && (cap.Headroom == nil || len(cap.Headroom.Blocks(cap.Builds)) == 0) {
		return 0
	}
	return room
}

// fillLive is the names of the roster's agents not done. A park, a resume,
// a new session or task leaves it alone: none of them makes room.
func fillLive(st *state.State) []string {
	var live []string
	for _, ag := range st.Agents {
		if !ag.Done {
			live = append(live, ag.Name)
		}
	}
	return live
}

func (l *Loop) FillCapacity(ctx context.Context) *capacity.View {
	cap, _ := l.ReadFillCapacity(ctx)
	return cap
}

func (l *Loop) ReadFillCapacity(ctx context.Context) (*capacity.View, error) {
	out, code, err := BeekeeperRun(ctx, l.As("capacity", "--json")...)
	if err != nil || code != 0 {
		return nil, fmt.Errorf("capacity --json: exit %d: %v: %s", code, err, Cmd.OneLine(out))
	}
	// The output is stdout and then stderr: the document is its first value.
	// A diagnostic after it (a stale state snapshot's) is no failed read.
	var cap capacity.View
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&cap); err != nil {
		return nil, fmt.Errorf("capacity --json: %w", err)
	}
	return &cap, nil
}

func (l *Loop) PreviewFill(ctx context.Context) error {
	st, err := l.Host.Store().Read()
	if err != nil {
		return err
	}
	if l.FillPreview != nil {
		st = l.FillPreview
	}
	capacity, err := l.ReadFillCapacity(ctx)
	if err != nil {
		return err
	}
	if l.Recording != nil {
		l.Recording.Room = l.FillRoom(capacity)
	}
	room := l.DeliveryFillRoom(l.FillRoom(capacity), st)
	if room <= 0 {
		return nil
	}
	next, err := l.NextItem(ctx)
	if errors.Is(err, ErrLoopLater) {
		_, err = fmt.Fprintln(l.Host.Out(), "SUPERVISOR WOULD NEXT wait: admission or GitHub budget holds new work")
		return err
	}
	if err != nil {
		return err
	}
	count, first := 0, "the board offers nothing"
	if next != nil {
		count, first = room, next.URL
	}
	_, err = fmt.Fprintf(l.Host.Out(), "SUPERVISOR WOULD NEXT fill %d to capacity.ceiling (first: %s)\n", count, first)
	return err
}

// Fill fills every free slot up to capacity.ceiling without waiting for a
// done event. Each start checks live capacity again; the first pass room
// also bounds this pass if a capacity read has not yet caught up with a
// start. The local room bounds the pass only when no start can be placed on
// an execution host: with supervisor.hosts each start's placement judges
// the local machine and every host itself, so a full coordinator still
// fills onto hosts. Claims are per board item, so an interrupted start is
// reconciled before another start.
func (l *Loop) Fill(ctx context.Context, st *state.State, claims map[string]LoopClaim, taken []superloop.Taken, reserved ...[]string) (result error) {
	startedCount := 0
	why := "no worker brief configured"
	defer func() {
		if !l.DryRun && l.Host.Cfg().Supervisor.WorkerBrief != "" && startedCount == 0 {
			if result != nil {
				why = Cmd.OneLine(result.Error())
			}
			_, _ = fmt.Fprintf(l.Host.Out(), RefillZeroFormat, why)
		}
	}()
	if l.Host.Cfg().Supervisor.WorkerBrief == "" {
		return nil
	}
	if l.DryRun {
		return l.PreviewFill(ctx)
	}
	if l.PauseForReexec() || ctx.Err() != nil {
		why = "loop stopping or waiting to re-exec"
		return nil
	}
	attempts := map[string]int{}
	for _, t := range taken {
		if ref, ok := strings.CutPrefix(t.Key, LoopFillKey); ok {
			ref, _, _ = strings.Cut(ref, " ")
			attempts[ref]++
		}
	}
	l.FillRan()
	defer l.SetFillPhase("")
	why = "loop stopping or waiting to re-exec"
	startedHere := map[string]bool{}
	attemptedHere := map[string]bool{}
	for key, c := range claims {
		if l.PauseForReexec() || ctx.Err() != nil {
			return nil
		}
		if !strings.HasPrefix(key, LoopFillKey) {
			continue
		}
		act := superloop.Action{Kind: superloop.KindNext, Key: key}
		l.SetFillPhase("reconcile " + key)
		what, done, err := l.Reconcile(ctx, st, act, c)
		if err != nil {
			return err
		}
		if done {
			if err := l.Record(act, what); err != nil {
				return err
			}
			ref, _, _ := strings.Cut(strings.TrimPrefix(key, LoopFillKey), " ")
			attempts[ref]++
			startedHere[ref] = true
			startedCount++
		}
	}
	// A non-positive limit means the first fresh capacity read sets it.
	limit := 0
	for started := 0; ; started++ {
		if l.HoldingStarts() {
			why = "starts held"
			l.StallClear()
			return nil
		}
		if l.PauseForReexec() || ctx.Err() != nil {
			return nil
		}
		l.SetFillPhase("capacity --json")
		cap, capacityErr := l.ReadFillCapacity(ctx)
		if capacityErr != nil {
			why = Cmd.OneLine(capacityErr.Error())
			l.StallWait("", why)
			return nil // a gate refusal is never a reason to start
		}
		l.SetFillPhase("read capacity reservations")
		live, err := l.Host.Store().Read()
		if err != nil {
			return err
		}
		capacityRoom := l.FillRoom(cap)
		room := l.DeliveryFillRoom(capacityRoom, live, reserved...)
		if room <= 0 {
			why = "no room for a worker"
			if capacityRoom <= 0 {
				if cap.Stopped != "" {
					why = "held: " + cap.Stopped
				} else if len(cap.Blocks) > 0 {
					why += ": " + strings.Join(cap.Blocks, "; ")
				} else if cap.Choice != nil && cap.Choice.Reason != "" {
					why += ": " + cap.Choice.Reason
				}
			} else {
				v := Cmd.ReadDeliveryWIP(live, l.Host.Cfg().Delivery.WIPLimit, l.Host.Cfg().Supervisor.LoopInterval.Duration, l.Host.TimeNow(), l.Host.DeliveryPolicy())
				why = fmt.Sprintf("delivery admission: %s (%d flowing, limit %d; last complete observation %s)", v.Reason, v.Count, v.Limit, v.CompleteAt.UTC().Format(time.RFC3339))
			}
			l.StallClear() // no admitted slot: nothing waits on the refill
			return nil
		}
		ordinary := Cmd.ReadDeliveryWIP(live, l.Host.Cfg().Delivery.WIPLimit, l.Host.Cfg().Supervisor.LoopInterval.Duration, l.Host.TimeNow(), l.Host.DeliveryPolicy()).Reason == ""
		capacity, alive := fillCapacityKey(cap)+"/"+fillRepositoryHoldsKey(live, l.Host.TimeNow()), fillLive(live)
		l.Mu.Lock()
		if !l.FillPause.Holds(l.Host.TimeNow(), capacity, alive) {
			if !l.FillPause.Until.IsZero() && l.FillPause.Changed(capacity, alive) {
				l.FillEmpty = 0 // a change may make room: the backoff starts over
			}
			l.FillPause = FillPause{}
		}
		paused := !l.FillPause.Until.IsZero()
		pauseUntil := l.FillPause.Until
		l.Mu.Unlock()
		pause := func() {
			if ordinary {
				l.PauseFill(capacity, alive)
			}
		}
		if started == 0 {
			limit = room
		}
		if started >= limit {
			return nil
		}
		if paused {
			why = "board retry backoff until " + pauseUntil.UTC().Format(time.RFC3339)
			return nil
		}
		l.SetFillPhase("board next")
		next, err := l.NextItem(ctx)
		if errors.Is(err, ErrLoopLater) {
			why = Cmd.OneLine(err.Error())
			l.StallWait("", why)
			return nil // the budget recovers on its own: no pause, no failure
		}
		if err != nil {
			why = "board next failed: " + Cmd.OneLine(err.Error())
			l.StallWait("", why)
			pause()
			l.SayOnce("fill board", "SUPERVISOR NEXT FAILED board next: %v", err)
			return nil
		}
		if next == nil {
			why = "no free board item"
			if !ordinary {
				why += " in WIP-exempt intake; ordinary intake is held"
			}
			l.StallClear() // the board offers nothing: nothing waits
			pause()
			return nil
		}
		if startedHere[next.Ref] || attemptedHere[next.Ref] || slices.Contains(l.FillSkipped(), next.Ref) {
			why = "board returned an already attempted or deferred item: " + next.URL
			pause()
			return nil // a stale board answer must not dispatch the same item twice
		}
		r, _ := Cmd.ParseRef(next.Ref)
		name := fmt.Sprintf("%s %d", RepoName(r.Repo), r.N)
		if slices.ContainsFunc(live.Agents, func(ag state.Agent) bool { return ag.Name == name }) {
			why = fmt.Sprintf("board next offers %s, but the roster already has an agent named %q", next.URL, name)
			l.StallWait(next.URL, why)
			pause()
			return nil // the board may lag the worker's registration
		}
		act := superloop.Action{Kind: superloop.KindNext, Key: fmt.Sprintf("%s%s attempt %d", LoopFillKey, next.Ref, attempts[next.Ref]+1)}
		attemptedHere[next.Ref] = true
		l.SetFillPhase("start " + next.URL)
		what, err := l.StartItem(ctx, act, *next, reserved...)
		if err == nil {
			startedCount++ // a later recording failure must not report zero starts
		}
		if err != nil {
			var failed *RefillStartError
			if errors.As(err, &failed) {
				l.DeferFill(next.Ref)
				why = next.URL + ": its start failed: " + Cmd.OneLine(err.Error())
				l.StallWait(next.URL, why)
				l.SayOnce(act.Key, "SUPERVISOR NEXT FAILED %s: %v", next.URL, err)
				// Bound a pass even when the board keeps offering refused items.
				if len(attemptedHere) >= 32 {
					return nil
				}
				started-- // a refused command consumes no free worker slot
				continue
			}
			why = next.URL + ": its start failed: " + Cmd.OneLine(err.Error())
			if errors.Is(err, ErrLoopLater) {
				why = next.URL + ": " + Cmd.OneLine(err.Error())
			}
			l.StallWait(next.URL, why)
			if !errors.Is(err, ErrLoopLater) {
				pause()
			}
			if !errors.Is(err, ErrLoopLater) {
				l.SayOnce(act.Key, "SUPERVISOR NEXT FAILED %s: %v", act.Key, err)
			}
			return nil
		}
		if err := l.Record(act, what); err != nil {
			return fmt.Errorf("recording capacity refill: %w", err)
		}
		attempts[next.Ref]++
		startedHere[next.Ref] = true
		l.StallClear()
		l.Mu.Lock()
		l.FillEmpty = 0
		delete(l.FillRetries, next.Ref)
		l.Mu.Unlock()
	}
}
