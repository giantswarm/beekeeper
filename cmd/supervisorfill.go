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

// fillCapacityKey is the configured ceiling. Room and the busy count move
// with every park, resume and wait; none of them can turn an empty board
// answer into a pick, and no room already stops a read.
func fillCapacityKey(cap *capacityView) string {
	return strconv.Itoa(cap.Ceiling)
}

// fillRepositoryHoldsKey invalidates an empty-board pause as soon as a
// repository hold is lifted, expires or changes its exception.
func fillRepositoryHoldsKey(st *state.State, now time.Time) string {
	var holds []string
	for _, h := range st.Holds {
		if _, ok := parseRef(h.Target + "#1"); ok && h.Active(now) {
			holds = append(holds, fmt.Sprintf("%q:%q", strings.ToLower(h.Target), strings.ToLower(h.Except)))
		}
	}
	slices.Sort(holds)
	return strings.Join(holds, ",")
}

// loopFillRoom is how many worker slots this pass can fill toward capacity.ceiling.
// Local room bounds the pass only when no start can be placed on an execution
// host; with supervisor.hosts, a hold across all providers (no choice picked
// and no local machine headroom block) also yields 0 so the pass does not
// query the board while every provider is held by pace, usage or its ceiling.
func loopFillRoom(l *Loop, cap *capacityView) int {
	if cap == nil || cap.Stopped != "" || cap.Ceiling <= len(cap.Busy) {
		return 0
	}
	room := cap.Ceiling - len(cap.Busy)
	if !l.PlacesRemotely() {
		return min(room, cap.Room)
	}
	if cap.Choice != nil && cap.Choice.Harness == "" && (cap.Headroom == nil || len(cap.Headroom.blocks(cap.Builds)) == 0) {
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

func loopFillCapacity(l *Loop, ctx context.Context) *capacityView {
	cap, _ := loopReadFillCapacity(l, ctx)
	return cap
}

func loopReadFillCapacity(l *Loop, ctx context.Context) (*capacityView, error) {
	out, code, err := BeekeeperRun(ctx, l.As("capacity", "--json")...)
	if err != nil || code != 0 {
		return nil, fmt.Errorf("capacity --json: exit %d: %v: %s", code, err, oneLine(out))
	}
	var cap capacityView
	if err := json.Unmarshal([]byte(out), &cap); err != nil {
		return nil, fmt.Errorf("capacity --json: %w", err)
	}
	return &cap, nil
}

func loopPreviewFill(l *Loop, ctx context.Context) error {
	st, err := l.Host.store.Read()
	if err != nil {
		return err
	}
	if l.FillPreview != nil {
		st = l.FillPreview
	}
	capacity, err := loopReadFillCapacity(l, ctx)
	if err != nil {
		return err
	}
	if l.Recording != nil {
		l.Recording.Room = loopFillRoom(l, capacity)
	}
	room := l.DeliveryFillRoom(loopFillRoom(l, capacity), st)
	if room <= 0 {
		return nil
	}
	next, err := l.NextItem(ctx)
	if errors.Is(err, ErrLoopLater) {
		_, err = fmt.Fprintln(l.Host.out, "SUPERVISOR WOULD NEXT wait: admission or GitHub budget holds new work")
		return err
	}
	if err != nil {
		return err
	}
	count, first := 0, "the board offers nothing"
	if next != nil {
		count, first = room, next.URL
	}
	_, err = fmt.Fprintf(l.Host.out, "SUPERVISOR WOULD NEXT fill %d to capacity.ceiling (first: %s)\n", count, first)
	return err
}

// loopFill fills every free slot up to capacity.ceiling without waiting for a
// done event. Each start checks live capacity again; the first pass room
// also bounds this pass if a capacity read has not yet caught up with a
// start. The local room bounds the pass only when no start can be placed on
// an execution host: with supervisor.hosts each start's placement judges
// the local machine and every host itself, so a full coordinator still
// fills onto hosts. Claims are per board item, so an interrupted start is
// reconciled before another start.
func loopFill(l *Loop, ctx context.Context, st *state.State, claims map[string]LoopClaim, taken []superloop.Taken, reserved ...[]string) (result error) {
	startedCount := 0
	why := "no worker brief configured"
	defer func() {
		if !l.DryRun && l.Host.cfg.Supervisor.WorkerBrief != "" && startedCount == 0 {
			if result != nil {
				why = oneLine(result.Error())
			}
			_, _ = fmt.Fprintf(l.Host.out, RefillZeroFormat, why)
		}
	}()
	if l.Host.cfg.Supervisor.WorkerBrief == "" {
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
		cap, capacityErr := loopReadFillCapacity(l, ctx)
		if capacityErr != nil {
			why = oneLine(capacityErr.Error())
			l.StallWait("", why)
			return nil // a gate refusal is never a reason to start
		}
		l.SetFillPhase("read capacity reservations")
		live, err := l.Host.store.Read()
		if err != nil {
			return err
		}
		capacityRoom := loopFillRoom(l, cap)
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
				v := readDeliveryWIP(live, l.Host.cfg.Delivery.WIPLimit, l.Host.cfg.Supervisor.LoopInterval.Duration, l.Host.timeNow(), l.Host.deliveryPolicy())
				why = fmt.Sprintf("delivery admission: %s (%d flowing, limit %d; last complete observation %s)", v.Reason, v.Count, v.Limit, v.CompleteAt.UTC().Format(time.RFC3339))
			}
			l.StallClear() // no admitted slot: nothing waits on the refill
			return nil
		}
		ordinary := readDeliveryWIP(live, l.Host.cfg.Delivery.WIPLimit, l.Host.cfg.Supervisor.LoopInterval.Duration, l.Host.timeNow(), l.Host.deliveryPolicy()).Reason == ""
		capacity, alive := fillCapacityKey(cap)+"/"+fillRepositoryHoldsKey(live, l.Host.timeNow()), fillLive(live)
		l.Mu.Lock()
		if !l.FillPause.Holds(l.Host.timeNow(), capacity, alive) {
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
			why = oneLine(err.Error())
			l.StallWait("", why)
			return nil // the budget recovers on its own: no pause, no failure
		}
		if err != nil {
			why = "board next failed: " + oneLine(err.Error())
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
		r, _ := parseRef(next.Ref)
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
				why = next.URL + ": its start failed: " + oneLine(err.Error())
				l.StallWait(next.URL, why)
				l.SayOnce(act.Key, "SUPERVISOR NEXT FAILED %s: %v", next.URL, err)
				// Bound a pass even when the board keeps offering refused items.
				if len(attemptedHere) >= 32 {
					return nil
				}
				started-- // a refused command consumes no free worker slot
				continue
			}
			why = next.URL + ": its start failed: " + oneLine(err.Error())
			if errors.Is(err, ErrLoopLater) {
				why = next.URL + ": " + oneLine(err.Error())
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
