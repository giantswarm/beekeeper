package cmd

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The capacity tests' busy workers and their roomy verdict.
const (
	busyOne   = "W 1"
	busyTwo   = "W 2"
	roomSeven = "room for 7 starts"
)

// tasked is a roster entry with a task.
func tasked(name string) state.Agent {
	return state.Agent{Party: state.Party{Session: "s-" + name, Name: name}, Task: "fix o/r#1"}
}

// capacityRoster is a roster with every kind of entry: the supervisor and
// the guide registered as agents, two busy workers, a kept one, one a timer
// wakes, one waiting on its person, an idle one, a done one and two role
// runs relieved since.
func capacityRoster() (*state.State, []*claude.Session) {
	sup, guide, kept, woken, waits := tasked("Supervisor run 68"), tasked("Guide run 9"), tasked("Judge"), tasked("BK 3"), tasked("BK 4")
	kept.Keep = &state.Keep{Reason: "a person returns to it"}
	done := tasked("BK 5")
	done.Done = true
	relieved, guideRun := tasked("Supervisor run 60"), tasked("Guide run 8")
	relieved.Task, guideRun.Task = "the supervisor's watch as Supervisor run 60", "the guide's role as Guide run 8"
	st := &state.State{
		Supervisor: &state.Supervisor{Party: sup.Party},
		Guide:      &state.Role{Holder: &state.Supervisor{Party: guide.Party}},
		Agents:     []state.Agent{sup, guide, tasked(busyOne), tasked(busyTwo), kept, woken, waits, {Party: state.Party{Session: "s-spare", Name: "spare"}}, done, relieved, guideRun},
		Timers:     []state.Timer{{ID: 7, Due: keepNow.Add(time.Hour), What: "the batch job ended", Wake: "BK 3"}},
	}
	sessions := []*claude.Session{{ID: "s-" + busyOne}, {ID: "s-BK 4", Waiting: &claude.Waiting{Action: "approve the plan"}}}
	return st, sessions
}

// Busy is a roster agent with a task that is neither kept, woken by a timer
// nor waiting on its person; the supervisor, the guide and their relieved
// runs are not counted.
func TestCountAgents(t *testing.T) {
	st, sessions := capacityRoster()
	c := countAgents(st, sessions, keepNow)
	want := agentCount{
		Busy:   []string{busyOne, busyTwo},
		Parked: []string{"Judge (kept: a person returns to it)", "BK 3 (timer #7 wakes it)", "BK 4 (waits on its person)"},
		Idle:   []string{"spare", "BK 5"},
	}
	if !slices.Equal(c.Busy, want.Busy) || !slices.Equal(c.Parked, want.Parked) || !slices.Equal(c.Idle, want.Idle) {
		t.Errorf("count %+v, want %+v", c, want)
	}
	// The successor of an open relay is the role's, not a worker.
	st.Relay = &state.Relay{To: st.Agents[2].Party, Expires: keepNow.Add(time.Minute)}
	if c := countAgents(st, sessions, keepNow); !slices.Equal(c.Busy, []string{busyTwo}) {
		t.Errorf("busy with a relay open to %s: %v", busyOne, c.Busy)
	}
}

// roomy is a headroom with memory, swap, slots and labs to spare.
func roomy() *headroom {
	return &headroom{AvailableMiB: 45 << 10, AvailMinMiB: 20 << 10, SwapGrowthMaxMiB: 256,
		Swap: &swapReading{Rated: true, PerHourMiB: 10}, SlotsFree: 1, Slots: 2, Labs: 1, MaxLabs: 2}
}

// The verdict is room up to the ceiling within the guards, else every guard
// that blocks.
func TestCapacityVerdict(t *testing.T) {
	k := config.Capacity{Floor: 5, Ceiling: 10}
	busy := func(n int) agentCount {
		c := agentCount{}
		for i := range n {
			c.Busy = append(c.Busy, fmt.Sprint("BK ", i))
		}
		return c
	}
	for _, c := range []struct {
		name string
		busy int
		edit func(*headroom)
		want string
	}{
		{"room", 3, func(*headroom) {}, roomSeven},
		{"one", 9, func(*headroom) {}, "room for 1 start"},
		{"ceiling", 10, func(*headroom) {}, "no start: 10 busy at the ceiling of 10"},
		{"memory", 3, func(h *headroom) { h.AvailableMiB = 15 << 10 }, "no start: MemAvailable 15.0 GiB under 20 GiB"},
		{"swap", 3, func(h *headroom) { h.Swap.PerHourMiB = 900 }, "no start: swap growing +900 MiB/h, over 256"},
		{"swap unrated", 3, func(h *headroom) { h.Swap = &swapReading{PerHourMiB: 900} }, roomSeven},
		{"swap full, flat", 3, func(h *headroom) { h.Swap.UsedMiB, h.Swap.PerHourMiB = 16<<10, 0 }, roomSeven},
		{"slots", 3, func(h *headroom) { h.SlotsFree = 0 }, "no start: no free build slot"},
		{"labs", 3, func(h *headroom) { h.Labs = 3 }, "no start: 3 kind labs over the cap of 2"},
		{"labs at the cap", 3, func(h *headroom) { h.Labs = 2 }, roomSeven},
		{"two", 3, func(h *headroom) { h.AvailableMiB, h.SlotsFree = 10<<10, 0 }, "no start: MemAvailable 10.0 GiB under 20 GiB; no free build slot"},
	} {
		h := roomy()
		c.edit(h)
		if got := newCapacity(busy(c.busy), k, h).verdict(); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	h := roomy()
	if got, want := h.line(), "headroom: MemAvailable 45.0 GiB (floor 20 GiB), swap +10 MiB/h (max 256), build slots 1 of 2 free, kind labs 1 of 2"; got != want {
		t.Errorf("line %q, want %q", got, want)
	}
	h.Swap = nil
	if got := h.line(); !strings.Contains(got, "swap growth not measured (no watch reading)") {
		t.Errorf("line without a reading: %q", got)
	}
}

// The watch says CAPACITY LOW once while busy stays under the floor with
// room for a start, ENDED when the count recovers, and again when it drops;
// CAPACITY FULL at the ceiling. Without room it says nothing.
func TestWatchCapacity(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	room := roomy()
	reads := 0
	w.readHeadroom = func(context.Context, *swapReading) *headroom { reads++; return room }
	roster := func(n int) *state.State {
		st := &state.State{Supervisor: &state.Supervisor{Party: state.Party{Session: "s-sup", Name: "Supervisor run 68"}}}
		for i := range n {
			st.Agents = append(st.Agents, tasked(fmt.Sprint("BK ", i)))
		}
		return st
	}
	count := func(s string) int { return strings.Count(out.String(), s) }

	w.capacity(context.Background(), roster(3), nil)
	w.capacity(context.Background(), roster(3), nil)
	if count("CAPACITY LOW 3 of 5: room for 7 starts; headroom: MemAvailable 45.0 GiB") != 1 {
		t.Fatalf("under the floor twice:\n%s", out)
	}
	w.capacity(context.Background(), roster(5), nil)
	if count("ENDED CAPACITY LOW 3 of 5") != 1 || reads != 2 {
		t.Fatalf("recovered (reads %d):\n%s", reads, out)
	}
	w.capacity(context.Background(), roster(4), nil)
	if count("CAPACITY LOW 4 of 5") != 1 {
		t.Fatalf("dropped again:\n%s", out)
	}
	w.capacity(context.Background(), roster(10), nil)
	if count("CAPACITY FULL 10 of 10: headroom:") != 1 {
		t.Fatalf("at the ceiling:\n%s", out)
	}
	out.Reset()
	room.AvailableMiB = 5 << 10
	w.capacity(context.Background(), roster(2), nil)
	if strings.Contains(out.String(), "CAPACITY LOW") {
		t.Fatalf("under the floor without room:\n%s", out)
	}
}

// The watch's swap reading reaches capacity through the state while fresh.
func TestWatchSwapReading(t *testing.T) {
	w, _, _ := notifyingWatch(t, t.TempDir(), false)
	w.keepSwap(&swapReading{At: relayNow, UsedMiB: 900, PerHourMiB: 40, Rated: true})
	if r := w.watchSwap(); r == nil || r.PerHourMiB != 40 {
		t.Fatalf("fresh reading: %+v", r)
	}
	w.now = relayNow.Add(swapFresh + time.Second)
	if r := w.watchSwap(); r != nil {
		t.Errorf("stale reading counted: %+v", r)
	}
}

// The successor's prompt carries the count against the target, and the
// command that reads the headroom.
func TestPromptCapacity(t *testing.T) {
	st, sessions := capacityRoster()
	var buf bytes.Buffer
	a := &app{out: &buf, now: keepNow, cfg: &config.Config{Capacity: config.Capacity{Floor: 5, Ceiling: 10}}}
	a.promptCapacity(func(f string, args ...any) { buf.WriteString(fmt.Sprintf(f, args...) + "\n") }, st, sessions)
	if p := buf.String(); !strings.Contains(p, "### Capacity") ||
		!strings.Contains(p, "2 busy of floor 5, ceiling 10: "+busyOne+", "+busyTwo+"; 3 parked, 2 idle; `beekeeper capacity` says whether a start fits.") {
		t.Errorf("prompt:\n%s", p)
	}
}
