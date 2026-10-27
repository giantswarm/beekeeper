package cmd

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/state"
)

// capacityView is what capacity shows: the roster's busy, parked and idle
// agents against the target, the headroom a new start needs and the verdict.
type capacityView struct {
	agentCount
	Floor    int       `json:"floor"`
	Ceiling  int       `json:"ceiling"`
	Headroom *headroom `json:"headroom"`
	// Builds says the start judged builds: the build slot is then one of
	// its guards.
	Builds bool `json:"builds"`
	// Room is how many starts fit; Blocks says what stops one when none does.
	Room   int      `json:"room"`
	Blocks []string `json:"blocks,omitempty"`
	// Notes is what a start that fits should know: disk swap growing
	// without memory pressure, the busy build slot a start without builds
	// does not wait for.
	Notes []string `json:"notes,omitempty"`
	// Stopped says the standing beekeeper stop, which no --force passes.
	Stopped string `json:"stopped,omitempty"`
}

// agentCount sorts the roster: busy with a task, parked (kept on purpose, a
// timer wakes it, or it waits on its person), waiting in a wait of its own
// and idle. Only busy agents hold a slot. The supervisor, the guide, a
// role's successor and a role's run relieved since are none of them.
type agentCount struct {
	Busy    []string `json:"busy"`
	Parked  []string `json:"parked"`
	Waiting []string `json:"waiting"`
	Idle    []string `json:"idle"`
}

// waitQuiet is how long a session whose tool commands only wait must have
// been quiet before its agent counts as waiting rather than busy: a turn
// that still works writes its transcript between its commands.
const waitQuiet = 5 * time.Minute

// countAgents sorts st's roster at now; sessions say which agents wait on
// their person or in a wait of their own (none known without them).
func countAgents(st *state.State, sessions []*claude.Session, now time.Time) agentCount {
	c := agentCount{Busy: []string{}, Parked: []string{}, Waiting: []string{}, Idle: []string{}}
	sup, guide := st.SupervisorRole(), st.GuideRole()
	for _, ag := range st.Agents {
		p := ag.Party
		if roleRun(st, p) || sup.Relay.Open(now) && sup.Relay.To.Is(p) || guide.Relay.Open(now) && guide.Relay.To.Is(p) {
			continue
		}
		if ag.Task == "" || ag.Done {
			c.Idle = append(c.Idle, ag.Name)
			continue
		}
		if k := keptBy(st, ag, now); k != "" {
			c.Parked = append(c.Parked, fmt.Sprintf("%s (%s)", ag.Name, k))
			continue
		}
		s, live := claude.Live(sessions, p)
		if live && s.Waiting != nil {
			c.Parked = append(c.Parked, ag.Name+" (waits on its person)")
			continue
		}
		if live {
			if w := s.OwnWait(now, waitQuiet); w != "" {
				c.Waiting = append(c.Waiting, fmt.Sprintf("%s (waits: %s)", ag.Name, w))
				continue
			}
		}
		c.Busy = append(c.Busy, ag.Name)
	}
	return c
}

// line says the count against the target in one line.
func (c agentCount) line(k config.Capacity) string {
	s := fmt.Sprintf("%d busy of floor %d, ceiling %d", len(c.Busy), k.Floor, k.Ceiling)
	if len(c.Busy) > 0 {
		s += ": " + strings.Join(c.Busy, ", ")
	}
	return s + fmt.Sprintf("; %d parked, %d waiting, %d idle", len(c.Parked), len(c.Waiting), len(c.Idle))
}

// headroom is what bounds a new start: MemAvailable, disk swap's growth
// over the watch's readings weighed with memory pressure and MemAvailable,
// the 1-minute load against the watch's limit, the kind labs against their
// cap, and for a start that builds the free build slots.
type headroom struct {
	AvailableMiB int    `json:"availableMiB"`
	AvailMinMiB  int    `json:"availMinMiB"`
	MemErr       string `json:"memError,omitempty"`
	// PSI is memory pressure now ("full avg60" of /proc/pressure/memory,
	// in percent), PSIMax the most under which disk swap's growth is a
	// warning rather than a block (capacity.swapPSIMax).
	PSI    float64 `json:"psi"`
	PSIMax float64 `json:"psiMax"`
	PSIErr string  `json:"psiError,omitempty"`
	// Swap is the watch's latest swap reading; nil when no fresh one exists.
	Swap             *swapReading `json:"swap,omitempty"`
	SwapGrowthMaxMiB int          `json:"swapGrowthMaxMiB"`
	// Load1 is the 1-minute load average, LoadMax the watch's HIGH LOAD
	// limit (watch.loadMax, or watch.loadPerCoreMax × cores).
	Load1     float64 `json:"load1"`
	LoadMax   float64 `json:"loadMax"`
	LoadErr   string  `json:"loadError,omitempty"`
	SlotsFree int     `json:"slotsFree"`
	Slots     int     `json:"slots"`
	Labs      int     `json:"labs"`
	MaxLabs   int     `json:"maxLabs"`
	LabsErr   string  `json:"labsError,omitempty"`
}

// swapReading is a watch's latest machine swap sample: in use, split into
// disk and zswap, and per hour over the watch's readings of
// watch.swapWindow (Rated once they span config.MinSwapSpan) disk swap's
// growth from new swap-outs and zswap's writeback apart. Disk swap growing
// weighs against a start, with memory pressure and MemAvailable, never the
// figure in use: swap may sit full for days without a byte moving, zswap's
// share is held in RAM, and its writeback moves pages already swapped.
type swapReading struct {
	At      time.Time `json:"at"`
	UsedMiB int       `json:"usedMiB"`
	DiskMiB int       `json:"diskMiB"`
	// ZswapMiB is the swap zswap holds compressed in RAM.
	ZswapMiB int `json:"zswapMiB"`
	// PerHourMiB is disk swap's growth less zswap's writeback.
	PerHourMiB int `json:"perHourMiB"`
	// WritebackPerHourMiB is zswap's writeback to disk (zswpwb of
	// /proc/vmstat).
	WritebackPerHourMiB int  `json:"writebackPerHourMiB"`
	Rated               bool `json:"rated"`
}

// swapFile is the state's side file the watch keeps its latest swap reading
// in, for capacity; swapFresh is how old a reading may be and still count.
const (
	swapFile  = "swap.json"
	swapFresh = 5 * time.Minute
)

// noFreeSlot is the build slot's guard: a block for a start that builds, a
// note for one that does not. ceilingZero is the block when capacity.ceiling
// is configured as 0.
const (
	noFreeSlot  = "no free build slot"
	ceilingZero = "ceiling 0"
)

// blocks says what stops a new start: memory, swap, load and the labs stop
// any; the build slot stops one that builds, which waits for it, while a
// start that does not build has no use for it.
func (h *headroom) blocks(builds bool) []string {
	var out []string
	switch {
	case h.MemErr != "":
		out = append(out, "MemAvailable unknown ("+h.MemErr+")")
	case h.AvailableMiB < h.AvailMinMiB:
		out = append(out, fmt.Sprintf("MemAvailable %.1f GiB under %d GiB", float64(h.AvailableMiB)/1024, gib(h.AvailMinMiB)))
	}
	if line, blocks := h.swapGuard(); blocks {
		out = append(out, line)
	}
	if h.LoadErr == "" && h.LoadMax > 0 && h.Load1 > h.LoadMax {
		out = append(out, fmt.Sprintf("1m load %.0f over %.0f", h.Load1, h.LoadMax))
	}
	if builds && h.slotBusy() {
		out = append(out, noFreeSlot)
	}
	if h.LabsErr == "" && h.Labs > h.MaxLabs {
		out = append(out, fmt.Sprintf("%d kind labs over the cap of %d", h.Labs, h.MaxLabs))
	}
	return out
}

// swapGuard weighs disk swap's growth with memory pressure and
// MemAvailable, the three figures in its line: a growth over
// SwapGrowthMaxMiB blocks a start under pressure (PSI over PSIMax or
// MemAvailable under the floor; either unknown counts as pressure) and is
// a warning without, a single writeback burst having left the rate after
// watch.swapWindow. The line is "" while the growth is unrated or within
// the max.
func (h *headroom) swapGuard() (line string, blocks bool) {
	s := h.Swap
	if s == nil || !s.Rated || s.PerHourMiB <= h.SwapGrowthMaxMiB {
		return "", false
	}
	psi, avail := h.psiFigure(), h.availFigure()
	pressure := "without pressure"
	switch {
	case h.PSIErr != "" || h.MemErr != "":
		pressure, blocks = "pressure unknown", true
	case h.PSI > h.PSIMax || h.AvailableMiB < h.AvailMinMiB:
		pressure, blocks = "under pressure", true
	}
	return fmt.Sprintf("disk swap growing %+d MiB/h over %d, %s: %s, %s", s.PerHourMiB, h.SwapGrowthMaxMiB, pressure, psi, avail), blocks
}

// psiFigure says memory pressure against the swap guard's max.
func (h *headroom) psiFigure() string {
	switch {
	case h.PSIErr != "":
		return "memory PSI unknown (" + h.PSIErr + ")"
	case h.PSI > h.PSIMax:
		return fmt.Sprintf("memory PSI %s over %s", pct(h.PSI), pct(h.PSIMax))
	}
	return fmt.Sprintf("memory PSI %s (max %s)", pct(h.PSI), pct(h.PSIMax))
}

// availFigure says MemAvailable against the floor.
func (h *headroom) availFigure() string {
	switch {
	case h.MemErr != "":
		return "MemAvailable unknown (" + h.MemErr + ")"
	case h.AvailableMiB < h.AvailMinMiB:
		return fmt.Sprintf("MemAvailable %.1f GiB under the floor of %d GiB", float64(h.AvailableMiB)/1024, gib(h.AvailMinMiB))
	}
	return fmt.Sprintf("MemAvailable %.1f GiB (floor %d GiB)", float64(h.AvailableMiB)/1024, gib(h.AvailMinMiB))
}

// pct renders a pressure share as read ("0%", "0.4%", "12%").
func pct(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) + "%" }

// slotBusy reports whether no build slot is free.
func (h *headroom) slotBusy() bool { return h.Slots > 0 && h.SlotsFree == 0 }

// line says the headroom in one line.
func (h *headroom) line() string {
	mem := "MemAvailable unknown"
	if h.MemErr == "" {
		mem = fmt.Sprintf("MemAvailable %.1f GiB (floor %d GiB)", float64(h.AvailableMiB)/1024, gib(h.AvailMinMiB))
	}
	mem += ", " + h.psiFigure()
	swap := "swap growth not measured (no watch reading)"
	if s := h.Swap; s != nil {
		swap = fmt.Sprintf("disk swap %d MiB, zswap %d MiB, growth not yet measured", s.DiskMiB, s.ZswapMiB)
		if s.Rated {
			swap = fmt.Sprintf("disk swap %d MiB %+d MiB/h (max %d), zswap %d MiB writing back %d MiB/h", s.DiskMiB, s.PerHourMiB, h.SwapGrowthMaxMiB, s.ZswapMiB, s.WritebackPerHourMiB)
		}
	}
	load := "load unknown"
	if h.LoadErr == "" {
		load = fmt.Sprintf("1m load %.0f (max %.0f)", h.Load1, h.LoadMax)
	}
	labs := fmt.Sprintf("kind labs %d of %d", h.Labs, h.MaxLabs)
	if h.LabsErr != "" {
		labs = "kind labs unknown"
	}
	return fmt.Sprintf("headroom: %s, %s, %s, build slots %d of %d free, %s", mem, swap, load, h.SlotsFree, h.Slots, labs)
}

// newCapacity weighs the count against the target and the headroom for a
// start that builds or not: only one that builds waits for the build slot,
// one that does not is told the slot is busy; disk swap growing without
// pressure is a note, not a block.
func newCapacity(c agentCount, k config.Capacity, h *headroom, builds bool) *capacityView {
	v := &capacityView{agentCount: c, Floor: k.Floor, Ceiling: k.Ceiling, Headroom: h, Builds: builds}
	switch busy := len(c.Busy); {
	case k.Ceiling == 0:
		v.Blocks = append(v.Blocks, ceilingZero)
	case busy >= k.Ceiling:
		v.Blocks = append(v.Blocks, fmt.Sprintf("%d busy at the ceiling of %d", busy, k.Ceiling))
	}
	v.Blocks = append(v.Blocks, h.blocks(builds)...)
	if line, blocks := h.swapGuard(); line != "" && !blocks {
		v.Notes = append(v.Notes, line)
	}
	if !builds && h.slotBusy() {
		v.Notes = append(v.Notes, noFreeSlot+": a start that builds (--builds) waits for it")
	}
	if len(v.Blocks) == 0 {
		v.Room = k.Ceiling - len(c.Busy)
	}
	return v
}

// stopped makes the verdict "no start" while the stop s stands.
func (v *capacityView) stopped(a *app, s *state.Stop) *capacityView {
	if s != nil {
		v.Stopped = a.stopText(s)
		v.Blocks, v.Room = append([]string{v.Stopped}, v.Blocks...), 0
	}
	return v
}

// verdict is the one-line answer: room for N starts, or what blocks one,
// with the notes in parentheses.
func (v *capacityView) verdict() string {
	var s string
	switch {
	case v.Room == 1:
		s = "room for 1 start"
	case v.Room > 1:
		s = fmt.Sprintf("room for %d starts", v.Room)
	default:
		s = "no start: " + strings.Join(v.Blocks, "; ")
	}
	if len(v.Notes) > 0 {
		s += " (" + strings.Join(v.Notes, "; ") + ")"
	}
	return s
}

// liveCapacity returns the current Capacity from the active configuration
// file (cfgPath or loadedCfg) without mutating a.cfg across goroutines; it
// falls back to a.cfg.Capacity when no configuration file path is active or
// on a read or parse error.
func (a *app) liveCapacity() config.Capacity {
	if p := cmp.Or(a.cfgPath, a.loadedCfg); p != "" {
		if c, err := config.Load(p); err == nil {
			return c.Capacity
		}
	}
	return a.cfg.Capacity
}

// readHeadroom reads the machine's headroom now against k; swap is the
// watch's latest reading, nil without a fresh one.
func (a *app) readHeadroom(ctx context.Context, k config.Capacity, swap *swapReading) *headroom {
	slotDir, slots := a.slotView()
	h := &headroom{AvailMinMiB: k.AvailMinMiB, SwapGrowthMaxMiB: k.SwapGrowthMaxMiB, PSIMax: k.SwapPSIMax, Swap: swap, Slots: slots}
	m, err := plat.Machine.Mem()
	if err != nil {
		h.MemErr = err.Error()
	}
	h.AvailableMiB = m.AvailableMiB
	h.MaxLabs = a.cfg.KindClusters(m.TotalMiB)
	psi, err := plat.Machine.MemoryPressure()
	if err != nil {
		h.PSIErr = err.Error()
	}
	h.PSI = psi
	load, err := plat.Machine.Load()
	if err != nil {
		h.LoadErr = err.Error()
	}
	h.Load1, h.LoadMax = load[0], a.cfg.Watch.LoadLimit(runtime.NumCPU())
	for _, s := range machine.ReadSlots(slotDir, slots) {
		if s.Free {
			h.SlotsFree++
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cs, err := machine.KindClusters(ctx)
	if err != nil {
		h.LabsErr = err.Error()
	}
	h.Labs = len(cs)
	return h
}

// watchSwap is the watch's latest swap reading, nil when none is fresh.
func (a *app) watchSwap() *swapReading {
	var r swapReading
	if found, err := a.store.ReadFile(swapFile, &r); err != nil || !found || a.now.Sub(r.At) > swapFresh {
		return nil
	}
	return &r
}

func (a *app) capacityCmd() *cobra.Command {
	var builds bool
	c := &cobra.Command{
		Use:   "capacity [--builds]",
		Short: "Busy agents against the target, the headroom for a start and the verdict",
		Long: `Count the roster's agents against the supervisor's target (capacity.floor,
capacity.ceiling; unset, 5 and 10) and say whether a new start fits. A
configured capacity.ceiling of 0 stops new starts ("no start: ceiling 0")
and suppresses CAPACITY LOW and CAPACITY FULL in the watch; capacity.floor
of 0 suppresses CAPACITY LOW. Busy is a roster agent
with a task that is neither parked, kept nor waiting: an agent kept on
purpose (agents keep), one a timer wakes and one whose session waits on its
person are parked; one whose session runs only waits (a sleep or a polling
loop's sleep, gh run watch or --watch, devctl pr wait, pr merge or release
wait, beekeeper gate, beekeeper lease --wait) and whose transcript and its
subagents' transcripts have been quiet for 5 minutes is waiting, listed
apart with what it waits on; one
without a task, or done, is idle. Only busy agents hold a slot: parked and
waiting agents do no work, spend next to no budget and never block a start.
The supervisor, the guide and a role's successor are not counted.

The headroom bounds a start: MemAvailable against capacity.availMinMiB,
disk swap's growth over the watch's readings of watch.swapWindow against
capacity.swapGrowthMaxMiB (never the swap in use, never zswap's share,
which sits in RAM, and never zswap's writeback), the 1-minute load against the watch's HIGH LOAD limit
(watch.loadMax, or watch.loadPerCoreMax × cores) and the kind labs against
their cap. Disk swap's growth is the swap in use less zswap's share
(SwapTotal − SwapFree − Zswapped of /proc/meminfo) less zswap's writeback
(zswpwb of /proc/vmstat, pages zswap moves from its pool to the swap
device), so only new swap-outs count; the writeback is shown apart as
"writing back N MiB/h". Disk swap's growth is weighed with memory pressure and
MemAvailable: over the max it blocks a start under pressure (memory PSI,
full avg60, over capacity.swapPSIMax, or MemAvailable under the floor) and
is a warning in parentheses after the verdict without, the line naming
the three figures. The build slot bounds only a start that builds:
--builds judges one whose task builds, tests or lints (beekeeper run takes
the slot), and the verdict is "no start: no free build slot" while none is
free; agents start --builds then waits its turn in the build slot queue
with beekeeper run's waiters. Without --builds the verdict is for a start that does not build (a
filer, a dispatcher, a reviewer, a promoter proving a release read-only),
which has no use for the slot: a busy slot is a note in parentheses after
the verdict, never a "no start". The verdict is "room for N starts" up to
the ceiling, or what blocks one, each guard that refuses named; while
beekeeper stop stands it is "no start: beekeeper is stopped …" with the
stop's reason. agents
start judges its start the same way, --builds marking one that builds.
capacity reads the roster, the sessions and the machine and changes
nothing; it makes no GitHub call.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			sessions, _, err := a.sessions()
			if err != nil && !platform.Missing(err) {
				return err
			}
			c := countAgents(st, sessions, a.now)
			k := a.liveCapacity()
			v := newCapacity(c, k, a.readHeadroom(cmd.Context(), k, a.watchSwap()), builds).stopped(a, st.Stop)
			if a.json {
				return a.printJSON(struct {
					*capacityView
					Verdict string `json:"verdict"`
				}{v, v.verdict()})
			}
			p := func(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format+"\n", args...) }
			p("capacity: %s", c.line(k))
			if len(c.Parked) > 0 {
				p("parked: %s", strings.Join(c.Parked, ", "))
			}
			if len(c.Waiting) > 0 {
				p("waiting: %s", strings.Join(c.Waiting, ", "))
			}
			if len(c.Idle) > 0 {
				p("idle: %s", strings.Join(c.Idle, ", "))
			}
			p("%s", v.Headroom.line())
			p("%s", v.verdict())
			return nil
		},
	}
	c.Flags().BoolVar(&builds, buildsFlag, false, "the verdict for a start that builds, which waits for a free build slot")
	return c
}

// buildsFlag marks a start whose task builds: the build slot is one of its
// guards.
const buildsFlag = "builds"

// gateStart judges capacity's verdict v for a start: room lets it through
// (nothing to say), none refuses it (exit 3, the verdict's guards named)
// unless force, which lets it through and returns the verdict overridden
// for the event log.
func gateStart(v *capacityView, force bool) (string, error) {
	switch {
	case v.Stopped != "":
		return "", refused("%s", v.verdict())
	case v.Room > 0:
		return "", nil
	case force:
		return v.verdict(), nil
	}
	return "", refused("%s (beekeeper capacity; --force starts anyway)", v.verdict())
}

// startGate reads the capacity verdict for a start that builds or not, as
// beekeeper capacity reads it, and judges it (queueStart).
func (a *app) startGate(ctx context.Context, name string, builds, force bool) (string, error) {
	v, err := a.startCapacity(ctx, builds)
	if err != nil {
		return "", err
	}
	o, wait, err := a.startSlot()
	if err != nil {
		return "", err
	}
	return queueStart(v, builds, force, wait,
		func() (bool, int, error) { return guard.AwaitSlot(o, "agents start "+name) },
		func() (*capacityView, error) { return a.startCapacity(ctx, false) })
}

// startSlot is the build slot a start that builds queues for, in the slots
// beekeeper run takes (buildSlots), and the wait for its turn: run's
// default, MEMCAP_WAIT.
func (a *app) startSlot() (guard.Options, string, error) {
	o := guard.Options{Stderr: os.Stderr}
	var err error
	if o.SlotDir, o.Slots, err = a.buildSlots(); err != nil {
		return o, "", err
	}
	var wait string
	wait, o.WaitFrom, o.Background = guard.ResolveWait("", false)
	if o.Wait, err = guard.ParseWait(wait); err != nil {
		return o, "", usageErr("%v", err)
	}
	return o, wait, nil
}

// dryRunSlot prints the build slot a start that builds would find now, as
// its start resolves it, and claims nothing: "free", or the wait its start
// would take its turn in, with the slots' holders and the waiters ahead.
func (a *app) dryRunSlot() error {
	o, wait, err := a.startSlot()
	if err != nil {
		return err
	}
	free, why, err := guard.PeekSlot(o)
	if err != nil {
		return err
	}
	line := "build slot: free"
	if !free {
		line = fmt.Sprintf("build slot: busy, the start waits its turn up to %s for %s", wait, strings.TrimSuffix(why, "\n"))
	}
	_, err = fmt.Fprintln(a.out, line)
	return err
}

// queueStart judges a start's verdict v (gateStart). A start that builds,
// refused by the busy build slot alone, first waits its turn in the slot
// queue with the runs (await, up to wait): first in first out, instead of a
// refusal that a slot held by run after run never lets through. Its turn
// come, the start is judged again (again) as one the slot no longer stops;
// a turn that does not come within the wait refuses it, the waiters still
// ahead named.
func queueStart(v *capacityView, builds, force bool, wait string, await func() (bool, int, error), again func() (*capacityView, error)) (string, error) {
	if !builds || force || !v.onlySlot() {
		return gateStart(v, force)
	}
	turn, ahead, err := await()
	if err != nil {
		return "", err
	}
	if !turn {
		return "", refused("%s, %d ahead in the build slot queue after %s (beekeeper capacity; --force starts anyway)", v.verdict(), ahead, wait)
	}
	if v, err = again(); err != nil {
		return "", err
	}
	return gateStart(v, force)
}

// startCapacity is capacity's verdict for a start now.
func (a *app) startCapacity(ctx context.Context, builds bool) (*capacityView, error) {
	st, err := a.store.Read()
	if err != nil {
		return nil, err
	}
	sessions, _, err := a.sessions()
	if err != nil && !platform.Missing(err) {
		return nil, err
	}
	c := countAgents(st, sessions, a.now)
	k := a.liveCapacity()
	return newCapacity(c, k, a.readHeadroom(ctx, k, a.watchSwap()), builds).stopped(a, st.Stop), nil
}

// onlySlot reports whether the busy build slot alone stops the start.
func (v *capacityView) onlySlot() bool {
	return v.Room == 0 && len(v.Blocks) == 1 && v.Blocks[0] == noFreeSlot
}

// keepSwap keeps the machine sample's swap reading for the poll and, in the
// state's side file, for capacity.
func (w *watcher) keepSwap(r *swapReading) {
	w.mu.Lock()
	w.swap = r
	w.mu.Unlock()
	_ = w.store.WriteFile(swapFile, r)
}

// capacity says CAPACITY LOW once while fewer agents are busy than the floor
// and the headroom allows a start, and CAPACITY FULL once at a positive
// ceiling, each with the headroom; each ends with one ENDED line when the
// count recovers, and a ceiling of 0 clears both and stays silent. The
// machine is read only at those edges.
func (w *watcher) capacity(ctx context.Context, st *state.State, sessions []*claude.Session) {
	k := w.liveCapacity()
	c := countAgents(st, sessions, w.now)
	busy := len(c.Busy)
	if k.Ceiling == 0 || (busy >= k.Floor && busy < k.Ceiling) || st.Stop != nil {
		w.clear("capacity-low")
		w.clear("capacity-full")
		return
	}
	w.mu.Lock()
	swap := w.swap
	w.mu.Unlock()
	var h *headroom
	if w.readHeadroom != nil {
		h = w.readHeadroom(ctx, swap)
	} else {
		h = w.app.readHeadroom(ctx, k, swap)
	}
	// The watch weighs a start that does not build: a busy slot is a note.
	v := newCapacity(c, k, h, false)
	w.check("capacity-low", busy < k.Floor && v.Room > 0, "CAPACITY LOW %d of %d: %s; %s", busy, k.Floor, v.verdict(), v.Headroom.line())
	w.check("capacity-full", busy >= k.Ceiling, "CAPACITY FULL %d of %d: %s", busy, k.Ceiling, v.Headroom.line())
}
