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
// runs, capacity is as it was and no agent it saw live has since been done
// or removed. An agent joining makes no room and keeps it.
func (p fillPause) holds(now time.Time, capacity string, live []string) bool {
	if !now.Before(p.until) || capacity != p.capacity {
		return false
	}
	for _, name := range p.live {
		if !slices.Contains(live, name) {
			return false
		}
	}
	return true
}

func (l *supervisorLoop) pauseFill(capacity string, live []string) {
	l.fillPause = fillPause{capacity: capacity, live: live, until: l.a.now.Add(l.a.cfg.Supervisor.FillRetryEvery.Duration)}
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
	room := l.fillRoom(l.fillCapacity(ctx))
	if room <= 0 {
		return nil
	}
	next, err := l.nextItem(ctx)
	if errors.Is(err, errLoopLater) {
		_, err = fmt.Fprintln(l.a.out, "SUPERVISOR WOULD NEXT wait: the GitHub budget is under github.floor")
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
func (l *supervisorLoop) fill(ctx context.Context, st *state.State, claims map[string]loopClaim, taken []superloop.Taken) error {
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
		cap := l.fillCapacity(ctx)
		if cap == nil {
			return nil // a gate refusal is never a reason to start
		}
		live, err := l.a.store.Read()
		if err != nil {
			return err
		}
		capacity, alive := fillCapacityKey(cap), fillLive(live)
		if !l.fillPause.holds(l.a.now, capacity, alive) {
			l.fillPause = fillPause{}
		}
		pause := func() { l.pauseFill(capacity, alive) }
		room := l.fillRoom(cap)
		if room <= 0 {
			return nil
		}
		if started == 0 {
			limit = room
		}
		if started >= limit {
			return nil
		}
		if !l.fillPause.until.IsZero() {
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
			pause()
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
	}
}
