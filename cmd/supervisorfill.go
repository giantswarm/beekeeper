package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const loopFillKey = "next capacity "

// fillPause holds the refill's board reads after an unproductive one: the
// capacity limits it saw and the agents then live (on the roster, not done).
type fillPause struct {
	capacity string
	live     []string
	until    time.Time
}

// fillCapacityKey is the configured ceiling. Room and the busy count move
// with every park, resume and wait; none of them can turn an empty board
// answer into a pick, and no room already stops a read.
func fillCapacityKey(cap *capacityView) string {
	return strconv.Itoa(cap.Ceiling)
}

// fillRoom is how many worker slots this pass can fill toward capacity.ceiling.
// Local room bounds the pass only when no start can be placed on an execution
// host; with supervisor.hosts, a hold across all providers (no choice picked
// and no local machine headroom block) also yields 0 so the pass does not
// query the board while every provider is held by pace, usage or its ceiling.
func (l *supervisorLoop) fillRoom(cap *capacityView) int {
	if cap == nil || cap.Stopped != "" || cap.Ceiling <= len(cap.Busy) {
		return 0
	}
	room := cap.Ceiling - len(cap.Busy)
	if !l.placesRemotely() {
		return min(room, cap.Room)
	}
	if cap.Choice != nil && cap.Choice.Harness == "" && (cap.Headroom == nil || len(cap.Headroom.blocks(cap.Builds)) == 0) {
		return 0
	}
	return room
}

// deliveryReservations identifies existing deliveries before new work competes with the
// independent review and transition streams. Read durable observations so a
// pending start from an earlier pass reserves room too.
func deliveryReservations(st *state.State) []string {
	refs := []string{}
	for _, d := range st.Deliveries {
		live := slices.ContainsFunc(st.Agents, func(ag state.Agent) bool {
			return !ag.Done && ag.Task != "" && strings.EqualFold(ag.Name, d.Agent)
		})
		if d.Step == state.DeliveryReviewQueued || d.Step == state.DeliveryFixing && !live {
			refs = append(refs, deliveryURL(d.Ref, slices.Contains(d.Pulls, d.Ref)))
		}
	}
	return refs
}

func (l *supervisorLoop) deliveryRoom(room int, st *state.State, reserved ...[]string) int {
	refs := deliveryReservations(st)
	if len(reserved) > 0 {
		refs = reserved[0]
	}

	slices.Sort(refs)
	key := "delivery reservations " + strings.Join(refs, ",")
	l.mu.Lock()
	for k := range l.said {
		if strings.HasPrefix(k, "delivery reservations ") && k != key {
			delete(l.said, k)
		}
	}
	l.mu.Unlock()
	if len(refs) > 0 {
		l.sayOnce(key, "SUPERVISOR NEXT RESERVES %d slots for deliveries: %s", len(refs), strings.Join(refs, ", "))
	}
	return max(0, room-len(refs))
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
	l.fillPause = fillPause{capacity: capacity, live: live, until: l.a.timeNow().Add(every)}
}

func (l *supervisorLoop) fillCapacity(ctx context.Context) *capacityView {
	out, code, err := beekeeperRun(ctx, l.as("capacity", "--json")...)
	if err != nil || code != 0 {
		return nil
	}
	var cap capacityView
	if err := json.Unmarshal([]byte(out), &cap); err != nil {
		l.sayOnce("fill capacity", "SUPERVISOR NEXT FAILED capacity --json: %v", err)
		return nil
	}
	return &cap
}

func (l *supervisorLoop) previewFill(ctx context.Context) error {
	st, err := l.a.store.Read()
	if err != nil {
		return err
	}
	if l.fillPreview != nil {
		st = l.fillPreview
	}
	room := l.deliveryFillRoom(l.fillRoom(l.fillCapacity(ctx)), st)
	if room <= 0 {
		return nil
	}
	next, err := l.nextItem(ctx)
	if errors.Is(err, errLoopLater) {
		_, err = fmt.Fprintln(l.a.out, "SUPERVISOR WOULD NEXT wait: admission or GitHub budget holds new work")
		return err
	}
	if err != nil {
		return err
	}
	count, first := 0, "the board offers nothing"
	if next != nil {
		count, first = room, next.URL
	}
	_, err = fmt.Fprintf(l.a.out, "SUPERVISOR WOULD NEXT fill %d to capacity.ceiling (first: %s)\n", count, first)
	return err
}

// fill fills every free slot up to capacity.ceiling without waiting for a
// done event. Each start checks live capacity again; the first pass room
// also bounds this pass if a capacity read has not yet caught up with a
// start. The local room bounds the pass only when no start can be placed on
// an execution host: with supervisor.hosts each start's placement judges
// the local machine and every host itself, so a full coordinator still
// fills onto hosts. Claims are per board item, so an interrupted start is
// reconciled before another start.
func (l *supervisorLoop) fill(ctx context.Context, st *state.State, claims map[string]loopClaim, taken []superloop.Taken, reserved ...[]string) error {
	if l.a.cfg.Supervisor.WorkerBrief == "" {
		return nil
	}
	if l.dryRun {
		return l.previewFill(ctx)
	}
	attempts := map[string]int{}
	for _, t := range taken {
		if ref, ok := strings.CutPrefix(t.Key, loopFillKey); ok {
			ref, _, _ = strings.Cut(ref, " ")
			attempts[ref]++
		}
	}
	startedHere := map[string]bool{}
	for key, c := range claims {
		if !strings.HasPrefix(key, loopFillKey) {
			continue
		}
		act := superloop.Action{Kind: superloop.KindNext, Key: key}
		what, done, err := l.reconcile(ctx, st, act, c)
		if err != nil {
			return err
		}
		if done {
			if err := l.record(act, what); err != nil {
				return err
			}
			ref, _, _ := strings.Cut(strings.TrimPrefix(key, loopFillKey), " ")
			attempts[ref]++
			startedHere[ref] = true
		}
	}
	// A non-positive limit means the first fresh capacity read sets it.
	limit := 0
	for started := 0; ; started++ {
		if l.pauseForReexec() {
			return nil
		}
		cap := l.fillCapacity(ctx)
		if cap == nil {
			return nil // a gate refusal is never a reason to start
		}
		live, err := l.a.store.Read()
		if err != nil {
			return err
		}
		capacity, alive := fillCapacityKey(cap), fillLive(live)
		l.mu.Lock()
		if !l.fillPause.holds(l.a.timeNow(), capacity, alive) {
			if !l.fillPause.until.IsZero() && l.fillPause.changed(capacity, alive) {
				l.fillEmpty = 0 // a change may make room: the backoff starts over
			}
			l.fillPause = fillPause{}
		}
		paused := !l.fillPause.until.IsZero()
		l.mu.Unlock()
		pause := func() { l.pauseFill(capacity, alive) }
		room := l.deliveryFillRoom(l.fillRoom(cap), live, reserved...)
		if room <= 0 {
			return nil
		}
		if started == 0 {
			limit = room
		}
		if started >= limit {
			return nil
		}
		if paused {
			return nil
		}
		next, err := l.nextItem(ctx)
		if errors.Is(err, errLoopLater) {
			return nil // the budget recovers on its own: no pause, no failure
		}
		if err != nil {
			pause()
			l.sayOnce("fill board", "SUPERVISOR NEXT FAILED board next: %v", err)
			return nil
		}
		if next == nil {
			pause()
			return nil
		}
		if startedHere[next.Ref] {
			pause()
			return nil // a stale board answer must not dispatch the same item twice
		}
		r, _ := parseRef(next.Ref)
		name := fmt.Sprintf("%s %d", repoName(r.Repo), r.N)
		if slices.ContainsFunc(live.Agents, func(ag state.Agent) bool { return ag.Name == name }) {
			pause()
			return nil // the board may lag the worker's registration
		}
		act := superloop.Action{Kind: superloop.KindNext, Key: fmt.Sprintf("%s%s attempt %d", loopFillKey, next.Ref, attempts[next.Ref]+1)}
		what, err := l.startItem(ctx, act, *next)
		if err != nil {
			if !errors.Is(err, errLoopLater) {
				pause()
			}
			if !errors.Is(err, errLoopLater) {
				l.sayOnce(act.Key, "SUPERVISOR NEXT FAILED %s: %v", act.Key, err)
			}
			return nil
		}
		if err := l.record(act, what); err != nil {
			return fmt.Errorf("recording capacity refill: %w", err)
		}
		attempts[next.Ref]++
		startedHere[next.Ref] = true
		l.mu.Lock()
		l.fillEmpty = 0
		l.mu.Unlock()
	}
}
