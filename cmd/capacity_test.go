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

// roomy is a headroom with memory, swap, load, slots and labs to spare.
func roomy() *headroom {
	return &headroom{AvailableMiB: 45 << 10, AvailMinMiB: 20 << 10, SwapGrowthMaxMiB: 256,
		Swap: &swapReading{Rated: true, AvailFalling: true, PerHourMiB: 10}, Load1: 3, LoadMax: 48, SlotsFree: 1, Slots: 2, Labs: 1, MaxLabs: 2}
}

// slotNote is the verdict's note for a start that does not build while no
// build slot is free.
const slotNote = " (no free build slot: a start that builds (--builds) waits for it)"

// tight is a roomy headroom whose memory leaves no room for a start: the
// watch tests' machine, which says no CAPACITY line under the floor.
func tight() *headroom {
	h := roomy()
	h.AvailableMiB = 5 << 10
	return h
}

// The verdict is room up to the ceiling within the guards, else every guard
// that blocks. The build slot guards only a start that builds: without the
// mark a busy slot is a note after the verdict, with it a block.
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
		name   string
		busy   int
		builds bool
		edit   func(*headroom)
		want   string
	}{
		{"room", 3, false, func(*headroom) {}, roomSeven},
		{"room, builds", 3, true, func(*headroom) {}, roomSeven},
		{"one", 9, false, func(*headroom) {}, "room for 1 start"},
		{"ceiling", 10, false, func(*headroom) {}, "no start: 10 busy at the ceiling of 10"},
		{"memory", 3, false, func(h *headroom) { h.AvailableMiB = 15 << 10 }, "no start: MemAvailable 15.0 GiB under 20 GiB"},
		{"swap", 3, false, func(h *headroom) { h.Swap.PerHourMiB = 900 }, "no start: disk swap growing +900 MiB/h while MemAvailable falls, over 256"},
		{"swap growing, RAM recovers", 3, false, func(h *headroom) { h.Swap.PerHourMiB, h.Swap.AvailFalling = 900, false }, roomSeven},
		{"swap unrated", 3, false, func(h *headroom) { h.Swap = &swapReading{PerHourMiB: 900} }, roomSeven},
		{"swap full, flat", 3, false, func(h *headroom) { h.Swap.UsedMiB, h.Swap.PerHourMiB = 16<<10, 0 }, roomSeven},
		{"load", 3, false, func(h *headroom) { h.Load1 = 60 }, "no start: 1m load 60 over 48"},
		{"load unknown", 3, false, func(h *headroom) { h.Load1, h.LoadErr = 60, "no /proc/loadavg" }, roomSeven},
		{"load unbounded", 3, false, func(h *headroom) { h.Load1, h.LoadMax = 60, 0 }, roomSeven},
		{"slot busy, no build", 3, false, func(h *headroom) { h.SlotsFree = 0 }, roomSeven + slotNote},
		{"slot busy, builds", 3, true, func(h *headroom) { h.SlotsFree = 0 }, "no start: no free build slot"},
		{"no slots configured", 3, true, func(h *headroom) { h.SlotsFree, h.Slots = 0, 0 }, roomSeven},
		{"labs", 3, false, func(h *headroom) { h.Labs = 3 }, "no start: 3 kind labs over the cap of 2"},
		{"labs at the cap", 3, false, func(h *headroom) { h.Labs = 2 }, roomSeven},
		{"memory and slot, no build", 3, false, func(h *headroom) { h.AvailableMiB, h.SlotsFree = 10<<10, 0 }, "no start: MemAvailable 10.0 GiB under 20 GiB" + slotNote},
		{"memory and slot, builds", 3, true, func(h *headroom) { h.AvailableMiB, h.SlotsFree = 10<<10, 0 }, "no start: MemAvailable 10.0 GiB under 20 GiB; no free build slot"},
	} {
		h := roomy()
		c.edit(h)
		v := newCapacity(busy(c.busy), k, h, c.builds)
		if got := v.verdict(); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		if v.Builds != c.builds {
			t.Errorf("%s: builds %v, want %v", c.name, v.Builds, c.builds)
		}
	}
	h := roomy()
	if got, want := h.line(), "headroom: MemAvailable 45.0 GiB (floor 20 GiB), disk swap 0 MiB +10 MiB/h (max 256), zswap 0 MiB, 1m load 3 (max 48), build slots 1 of 2 free, kind labs 1 of 2"; got != want {
		t.Errorf("line %q, want %q", got, want)
	}
	h.Swap = nil
	h.LoadErr = "no /proc/loadavg"
	if got := h.line(); !strings.Contains(got, "swap growth not measured (no watch reading)") || !strings.Contains(got, ", load unknown,") {
		t.Errorf("line without readings: %q", got)
	}
}

// The start gate lets a start with room through, refuses one without
// (exit 3, the guards named) and lets --force through with the verdict it
// overrides: a busy build slot refuses only a start that builds.
func TestGateStart(t *testing.T) {
	k := config.Capacity{Floor: 5, Ceiling: 10}
	three := countAgents(supervised(3), nil, keepNow)
	slotBusy := roomy()
	slotBusy.SlotsFree = 0
	for _, c := range []struct {
		name     string
		h        *headroom
		builds   bool
		force    bool
		wantOver string
		wantErr  string
	}{
		{"room", roomy(), false, false, "", ""},
		{"slot busy, no build", slotBusy, false, false, "", ""},
		{"slot busy, builds", slotBusy, true, false, "", "no start: no free build slot (beekeeper capacity; --force starts anyway)"},
		{"slot busy, builds, forced", slotBusy, true, true, "no start: no free build slot", ""},
		{"tight", tight(), false, false, "", "no start: MemAvailable 5.0 GiB under 20 GiB (beekeeper capacity; --force starts anyway)"},
		{"tight, forced", tight(), false, true, "no start: MemAvailable 5.0 GiB under 20 GiB", ""},
	} {
		over, err := gateStart(newCapacity(three, k, c.h, c.builds), c.force)
		if over != c.wantOver {
			t.Errorf("%s: overridden %q, want %q", c.name, over, c.wantOver)
		}
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: %v, want the start through", c.name, err)
		case c.wantErr != "" && (err == nil || err.Error() != c.wantErr):
			t.Errorf("%s: %v, want %q", c.name, err, c.wantErr)
		case c.wantErr != "" && Code(err) != ExitRefused:
			t.Errorf("%s: exit %d, want %d", c.name, Code(err), ExitRefused)
		}
	}
}

// The watch's CAPACITY LOW weighs a start that does not build: a busy build
// slot leaves the room and is a note in the line.
func TestWatchCapacitySlotBusy(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	room := roomy()
	room.SlotsFree = 0
	w.readHeadroom = func(context.Context, *swapReading) *headroom { return room }
	w.capacity(context.Background(), supervised(3), nil)
	if !strings.Contains(out.String(), "CAPACITY LOW 3 of 5: "+roomSeven+slotNote+"; headroom:") {
		t.Fatalf("under the floor with the slot busy:\n%s", out)
	}
}

// supervised is a roster of n busy workers under a supervisor.
func supervised(n int) *state.State {
	st := &state.State{Supervisor: &state.Supervisor{Party: state.Party{Session: "s-sup", Name: "Supervisor run 68"}}}
	for i := range n {
		st.Agents = append(st.Agents, tasked(fmt.Sprint("BK ", i)))
	}
	return st
}

// The watch says CAPACITY LOW once while busy stays under the floor with
// room for a start, ENDED when the count recovers, and again when it drops;
// CAPACITY FULL at the ceiling. Without room it says nothing.
func TestWatchCapacity(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	room := roomy()
	reads := 0
	w.readHeadroom = func(context.Context, *swapReading) *headroom { reads++; return room }
	count := func(s string) int { return strings.Count(out.String(), s) }

	w.capacity(context.Background(), supervised(3), nil)
	w.capacity(context.Background(), supervised(3), nil)
	if count("CAPACITY LOW 3 of 5: room for 7 starts; headroom: MemAvailable 45.0 GiB") != 1 {
		t.Fatalf("under the floor twice:\n%s", out)
	}
	w.capacity(context.Background(), supervised(5), nil)
	if count("ENDED CAPACITY LOW 3 of 5") != 1 || reads != 2 {
		t.Fatalf("recovered (reads %d):\n%s", reads, out)
	}
	w.capacity(context.Background(), supervised(4), nil)
	if count("CAPACITY LOW 4 of 5") != 1 {
		t.Fatalf("dropped again:\n%s", out)
	}
	w.capacity(context.Background(), supervised(10), nil)
	if count("CAPACITY FULL 10 of 10: headroom:") != 1 {
		t.Fatalf("at the ceiling:\n%s", out)
	}
	out.Reset()
	room.AvailableMiB = 5 << 10
	w.capacity(context.Background(), supervised(2), nil)
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
