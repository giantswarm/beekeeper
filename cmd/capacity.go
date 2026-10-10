package cmd

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
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
	// Notes is what a start that fits should know: the busy build slot a
	// start without builds does not wait for.
	Notes []string `json:"notes,omitempty"`
}

// agentCount sorts the roster: busy with a task, parked (kept on purpose, a
// timer wakes it, or it waits on its person) and idle. The supervisor, the
// guide, a role's successor and a role's run relieved since are none of them.
type agentCount struct {
	Busy   []string `json:"busy"`
	Parked []string `json:"parked"`
	Idle   []string `json:"idle"`
}

// countAgents sorts st's roster at now; sessions say which agents wait on
// their person (none known without them).
func countAgents(st *state.State, sessions []*claude.Session, now time.Time) agentCount {
	c := agentCount{Busy: []string{}, Parked: []string{}, Idle: []string{}}
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
		if s, live := claude.Live(sessions, p); live && s.Waiting != nil {
			c.Parked = append(c.Parked, ag.Name+" (waits on its person)")
			continue
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
	return s + fmt.Sprintf("; %d parked, %d idle", len(c.Parked), len(c.Idle))
}

// headroom is what bounds a new start: MemAvailable, the swap's growth over
// the watch's readings, the 1-minute load against the watch's limit, the
// kind labs against their cap, and for a start that builds the free build
// slots.
type headroom struct {
	AvailableMiB int    `json:"availableMiB"`
	AvailMinMiB  int    `json:"availMinMiB"`
	MemErr       string `json:"memError,omitempty"`
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
// disk and zswap, and disk swap's growth per hour over the watch's readings
// with whether MemAvailable fell (Rated once they span minSwapSpan). Disk
// swap growing while MemAvailable falls bounds a start, never the figure in
// use: swap may sit full for days without a byte moving, and zswap's share
// is held in RAM.
type swapReading struct {
	At      time.Time `json:"at"`
	UsedMiB int       `json:"usedMiB"`
	DiskMiB int       `json:"diskMiB"`
	// ZswapMiB is the swap zswap holds compressed in RAM.
	ZswapMiB int `json:"zswapMiB"`
	// PerHourMiB is disk swap's growth.
	PerHourMiB   int  `json:"perHourMiB"`
	AvailFalling bool `json:"availFalling"`
	Rated        bool `json:"rated"`
}

// swapFile is the state's side file the watch keeps its latest swap reading
// in, for capacity; swapFresh is how old a reading may be and still count.
const (
	swapFile  = "swap.json"
	swapFresh = 5 * time.Minute
)

// noFreeSlot is the build slot's guard: a block for a start that builds, a
// note for one that does not.
const noFreeSlot = "no free build slot"

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
	if s := h.Swap; s != nil && s.Rated && s.AvailFalling && s.PerHourMiB > h.SwapGrowthMaxMiB {
		out = append(out, fmt.Sprintf("disk swap growing %+d MiB/h while MemAvailable falls, over %d", s.PerHourMiB, h.SwapGrowthMaxMiB))
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

// slotBusy reports whether no build slot is free.
func (h *headroom) slotBusy() bool { return h.Slots > 0 && h.SlotsFree == 0 }

// line says the headroom in one line.
func (h *headroom) line() string {
	mem := "MemAvailable unknown"
	if h.MemErr == "" {
		mem = fmt.Sprintf("MemAvailable %.1f GiB (floor %d GiB)", float64(h.AvailableMiB)/1024, gib(h.AvailMinMiB))
	}
	swap := "swap growth not measured (no watch reading)"
	if s := h.Swap; s != nil {
		swap = fmt.Sprintf("disk swap %d MiB, zswap %d MiB, growth not yet measured", s.DiskMiB, s.ZswapMiB)
		if s.Rated {
			swap = fmt.Sprintf("disk swap %d MiB %+d MiB/h (max %d), zswap %d MiB", s.DiskMiB, s.PerHourMiB, h.SwapGrowthMaxMiB, s.ZswapMiB)
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
// one that does not is told the slot is busy.
func newCapacity(c agentCount, k config.Capacity, h *headroom, builds bool) *capacityView {
	v := &capacityView{agentCount: c, Floor: k.Floor, Ceiling: k.Ceiling, Headroom: h, Builds: builds}
	if busy := len(c.Busy); busy >= k.Ceiling {
		v.Blocks = append(v.Blocks, fmt.Sprintf("%d busy at the ceiling of %d", busy, k.Ceiling))
	}
	v.Blocks = append(v.Blocks, h.blocks(builds)...)
	if !builds && h.slotBusy() {
		v.Notes = append(v.Notes, noFreeSlot+": a start that builds (--builds) waits for it")
	}
	if len(v.Blocks) == 0 {
		v.Room = k.Ceiling - len(c.Busy)
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

// readHeadroom reads the machine's headroom now; swap is the watch's latest
// reading, nil without a fresh one.
func (a *app) readHeadroom(ctx context.Context, swap *swapReading) *headroom {
	k := a.cfg.Capacity
	h := &headroom{AvailMinMiB: k.AvailMinMiB, SwapGrowthMaxMiB: k.SwapGrowthMaxMiB, Swap: swap, Slots: a.cfg.Memcap.Slots}
	m, err := plat.Machine.Mem()
	if err != nil {
		h.MemErr = err.Error()
	}
	h.AvailableMiB = m.AvailableMiB
	h.MaxLabs = a.cfg.KindClusters(m.TotalMiB)
	load, err := plat.Machine.Load()
	if err != nil {
		h.LoadErr = err.Error()
	}
	h.Load1, h.LoadMax = load[0], a.cfg.Watch.LoadLimit(runtime.NumCPU())
	for _, s := range machine.ReadSlots(a.cfg.Memcap.SlotDir, a.cfg.Memcap.Slots) {
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
capacity.ceiling) and say whether a new start fits. Busy is a roster agent
with a task that is neither parked nor kept: an agent kept on purpose
(agents keep), one a timer wakes and one whose session waits on its person
are parked; one without a task, or done, is idle. The supervisor, the guide
and a role's successor are not counted.

The headroom bounds a start: MemAvailable against capacity.availMinMiB,
disk swap's growth over the watch's readings against
capacity.swapGrowthMaxMiB while MemAvailable falls (never the swap in
use, and never zswap's share, which sits in RAM), the 1-minute load
against the watch's HIGH LOAD limit (watch.loadMax, or
watch.loadPerCoreMax × cores) and the kind labs against their cap. The
build slot bounds only a start that builds: --builds judges one whose task
builds, tests or lints (beekeeper run takes the slot), and the verdict is
"no start: no free build slot" while none is free. Without --builds the
verdict is for a start that does not build (a filer, a dispatcher, a
reviewer, a promoter proving a release read-only), which has no use for
the slot: a busy slot is a note in parentheses after the verdict, never a
"no start". The verdict is "room for N starts" up to the ceiling, or what
blocks one, each guard that refuses named. agents start judges its start
the same way, --builds marking one that builds. capacity reads the roster,
the sessions and the machine and changes nothing; it makes no GitHub call.`,
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
			v := newCapacity(c, a.cfg.Capacity, a.readHeadroom(cmd.Context(), a.watchSwap()), builds)
			if a.json {
				return a.printJSON(struct {
					*capacityView
					Verdict string `json:"verdict"`
				}{v, v.verdict()})
			}
			p := func(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format+"\n", args...) }
			p("capacity: %s", c.line(a.cfg.Capacity))
			if len(c.Parked) > 0 {
				p("parked: %s", strings.Join(c.Parked, ", "))
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
	case v.Room > 0:
		return "", nil
	case force:
		return v.verdict(), nil
	}
	return "", refused("%s (beekeeper capacity; --force starts anyway)", v.verdict())
}

// startGate reads the capacity verdict for a start that builds or not, as
// beekeeper capacity reads it, and judges it (gateStart).
func (a *app) startGate(ctx context.Context, builds, force bool) (string, error) {
	st, err := a.store.Read()
	if err != nil {
		return "", err
	}
	sessions, _, err := a.sessions()
	if err != nil && !platform.Missing(err) {
		return "", err
	}
	c := countAgents(st, sessions, a.now)
	return gateStart(newCapacity(c, a.cfg.Capacity, a.readHeadroom(ctx, a.watchSwap()), builds), force)
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
// and the headroom allows a start, and CAPACITY FULL once at the ceiling,
// each with the headroom; each ends with one ENDED line when the count
// recovers. The machine is read only at those edges.
func (w *watcher) capacity(ctx context.Context, st *state.State, sessions []*claude.Session) {
	k := w.cfg.Capacity
	c := countAgents(st, sessions, w.now)
	busy := len(c.Busy)
	if busy >= k.Floor && busy < k.Ceiling {
		w.clear("capacity-low")
		w.clear("capacity-full")
		return
	}
	w.mu.Lock()
	swap := w.swap
	w.mu.Unlock()
	read := w.readHeadroom
	if read == nil {
		read = w.app.readHeadroom
	}
	// The watch weighs a start that does not build: a busy slot is a note.
	v := newCapacity(c, k, read(ctx, swap), false)
	w.check("capacity-low", busy < k.Floor && v.Room > 0, "CAPACITY LOW %d of %d: %s; %s", busy, k.Floor, v.verdict(), v.Headroom.line())
	w.check("capacity-full", busy >= k.Ceiling, "CAPACITY FULL %d of %d: %s", busy, k.Ceiling, v.Headroom.line())
}
