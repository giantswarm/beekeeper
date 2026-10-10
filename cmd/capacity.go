package cmd

import (
	"context"
	"fmt"
	"strconv"
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
	// Room is how many starts fit; Blocks says what stops one when none does.
	Room   int      `json:"room"`
	Blocks []string `json:"blocks,omitempty"`
	// Notes is what a start that fits should know: disk swap growing
	// without memory pressure.
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

// headroom is what bounds a new start: MemAvailable, disk swap's growth
// over the watch's readings weighed with memory pressure and MemAvailable,
// the free build slots and the kind labs against their cap.
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
	SlotsFree        int          `json:"slotsFree"`
	Slots            int          `json:"slots"`
	Labs             int          `json:"labs"`
	MaxLabs          int          `json:"maxLabs"`
	LabsErr          string       `json:"labsError,omitempty"`
}

// swapReading is a watch's latest machine swap sample: in use, split into
// disk and zswap, and disk swap's growth per hour over the watch's readings
// of watch.swapWindow (Rated once they span config.MinSwapSpan). Disk swap
// growing weighs against a start, with memory pressure and MemAvailable,
// never the figure in use: swap may sit full for days without a byte
// moving, and zswap's share is held in RAM.
type swapReading struct {
	At      time.Time `json:"at"`
	UsedMiB int       `json:"usedMiB"`
	DiskMiB int       `json:"diskMiB"`
	// ZswapMiB is the swap zswap holds compressed in RAM.
	ZswapMiB int `json:"zswapMiB"`
	// PerHourMiB is disk swap's growth.
	PerHourMiB int  `json:"perHourMiB"`
	Rated      bool `json:"rated"`
}

// swapFile is the state's side file the watch keeps its latest swap reading
// in, for capacity; swapFresh is how old a reading may be and still count.
const (
	swapFile  = "swap.json"
	swapFresh = 5 * time.Minute
)

// blocks says what stops a new start.
func (h *headroom) blocks() []string {
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
	if h.Slots > 0 && h.SlotsFree == 0 {
		out = append(out, "no free build slot")
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
			swap = fmt.Sprintf("disk swap %d MiB %+d MiB/h (max %d), zswap %d MiB", s.DiskMiB, s.PerHourMiB, h.SwapGrowthMaxMiB, s.ZswapMiB)
		}
	}
	labs := fmt.Sprintf("kind labs %d of %d", h.Labs, h.MaxLabs)
	if h.LabsErr != "" {
		labs = "kind labs unknown"
	}
	return fmt.Sprintf("headroom: %s, %s, build slots %d of %d free, %s", mem, swap, h.SlotsFree, h.Slots, labs)
}

// newCapacity weighs the count against the target and the headroom; disk
// swap growing without pressure is a note, not a block.
func newCapacity(c agentCount, k config.Capacity, h *headroom) *capacityView {
	v := &capacityView{agentCount: c, Floor: k.Floor, Ceiling: k.Ceiling, Headroom: h}
	if busy := len(c.Busy); busy >= k.Ceiling {
		v.Blocks = append(v.Blocks, fmt.Sprintf("%d busy at the ceiling of %d", busy, k.Ceiling))
	}
	v.Blocks = append(v.Blocks, h.blocks()...)
	if line, blocks := h.swapGuard(); line != "" && !blocks {
		v.Notes = append(v.Notes, line)
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
	h := &headroom{AvailMinMiB: k.AvailMinMiB, SwapGrowthMaxMiB: k.SwapGrowthMaxMiB, PSIMax: k.SwapPSIMax, Swap: swap, Slots: a.cfg.Memcap.Slots}
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
	return &cobra.Command{
		Use:   "capacity",
		Short: "Busy agents against the target, the headroom for a start and the verdict",
		Long: `Count the roster's agents against the supervisor's target (capacity.floor,
capacity.ceiling) and say whether a new start fits. Busy is a roster agent
with a task that is neither parked nor kept: an agent kept on purpose
(agents keep), one a timer wakes and one whose session waits on its person
are parked; one without a task, or done, is idle. The supervisor, the guide
and a role's successor are not counted.

The headroom bounds a start: MemAvailable against capacity.availMinMiB,
disk swap's growth over the watch's readings of watch.swapWindow against
capacity.swapGrowthMaxMiB (never the swap in use, and never zswap's share,
which sits in RAM), the free build slots and the kind labs against their
cap. Disk swap's growth is weighed with memory pressure and MemAvailable:
over the max it blocks a start under pressure (memory PSI, full avg60,
over capacity.swapPSIMax, or MemAvailable under the floor) and is a
warning in parentheses after the verdict without, the line naming the
three figures. The verdict is "room for N starts" up to the ceiling, or
what blocks one. capacity reads the roster, the sessions and the machine
and changes nothing; it makes no GitHub call.`,
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
			v := newCapacity(c, a.cfg.Capacity, a.readHeadroom(cmd.Context(), a.watchSwap()))
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
	v := newCapacity(c, k, read(ctx, swap))
	w.check("capacity-low", busy < k.Floor && v.Room > 0, "CAPACITY LOW %d of %d: %s; %s", busy, k.Floor, v.verdict(), v.Headroom.line())
	w.check("capacity-full", busy >= k.Ceiling, "CAPACITY FULL %d of %d: %s", busy, k.Ceiling, v.Headroom.line())
}
