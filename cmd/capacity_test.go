package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The capacity tests' busy workers and their roomy verdict.
const (
	busyOne            = "W 1"
	busyTwo            = "W 2"
	roomSeven          = "room for 7 starts"
	noStartCeilingZero = "no start: ceiling 0"
)

type roomyMachine struct{ platform.Machine }

func (roomyMachine) Mem() (machine.Mem, error) {
	return machine.Mem{AvailableMiB: 45 << 10, TotalMiB: 96 << 10}, nil
}
func (roomyMachine) MemoryPressure() (float64, error) { return 0, nil }

// Load keeps a start's room whatever the cores of the host running the test:
// 3 stays under every desk's limit, the per-core default included.
func (roomyMachine) Load() ([3]float64, error) { return [3]float64{3, 3, 3}, nil }
func (roomyMachine) Processes() (*proc.Table, error) {
	return &proc.Table{ByPID: map[int]*proc.Process{}}, nil
}

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

// An agent whose session only waits in a wait of its own (a polling loop's
// sleep, a CI watch) and has been quiet past waitQuiet is waiting, not
// busy, and leaves its slot to a start; one quiet for less, or running a
// build beside its wait, stays busy.
func TestCountAgentsWaiting(t *testing.T) {
	poll, ci, fresh, build := tasked("Poll"), tasked("CI"), tasked("Fresh"), tasked("Build")
	st := &state.State{Agents: []state.Agent{poll, ci, fresh, build}}
	quiet := keepNow.Add(-2 * waitQuiet)
	sleep := claude.Command{Args: "sleep 30", Wait: true}
	sessions := []*claude.Session{
		{ID: "s-Poll", LastActive: quiet, Commands: []claude.Command{sleep}},
		{ID: "s-CI", LastActive: quiet, Commands: []claude.Command{{Args: "gh run watch 7", Wait: true}}},
		{ID: "s-Fresh", LastActive: keepNow.Add(-time.Minute), Commands: []claude.Command{sleep}},
		{ID: "s-Build", LastActive: quiet, Commands: []claude.Command{sleep, {Args: "go test ./..."}}},
	}
	c := countAgents(st, sessions, keepNow)
	if want := []string{"Fresh", "Build"}; !slices.Equal(c.Busy, want) {
		t.Errorf("busy %v, want %v", c.Busy, want)
	}
	if want := []string{"Poll (waits: sleep 30)", "CI (waits: gh run watch 7)"}; !slices.Equal(c.Waiting, want) {
		t.Errorf("waiting %v, want %v", c.Waiting, want)
	}
	k := config.Capacity{Floor: 1, Ceiling: 3}
	if got := c.line(k); got != "2 busy of floor 1, ceiling 3: Fresh, Build; 0 parked, 2 waiting, 0 idle" {
		t.Errorf("line %q", got)
	}
	// Four agents at a ceiling of three, two of them waiting: room for one.
	if v := newCapacity(c, k, roomy(), false); v.Room != 1 {
		t.Errorf("room %d (%s), want 1", v.Room, v.verdict())
	}
}

// roomy is a headroom with memory, swap, pressure, load, slots and labs to
// spare.
func roomy() *headroom {
	return &headroom{AvailableMiB: 45 << 10, AvailMinMiB: 20 << 10, SwapGrowthMaxMiB: 256,
		Swap: &swapReading{Rated: true, PerHourMiB: 10}, Load1: 3, LoadMax: 48, SlotsFree: 1, Slots: 2, Labs: 1, MaxLabs: 2}
}

// The swap guard's figures at a disk swap growth over the max.
const (
	swapFigures   = "memory PSI 0% (max 0%), MemAvailable 45.0 GiB (floor 20 GiB)"
	swapWarning   = roomSeven + " (disk swap growing +900 MiB/h over 256, without pressure: " + swapFigures + ")"
	swapUnderPSI  = "no start: disk swap growing +900 MiB/h over 256, under pressure: memory PSI 0.4% over 0%, MemAvailable 45.0 GiB (floor 20 GiB)"
	swapUnderRoom = "no start: MemAvailable 15.0 GiB under 20 GiB; disk swap growing +900 MiB/h over 256, under pressure: memory PSI 0% (max 0%), MemAvailable 15.0 GiB under the floor of 20 GiB"
)

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
		// Disk swap growing over the max is a warning with PSI within the
		// max and MemAvailable over the floor, a block under either.
		{"swap, PSI 0 and room", 3, false, func(h *headroom) { h.Swap.PerHourMiB = 900 }, swapWarning},
		{"swap under PSI", 3, false, func(h *headroom) { h.Swap.PerHourMiB, h.PSI = 900, 0.4 }, swapUnderPSI},
		{"swap under the floor", 3, false, func(h *headroom) { h.Swap.PerHourMiB, h.AvailableMiB = 900, 15<<10 }, swapUnderRoom},
		{"swap, PSI within the max", 3, false, func(h *headroom) { h.Swap.PerHourMiB, h.PSI, h.PSIMax = 900, 3, 5 },
			roomSeven + " (disk swap growing +900 MiB/h over 256, without pressure: memory PSI 3% (max 5%), MemAvailable 45.0 GiB (floor 20 GiB))"},
		{"swap, PSI unknown", 3, false, func(h *headroom) { h.Swap.PerHourMiB, h.PSIErr = 900, "no pressure file" },
			"no start: disk swap growing +900 MiB/h over 256, pressure unknown: memory PSI unknown (no pressure file), MemAvailable 45.0 GiB (floor 20 GiB)"},
		{"swap warning and slot note", 3, false, func(h *headroom) { h.Swap.PerHourMiB, h.SlotsFree = 900, 0 },
			roomSeven + " (disk swap growing +900 MiB/h over 256, without pressure: " + swapFigures + "; no free build slot: a start that builds (--builds) waits for it)"},
		{"PSI alone", 3, false, func(h *headroom) { h.PSI = 12 }, roomSeven},
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
	if got, want := h.line(), "headroom: MemAvailable 45.0 GiB (floor 20 GiB), memory PSI 0% (max 0%), disk swap 0 MiB +10 MiB/h (max 256), zswap 0 MiB writing back 0 MiB/h, 1m load 3 (max 48), build slots 1 of 2 free, kind labs 1 of 2"; got != want {
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
		!strings.Contains(p, "2 busy of floor 5, ceiling 10: "+busyOne+", "+busyTwo+"; 3 parked, 0 waiting, 2 idle; `beekeeper capacity` says whether a start fits.") {
		t.Errorf("prompt:\n%s", p)
	}
}

// A start that builds, refused by the busy build slot alone, waits its turn
// in the slot queue and is judged again once it came; any other guard
// refuses it without the wait, and a turn that does not come refuses it
// with the waiters still ahead.
func TestQueueStart(t *testing.T) {
	k := config.Capacity{Floor: 5, Ceiling: 10}
	three := countAgents(supervised(3), nil, keepNow)
	slotBusy := roomy()
	slotBusy.SlotsFree = 0
	tightBusy := tight()
	tightBusy.SlotsFree = 0
	for _, c := range []struct {
		name      string
		h         *headroom
		turn      bool
		again     *headroom
		wantAwait bool
		wantErr   string
	}{
		{"slot busy, turn comes", slotBusy, true, slotBusy, true, ""},
		{"slot busy, turn comes, memory gone", slotBusy, true, tight(), true, "no start: MemAvailable 5.0 GiB under 20 GiB (beekeeper capacity; --force starts anyway)"},
		{"slot busy, no turn", slotBusy, false, nil, true, "no start: no free build slot, 2 ahead in the build slot queue after 8m (beekeeper capacity; --force starts anyway)"},
		{"slot busy and tight", tightBusy, true, nil, false, "no start: MemAvailable 5.0 GiB under 20 GiB; no free build slot (beekeeper capacity; --force starts anyway)"},
		{"slot free", roomy(), true, nil, false, ""},
	} {
		awaited := false
		await := func() (bool, int, error) { awaited = true; return c.turn, 2, nil }
		again := func() (*capacityView, error) { return newCapacity(three, k, c.again, false), nil }
		_, err := queueStart(newCapacity(three, k, c.h, true), true, false, "8m", await, again)
		if awaited != c.wantAwait {
			t.Errorf("%s: waited in the queue %v, want %v", c.name, awaited, c.wantAwait)
		}
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: %v, want the start through", c.name, err)
		case c.wantErr != "" && (err == nil || err.Error() != c.wantErr):
			t.Errorf("%s: %v, want %q", c.name, err, c.wantErr)
		}
	}
}

// start --builds --dry-run says how the start would find the build slot:
// free, or the wait with the slots' holders, and claims nothing.
func TestDryRunSlotNamesTheWait(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MEMCAP_STATE", state)
	t.Setenv("MEMCAP_SLOTS", "2")
	t.Setenv("MEMCAP_WAIT", "8m")
	var out bytes.Buffer
	a := &app{cfg: &config.Config{}, out: &out}
	if err := a.dryRunSlot(); err != nil || out.String() != "build slot: free\n" {
		t.Fatalf("dry run on free slots = %q, %v", out.String(), err)
	}
	holdSlots(t, state)
	out.Reset()
	if err := a.dryRunSlot(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"build slot: busy, the start waits its turn up to 8m for a build slot (all 2 held)", "go test holder-1", "go test holder-2"} {
		if !strings.Contains(got, want) {
			t.Errorf("dry run on held slots = %q, want %q", got, want)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(state, "slots", "queue")); len(entries) != 0 {
		t.Errorf("the dry run left %d tickets in the queue", len(entries))
	}
}

// A configured ceiling of 0 (and floor of 0) says "no start: ceiling 0",
// refuses agents start without --force, and keeps the watch silent; unset
// keeps the 5/10 defaults.
func TestCapacityCeilingZeroAndUnset(t *testing.T) {
	zero := config.Capacity{Floor: 0, Ceiling: 0}
	for _, n := range []int{0, 2} {
		v := newCapacity(countAgents(supervised(n), nil, keepNow), zero, roomy(), false)
		if got := v.verdict(); got != noStartCeilingZero || v.Room != 0 {
			t.Errorf("%d busy at ceiling 0: verdict %q (room %d), want %q", n, got, v.Room, noStartCeilingZero)
		}
	}
	vTight := newCapacity(agentCount{}, zero, tight(), false)
	if got, want := vTight.verdict(), noStartCeilingZero+"; MemAvailable 5.0 GiB under 20 GiB"; got != want {
		t.Errorf("ceiling 0 and tight memory: %q, want %q", got, want)
	}
	vZero := newCapacity(agentCount{}, zero, roomy(), false)
	if _, err := gateStart(vZero, false); Code(err) != ExitRefused || err.Error() != noStartCeilingZero+" (beekeeper capacity; --force starts anyway)" {
		t.Errorf("gateStart without --force: exit %d, %v", Code(err), err)
	}
	if over, err := gateStart(vZero, true); err != nil || over != noStartCeilingZero {
		t.Errorf("gateStart with --force: over %q, %v", over, err)
	}

	// End-to-end: config set capacity.floor 0 capacity.ceiling 0 -> capacity
	// says "no start: ceiling 0" and agents start refuses without --force;
	// an unset config keeps floor 5, ceiling 10.
	orig := plat
	plat = platform.Stub()
	plat.Machine = roomyMachine{Machine: plat.Machine}
	t.Cleanup(func() { plat = orig })

	dir := t.TempDir()
	cfgPath := emptyTestConfig(t, filepath.Join(dir, "config.yaml"))
	t.Setenv("HOME", dir)
	t.Setenv("BEEKEEPER_CONFIG", cfgPath)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("MEMCAP_STATE", filepath.Join(dir, "memcap"))
	t.Setenv("PATH", dir)

	openApp := func(jsonOut bool) (*app, *bytes.Buffer) {
		c, err := config.Load(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		store, err := state.OpenVersion(c.StateDir, "dev")
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		return &app{cfgPath: cfgPath, cfg: c, store: store, now: keepNow, out: &out, as: agentOne, json: jsonOut}, &out
	}

	const (
		floorKey        = "capacity.floor"
		ceilingKey      = "capacity.ceiling"
		verbAgentsStart = "agents.start"
		lowTwoOfFive    = "CAPACITY LOW 2 of 5"
		dirFlag         = "--dir"
		forceFlag       = "--force"
	)
	setCap := func(path, floor, ceiling string, extra ...config.Setting) {
		t.Helper()
		settings := append([]config.Setting{{Key: floorKey, Value: floor}, {Key: ceilingKey, Value: ceiling}}, extra...)
		if _, err := config.Set(path, settings); err != nil {
			t.Fatal(err)
		}
	}

	aUnset, outUnset := openApp(false)
	if err := aUnset.capacityCmd().Execute(); err != nil || !strings.Contains(outUnset.String(), "0 busy of floor 5, ceiling 10") || !strings.Contains(outUnset.String(), "room for 10 starts\n") {
		t.Fatalf("unset capacity = %q, %v", outUnset.String(), err)
	}
	cfgCmd := aUnset.configCmd()
	cfgCmd.SetArgs([]string{"set", floorKey, "0", ceilingKey, "0"})
	if err := cfgCmd.Execute(); err != nil {
		t.Fatalf("config set 0 = %v", err)
	}
	aZero, outZero := openApp(false)
	if err := aZero.capacityCmd().Execute(); err != nil || !strings.Contains(outZero.String(), "0 busy of floor 0, ceiling 0") || !strings.Contains(outZero.String(), "\n"+noStartCeilingZero+"\n") {
		t.Fatalf("ceiling 0 capacity = %q, %v", outZero.String(), err)
	}
	aJSON, outJSON := openApp(true)
	if err := aJSON.capacityCmd().Execute(); err != nil || !strings.Contains(outJSON.String(), `"floor": 0`) || !strings.Contains(outJSON.String(), `"ceiling": 0`) || !strings.Contains(outJSON.String(), `"verdict": "`+noStartCeilingZero+`"`) {
		t.Fatalf("ceiling 0 capacity --json = %q, %v", outJSON.String(), err)
	}
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("Work the task.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	startCmd := aZero.agentStartCmd()
	startCmd.SilenceErrors = true
	startCmd.SilenceUsage = true
	startCmd.SetArgs([]string{busyOne, brief, dirFlag, dir})
	if err := startCmd.Execute(); Code(err) != ExitRefused || !strings.Contains(err.Error(), noStartCeilingZero) {
		t.Errorf("agents start at ceiling 0 = exit %d (%v), want ExitRefused naming ceiling 0", Code(err), err)
	}

	// agents start --force goes through agentStartCmd and logs the overridden
	// ceiling-0 verdict in the store.
	stubBackground(t, "backgrounded · "+bgShortID+" · "+busyOne+"\n", bgSession{ID: bgShortID, SessionID: bgID, Name: busyOne})
	outZero.Reset()
	forceCmd := aZero.agentStartCmd()
	forceCmd.SilenceErrors = true
	forceCmd.SilenceUsage = true
	forceCmd.SetArgs([]string{busyOne, brief, dirFlag, dir, modeFlag, config.ModeBackground, forceFlag})
	if err := forceCmd.Execute(); err != nil {
		t.Fatalf("agents start --force at ceiling 0 = %v", err)
	}
	wantOverLine := "started over " + noStartCeilingZero + " (--force)"
	if !strings.Contains(outZero.String(), wantOverLine+"\n") {
		t.Errorf("agents start --force output = %q, want %q", outZero.String(), wantOverLine)
	}
	evs, err := aZero.store.Events(0, func(e state.Event) bool { return e.Verb == verbAgentsStart })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(evs, func(e state.Event) bool { return strings.Contains(e.Detail, busyOne+": "+wantOverLine) }) {
		t.Errorf("agents.start events = %+v, want %q", evs, busyOne+": "+wantOverLine)
	}

	// A queued agents start --builds reloads Capacity from cfgPath when its
	// slot turn arrives, catching a ceiling: 0 written while it waited; it
	// also passes the reloaded Capacity to readHeadroom.
	setCap(cfgPath, "5", "10")
	aQueued, _ := openApp(false)
	// Clear the forced worker so 0 agents are busy when the build start enters.
	if err := aQueued.store.Update(func(st *state.State) ([]state.Event, error) { st.Agents = nil; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	holdSlots(t, filepath.Join(dir, "memcap"))
	vInitial, err := aQueued.startCapacity(context.Background(), true)
	if err != nil || !vInitial.onlySlot() {
		t.Fatalf("initial build start = %+v, %v; want onlySlot", vInitial, err)
	}
	_, err = queueStart(vInitial, true, false, "8m",
		func() (bool, int, error) {
			_, serr := config.Set(cfgPath, []config.Setting{
				{Key: floorKey, Value: "0"},
				{Key: ceilingKey, Value: "0"},
				{Key: "capacity.availMinMiB", Value: "61440"},
			})
			return serr == nil, 0, serr
		},
		func() (*capacityView, error) { return aQueued.startCapacity(context.Background(), false) })
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), noStartCeilingZero) || !strings.Contains(err.Error(), "MemAvailable 45.0 GiB under 60 GiB") {
		t.Errorf("queued start after config set ceiling 0 = exit %d (%v), want ceiling 0 and reloaded availMinMiB", Code(err), err)
	}

	// Watch: reloads Capacity from watchCfg on each poll without mutating
	// w.cfg; ceiling 0 clears open capacity conditions and suppresses new ones,
	// a watcher started at ceiling 0 resumes emitting once a positive ceiling
	// is restored, and floor 0 with a positive ceiling stays silent at 0 busy.
	watchDir := t.TempDir()
	watchCfg := filepath.Join(watchDir, "config.yaml")
	w, _, out := notifyingWatch(t, watchDir, false)
	w.cfgPath = watchCfg
	reads := 0
	w.readHeadroom = func(context.Context, *swapReading) *headroom { reads++; return roomy() }
	w.capacity(context.Background(), supervised(2), nil)
	if !strings.Contains(out.String(), lowTwoOfFive) || reads != 1 {
		t.Fatalf("before ceiling 0 (reads %d):\n%s", reads, out)
	}
	out.Reset()
	setCap(watchCfg, "0", "0")
	w.capacity(context.Background(), supervised(0), nil)
	if !strings.Contains(out.String(), "ENDED "+lowTwoOfFive) || strings.Contains(out.String(), "CAPACITY FULL") || reads != 1 {
		t.Fatalf("at ceiling 0 with 0 busy (reads %d):\n%s", reads, out)
	}
	if w.cfg.Capacity.Ceiling != config.DefaultCeiling {
		t.Errorf("w.cfg.Capacity mutated to %+v", w.cfg.Capacity)
	}
	out.Reset()
	w.capacity(context.Background(), supervised(2), nil)
	if out.Len() != 0 || reads != 1 {
		t.Fatalf("at ceiling 0 with 2 busy (reads %d):\n%s", reads, out)
	}

	// A watcher started at ceiling 0 speaks once watchCfg restores a positive ceiling.
	wFromZero, _, outFromZero := notifyingWatch(t, t.TempDir(), false)
	wFromZero.cfg.Capacity = zero
	wFromZero.cfgPath = watchCfg
	fromZeroReads := 0
	wFromZero.readHeadroom = func(context.Context, *swapReading) *headroom { fromZeroReads++; return roomy() }
	wFromZero.capacity(context.Background(), supervised(2), nil)
	if outFromZero.Len() != 0 || fromZeroReads != 0 {
		t.Fatalf("started at ceiling 0 (reads %d):\n%s", fromZeroReads, outFromZero)
	}
	setCap(watchCfg, "5", "10")
	wFromZero.capacity(context.Background(), supervised(2), nil)
	if !strings.Contains(outFromZero.String(), lowTwoOfFive) || fromZeroReads != 1 {
		t.Fatalf("after restoring positive ceiling (reads %d):\n%s", fromZeroReads, outFromZero)
	}

	out.Reset()
	setCap(watchCfg, "0", "10")
	w.capacity(context.Background(), supervised(0), nil)
	if out.Len() != 0 || reads != 1 {
		t.Fatalf("at floor 0, ceiling 10 with 0 busy (reads %d):\n%s", reads, out)
	}

	// A broken configuration file falls back to w.cfg.Capacity.
	if err := os.WriteFile(watchCfg, []byte("capacity: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := wFromZero.liveCapacity(); got.Floor != zero.Floor || got.Ceiling != zero.Ceiling {
		t.Errorf("liveCapacity on broken file = %+v, want fallback %+v", got, zero)
	}
}
