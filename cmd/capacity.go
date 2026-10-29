package cmd

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/state"
)

// capacityView is what capacity shows: the roster's busy, parked and idle
// agents against the target, the headroom a new start needs and the verdict.
type capacityView struct {
	agentCount
	Floor     int                      `json:"floor"`
	Ceiling   int                      `json:"ceiling"`
	Usage     *usageView               `json:"usage,omitempty"`
	Providers map[string]*providerView `json:"providers,omitempty"`
	Choice    *choiceView              `json:"choice,omitempty"`
	Headroom  *headroom                `json:"headroom"`
	// Builds says the start judged builds: the build slot is then one of
	// its guards.
	Builds bool `json:"builds"`
	// Room is how many starts fit; Blocks says what stops one when none does.
	Room   int      `json:"room"`
	Blocks []string `json:"blocks,omitempty"`
	// subRoomCap is the room capacity.ceiling still leaves the subscription
	// providers together; selfHostedRoom is the room the self-hosted providers
	// have, which capacity.ceiling never bounds. forTarget bounds a target
	// that resolves to no provider of its own by the subscription side alone.
	subRoomCap     int
	selfHostedRoom int
	// Notes is what a start that fits should know: disk swap growing
	// without memory pressure, the busy build slot a start without builds
	// does not wait for.
	Notes []string `json:"notes,omitempty"`
	// Stopped says the standing beekeeper stop, which no --force passes.
	Stopped string `json:"stopped,omitempty"`
}

// bucketView is one usage bucket's windows and derived target within a
// multi-bucket provider such as agy.
type bucketView struct {
	Name     string       `json:"name"`
	Busy     []string     `json:"busy"`
	Floor    int          `json:"floor"`
	Ceiling  int          `json:"ceiling"`
	Room     int          `json:"room"`
	FiveHour *usageWindow `json:"fiveHour,omitempty"`
	SevenDay *usageWindow `json:"sevenDay,omitempty"`
	Pace     *float64     `json:"pace,omitempty"`
	High     bool         `json:"high,omitempty"`
	Blocks   []string     `json:"blocks,omitempty"`
}

func (b *bucketView) windowsLine(now time.Time) string {
	return windowsPairLine(b.FiveHour, b.SevenDay, now)
}

// providerView is one provider's busy agents, usage windows, derived target
// and capacity verdict.
type providerView struct {
	Name        string                 `json:"name"`
	Busy        []string               `json:"busy"`
	Floor       int                    `json:"floor"`
	Ceiling     int                    `json:"ceiling"`
	Room        int                    `json:"room"`
	At          time.Time              `json:"at,omitzero"`
	FiveHour    *usageWindow           `json:"fiveHour,omitempty"`
	SevenDay    *usageWindow           `json:"sevenDay,omitempty"`
	Pace        *float64               `json:"pace,omitempty"`
	Buckets     map[string]*bucketView `json:"buckets,omitempty"`
	bucketOrder []string
	High        bool     `json:"high,omitempty"`
	HighPercent float64  `json:"highPercent,omitempty"`
	Stale       bool     `json:"stale,omitempty"`
	Error       string   `json:"error,omitempty"`
	Blocks      []string `json:"blocks,omitempty"`
	Verdict     string   `json:"verdict"`
	// Kind is "self-hosted" for a provider with no subscription behind it: no
	// usage reader, no pace, a ceiling of sessions at once of its own.
	Kind             string `json:"kind,omitempty"`
	ServingMode      string `json:"servingMode,omitempty"`
	ServingModeError string `json:"servingModeError,omitempty"`
}

func (p *providerView) findBucket(name string) *bucketView {
	if len(p.Buckets) == 0 {
		return nil
	}
	if b, ok := p.Buckets[name]; ok {
		return b
	}
	for k, b := range p.Buckets {
		if strings.EqualFold(k, name) {
			return b
		}
	}
	return nil
}

func (p *providerView) windowsSummary(now time.Time) string {
	if p.Kind == config.KindSelfHosted {
		if p.ServingModeError != "" {
			return "serving mode unknown (" + truncate(p.ServingModeError, 120) + ")"
		}
		if p.ServingMode != "" {
			return "serving mode " + p.ServingMode
		}
		return ""
	}
	if p.Error != "" {
		return "usage unknown (" + truncate(p.Error, 120) + ")"
	}
	if len(p.Buckets) > 0 {
		var parts []string
		for _, bName := range p.bucketOrder {
			if bv := p.Buckets[bName]; bv != nil {
				parts = append(parts, bName+" "+bv.windowsLine(now))
			}
		}
		return strings.Join(parts, "; ")
	}
	if p.FiveHour != nil || p.SevenDay != nil {
		return "usage " + windowsPairLine(p.FiveHour, p.SevenDay, now)
	}
	return ""
}

func (p *providerView) line(now time.Time) string {
	if p.Kind == config.KindSelfHosted {
		// A self-hosted provider shows the sessions it runs against its own
		// ceiling; it has no usage windows, only the serving mode its ceiling
		// may follow.
		s := fmt.Sprintf("%d busy of ceiling %d", len(p.Busy), p.Ceiling)
		if len(p.Busy) > 0 {
			s += ": " + strings.Join(p.Busy, ", ")
		}
		if ws := p.windowsSummary(now); ws != "" {
			s += "; " + ws
		}
		return s + "; " + p.Verdict
	}
	s := fmt.Sprintf("%d busy of floor %d, ceiling %d", len(p.Busy), p.Floor, p.Ceiling)
	if len(p.Busy) > 0 {
		s += ": " + strings.Join(p.Busy, ", ")
	}
	if ws := p.windowsSummary(now); ws != "" {
		s += "; " + ws
	}
	return s + "; " + p.Verdict
}

func (p *providerView) watchSummary(now time.Time) string {
	if p.Kind == config.KindSelfHosted {
		s := fmt.Sprintf("%d busy of ceiling %d", len(p.Busy), p.Ceiling)
		if ws := p.windowsSummary(now); ws != "" {
			s += ", " + ws
		}
		return s + ", " + p.Verdict
	}
	s := fmt.Sprintf("%d busy of floor %d, ceiling %d", len(p.Busy), p.Floor, p.Ceiling)
	if ws := p.windowsSummary(now); ws != "" {
		s += ", " + ws
	}
	s += ", " + p.Verdict
	if p.High {
		s += " (USAGE HIGH)"
	}
	return s
}

func providerVerdict(room int, blocks []string) string {
	switch {
	case room == 1:
		return "room for 1 start"
	case room > 1:
		return fmt.Sprintf("room for %d starts", room)
	case len(blocks) > 0:
		return "no start: " + strings.Join(blocks, "; ")
	default:
		return "no start: ceiling 0"
	}
}

// choiceCandidate is one evaluated entry of capacity.choices.
type choiceCandidate struct {
	Harness  string `json:"harness"`
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Bucket   string `json:"bucket,omitempty"`
	OK       bool   `json:"ok"`
	Reason   string `json:"reason,omitempty"`
}

// choiceView is the result of evaluating capacity.choices for a start.
type choiceView struct {
	Harness    string            `json:"harness,omitempty"`
	Model      string            `json:"model,omitempty"`
	Provider   string            `json:"provider,omitempty"`
	Bucket     string            `json:"bucket,omitempty"`
	Review     bool              `json:"review,omitempty"`
	Exclude    []string          `json:"exclude,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	Candidates []choiceCandidate `json:"candidates,omitempty"`
}

const (
	harnessAuto = "auto"
	harnessAgy  = "agy"
)

func (c *choiceView) line() string {
	if c == nil {
		return noProblems
	}
	if c.Harness != "" && c.Model != "" {
		s := fmt.Sprintf("%s / %s (provider %s", c.Harness, c.Model, c.Provider)
		if c.Bucket != "" {
			s += ", bucket " + c.Bucket
		}
		if c.Review {
			s += ", review"
		}
		return s + ")"
	}
	if c.Reason != "" {
		return noProblems + " (" + c.Reason + ")"
	}
	return noProblems
}

func (c *choiceView) summary() string {
	if c == nil {
		return noProblems
	}
	if c.Harness != "" && c.Model != "" {
		return c.Harness + "/" + c.Model
	}
	if c.Reason != "" {
		return noProblems + " (" + c.Reason + ")"
	}
	return noProblems
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
		if ag.Execution != nil {
			if executionReserved(st, ag) {
				c.Busy = append(c.Busy, ag.Name)
			} else {
				c.Idle = append(c.Idle, ag.Name)
			}
			continue
		}
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

type providerAgents struct {
	Unconfirmed []string
	Busy        []string
	BucketBusy  map[string][]string
}

func countProviderAgents(st *state.State, sessions []*claude.Session, k config.Capacity, now time.Time) map[string]providerAgents {
	out := make(map[string]providerAgents, len(k.Providers))
	for _, name := range k.ProviderNames() {
		out[name] = providerAgents{Busy: []string{}, BucketBusy: map[string][]string{}}
	}
	sup, guide := st.SupervisorRole(), st.GuideRole()
	for _, ag := range st.Agents {
		if ag.Execution != nil {
			if !executionReserved(st, ag) {
				continue
			}
			r, _ := executionRecord(st, *ag.Execution)
			configured := false
			if r != nil {
				_, configured = out[r.Provider]
			}
			if r == nil || !configured {
				// An unresolved reservation cannot become a fresh provider
				// budget after a damaged record or configuration change.
				for name, pa := range out {
					pa.Busy = append(pa.Busy, ag.Name)
					pa.Unconfirmed = append(pa.Unconfirmed, ag.Name)
					out[name] = pa
				}
				continue
			}
			pa := out[r.Provider]
			if pa.BucketBusy == nil {
				pa.BucketBusy = map[string][]string{}
			}
			pa.Busy = append(pa.Busy, ag.Name)
			if r.Bucket != "" {
				pa.BucketBusy[r.Bucket] = append(pa.BucketBusy[r.Bucket], ag.Name)
			}
			out[r.Provider] = pa
			continue
		}
		p := ag.Party
		if roleRun(st, p) || sup.Relay.Open(now) && sup.Relay.To.Is(p) || guide.Relay.Open(now) && guide.Relay.To.Is(p) {
			continue
		}
		if ag.Task == "" || ag.Done || keptBy(st, ag, now) != "" {
			continue
		}
		s, live := claude.Live(sessions, p)
		if live && (s.Waiting != nil || s.OwnWait(now, waitQuiet) != "") {
			continue
		}
		harness, model := agentHarnessModel(st, sessions, ag)
		prov, bucket := resolveAgentProviderBucket(k, harness, model)
		pa := out[prov]
		if pa.BucketBusy == nil {
			pa.BucketBusy = map[string][]string{}
		}
		pa.Busy = append(pa.Busy, ag.Name)
		if bucket != "" {
			pa.BucketBusy[bucket] = append(pa.BucketBusy[bucket], ag.Name)
		}
		out[prov] = pa
	}
	return out
}

func agentHarnessModel(st *state.State, sessions []*claude.Session, ag state.Agent) (harness, model string) {
	harness, model = strings.TrimSpace(ag.Harness), strings.TrimSpace(ag.Model)
	for i := len(st.Starts) - 1; i >= 0; i-- {
		s := st.Starts[i]
		if s.Is(ag.Party) || (s.Name != "" && s.Name == ag.Name) {
			if harness == "" {
				harness = strings.TrimSpace(s.Harness)
			}
			if model == "" {
				model = strings.TrimSpace(s.Model)
			}
			break
		}
	}
	if sess, live := claude.Live(sessions, ag.Party); live {
		if harness == "" {
			harness = strings.TrimSpace(sess.Harness)
		}
		if model == "" {
			model = strings.TrimSpace(sess.Model)
		}
	}
	if harness == "" {
		switch {
		case strings.HasPrefix(ag.HostSession, "omp_") || strings.HasPrefix(ag.Session, "omp_"):
			harness = omp.Harness
		case strings.HasPrefix(ag.HostSession, "agy_") || strings.HasPrefix(ag.Session, "agy_"):
			harness = harnessAgy
		case strings.HasPrefix(ag.HostSession, codex.HostPrefix) || strings.HasPrefix(ag.Session, codex.HostPrefix):
			harness = codex.Harness
		default:
			harness = claudeComm
		}
	}
	return harness, model
}

func resolveAgentProviderBucket(k config.Capacity, harness, model string) (prov, bucket string) {
	h := cmp.Or(strings.TrimSpace(harness), claudeComm)
	m := strings.TrimSpace(model)
	for _, ch := range k.Choices {
		if !strings.EqualFold(ch.Harness, h) {
			continue
		}
		if m != "" && (strings.EqualFold(ch.Model, m) || strings.HasPrefix(strings.ToLower(m), strings.ToLower(ch.Model))) {
			p := ch.EffectiveProvider()
			return p, ch.EffectiveBucket(k.Providers[p])
		}
	}
	for _, ch := range k.Choices {
		if strings.EqualFold(ch.Harness, h) && m == "" {
			p := ch.EffectiveProvider()
			return p, ch.EffectiveBucket(k.Providers[p])
		}
	}
	// An omp model is <provider>/<id>: its provider is the one it names, when
	// the desk configures it. Else the first self-hosted provider whose model
	// prefix the selector carries (omp's default provider, unset, "local");
	// with no prefix match, the only self-hosted provider, if there is
	// exactly one: an omp model selector names a models.yml provider, which
	// need not carry the capacity.providers key's name.
	if h == omp.Harness {
		if p := ompProvider(m); p != "" {
			if pp, ok := k.Providers[p]; ok && pp.SelfHosted() {
				return p, pp.BucketFor(m)
			}
		}
		var selfHosted []string
		for _, name := range k.ProviderNames() {
			if k.Providers[name].SelfHosted() {
				selfHosted = append(selfHosted, name)
			}
		}
		for _, name := range selfHosted {
			if m != "" && strings.HasPrefix(strings.ToLower(m), strings.ToLower(name+"/")) {
				return name, k.Providers[name].BucketFor(m)
			}
		}
		if len(selfHosted) == 1 {
			return selfHosted[0], k.Providers[selfHosted[0]].BucketFor(m)
		}
		return h, ""
	}
	if pp, ok := k.Providers[h]; ok && !pp.SelfHosted() {
		return h, pp.BucketFor(m)
	}
	// A subscription harness never resolves to a self-hosted provider: its
	// room would let a subscription start pass a capacity.ceiling of 0, the
	// very hold that pauses it. With no subscription provider in the pool,
	// the harness's own name is the provider, and no view bounds it.
	if names := k.ProviderNames(); len(names) > 0 {
		for _, name := range names {
			if !k.Providers[name].SelfHosted() {
				return name, k.Providers[name].BucketFor(m)
			}
		}
		return h, ""
	}
	return h, ""
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

type providerReadingResult struct {
	reading usageReading
	err     error
	ok      bool
	// serveMode is a self-hosted provider's serving-mode read, and
	// serveModeErr why that read failed.
	serveMode    string
	serveModeErr string
}

func buildSingleProviderView(name string, pCfg config.Provider, pa providerAgents, res providerReadingResult, now time.Time) *providerView {
	if pCfg.SelfHosted() {
		return buildSelfHostedProviderView(name, pCfg, pa, res, now)
	}
	busy := pa.Busy
	if busy == nil {
		busy = []string{}
	}
	pv := &providerView{Name: name, Busy: busy}
	if len(pa.Unconfirmed) > 0 {
		pv.Error = "remote execution reservation is unresolved; inspect its registration and provider"
		pv.Blocks = []string{pv.Error}
		pv.Verdict = providerVerdict(0, pv.Blocks)
		return pv
	}
	if pCfg.Enabled() {
		pv.HighPercent = pCfg.EffectiveHighPercent()
		if !res.ok || res.err != nil {
			errMsg := "usage not read"
			if res.err != nil {
				errMsg = res.err.Error()
			}
			pv.Floor, pv.Ceiling, pv.Room = 0, 0, 0
			pv.Error = errMsg
			pv.Stale = strings.Contains(errMsg, "stale")
			pv.Blocks = []string{"subscription usage unknown (" + errMsg + ")"}
			pv.Verdict = providerVerdict(0, pv.Blocks)
			return pv
		}
		r := res.reading
		pv.At = r.At
		if len(r.Buckets) > 0 {
			pv.Buckets = make(map[string]*bucketView, len(r.Buckets))
			pv.bucketOrder = bucketNames(r)
			maxBucketCeiling := 0
			anyBucketOpen := false
			allHigh := true
			var firstHighFH *usageWindow
			var bucketBlocks []string
			for _, bName := range pv.bucketOrder {
				b := r.Buckets[bName]
				fh := effectiveWindow(b.FiveHour, now)
				sd := effectiveSevenDayWindow(b.SevenDay, now)
				dp, high := pCfg.DeriveWithPace(pctPtr(fh), pctPtr(sd), sdPace(sd))
				if high && firstHighFH == nil {
					firstHighFH = fh
				}
				bBusy := pa.BucketBusy[bName]
				if bBusy == nil {
					bBusy = []string{}
				}
				var bBlocks []string
				switch {
				case high:
					bBlocks = append(bBlocks, highBlockText(fh, pv.HighPercent, now))
				case pCfg.FiveHourMax > 0 && fh != nil && fh.UsedPercent >= pCfg.FiveHourMax:
					bBlocks = append(bBlocks, fmt.Sprintf("5h %s at or past %s", pct(fh.UsedPercent), pct(pCfg.FiveHourMax)))
				case pCfg.SevenDayMax > 0 && sd != nil && sd.UsedPercent >= pCfg.SevenDayMax:
					bBlocks = append(bBlocks, fmt.Sprintf("7d %s at or past %s", pct(sd.UsedPercent), pct(pCfg.SevenDayMax)))
				case dp.Ceiling == 0:
					bBlocks = append(bBlocks, ceilingZero)
				case len(bBusy) >= dp.Ceiling:
					bBlocks = append(bBlocks, fmt.Sprintf("%d busy at the ceiling of %d", len(bBusy), dp.Ceiling))
				}
				bRoom := 0
				if len(bBlocks) == 0 {
					bRoom = max(0, dp.Ceiling-len(bBusy))
					anyBucketOpen = true
				} else {
					bucketBlocks = append(bucketBlocks, bName+": "+bBlocks[0])
				}
				if !high {
					allHigh = false
				}
				maxBucketCeiling = max(maxBucketCeiling, dp.Ceiling)
				pv.Buckets[bName] = &bucketView{
					Name:     bName,
					Busy:     bBusy,
					Floor:    dp.Floor,
					Ceiling:  dp.Ceiling,
					Room:     bRoom,
					FiveHour: fh,
					SevenDay: sd,
					Pace:     sdPace(sd),
					High:     high,
					Blocks:   bBlocks,
				}
			}
			if maxBucketCeiling == 0 {
				pv.Floor, pv.Ceiling, pv.Room = 0, 0, 0
				pv.High = allHigh
				if allHigh {
					pv.FiveHour = firstHighFH
				}
				pv.Blocks = bucketBlocks
				pv.Verdict = providerVerdict(0, pv.Blocks)
				return pv
			}
			pv.Ceiling = min(pCfg.Ceiling, maxBucketCeiling)
			pv.Floor = min(pCfg.Floor, pv.Ceiling)
			switch {
			case pv.Ceiling == 0:
				pv.Blocks = []string{ceilingZero}
			case len(pv.Busy) >= pv.Ceiling:
				pv.Blocks = []string{fmt.Sprintf("%d busy at the ceiling of %d", len(pv.Busy), pv.Ceiling)}
			case !anyBucketOpen:
				pv.Blocks = bucketBlocks
			default:
				pv.Room = pv.Ceiling - len(pv.Busy)
				for _, bv := range pv.Buckets {
					bv.Room = min(bv.Room, pv.Room)
				}
			}
			pv.Verdict = providerVerdict(pv.Room, pv.Blocks)
			return pv
		}
		pv.FiveHour = effectiveWindow(r.FiveHour, now)
		pv.SevenDay = effectiveSevenDayWindow(r.SevenDay, now)
		pv.Pace = sdPace(pv.SevenDay)
		dp, high := pCfg.DeriveWithPace(pctPtr(pv.FiveHour), pctPtr(pv.SevenDay), pv.Pace)
		pv.Floor, pv.Ceiling, pv.High = dp.Floor, dp.Ceiling, high
	} else {
		pv.Floor, pv.Ceiling = pCfg.Floor, pCfg.Ceiling
	}
	switch {
	case pv.High:
		pv.Blocks = []string{highBlockText(pv.FiveHour, pv.HighPercent, now)}
	case pCfg.Enabled() && pCfg.FiveHourMax > 0 && pv.FiveHour != nil && pv.FiveHour.UsedPercent >= pCfg.FiveHourMax:
		pv.Blocks = []string{fmt.Sprintf("5h %s at or past %s", pct(pv.FiveHour.UsedPercent), pct(pCfg.FiveHourMax))}
	case pCfg.Enabled() && pCfg.SevenDayMax > 0 && pv.SevenDay != nil && pv.SevenDay.UsedPercent >= pCfg.SevenDayMax:
		pv.Blocks = []string{fmt.Sprintf("7d %s at or past %s", pct(pv.SevenDay.UsedPercent), pct(pCfg.SevenDayMax))}
	case pv.Ceiling == 0:
		pv.Blocks = []string{ceilingZero}
	case len(pv.Busy) >= pv.Ceiling:
		pv.Blocks = []string{fmt.Sprintf("%d busy at the ceiling of %d", len(pv.Busy), pv.Ceiling)}
	default:
		pv.Room = pv.Ceiling - len(pv.Busy)
	}
	pv.Verdict = providerVerdict(pv.Room, pv.Blocks)
	return pv
}

// buildSelfHostedProviderView fills the view of a self-hosted provider: a
// model served on the desk's own machines, which has no subscription behind
// it and so no usage reader, no pace and no USAGE HIGH. Its ceiling of busy
// sessions is its own — the one its serving mode names, when it has one, else
// the configured ceiling — and only its busy agents count against it. The
// machine guards and, for the subscription providers, capacity.ceiling still
// cap what a start needs.
func buildSelfHostedProviderView(name string, pCfg config.Provider, pa providerAgents, res providerReadingResult, now time.Time) *providerView {
	busy := pa.Busy
	if busy == nil {
		busy = []string{}
	}
	pv := &providerView{Name: name, Busy: busy, Kind: config.KindSelfHosted}
	if len(pa.Unconfirmed) > 0 {
		pv.Error = "remote execution reservation is unresolved; inspect its registration and provider"
		pv.Blocks = []string{pv.Error}
		pv.Verdict = providerVerdict(0, pv.Blocks)
		return pv
	}
	// The mode's ceiling, applied here rather than on the shared config: no
	// caller's capacity map changes.
	eff := pCfg.WithServeMode(config.ServeModeReading{Mode: res.serveMode, Err: res.serveModeErr})
	pv.Ceiling = eff.Ceiling
	pv.Floor = eff.Floor
	pv.ServingMode = res.serveMode
	if pCfg.HasServeMode() {
		if res.serveModeErr != "" {
			// The mode could not be read: the provider keeps its configured
			// ceiling, and the line says the mode is unknown.
			pv.ServingModeError = res.serveModeErr
		} else if pv.ServingMode == "" {
			pv.ServingModeError = "serving mode not read"
		}
	}
	switch {
	case pv.Ceiling == 0:
		pv.Blocks = []string{ceilingZero}
	case len(pv.Busy) >= pv.Ceiling:
		pv.Blocks = []string{fmt.Sprintf("%d busy at the ceiling of %d", len(pv.Busy), pv.Ceiling)}
	default:
		pv.Room = pv.Ceiling - len(pv.Busy)
	}
	pv.Verdict = providerVerdict(pv.Room, pv.Blocks)
	return pv
}

// buildProvidersCapacity folds the per-provider views, the machine guards
// and capacity.choices into one verdict: each provider against its own
// target (capacity.ceiling bounds the subscription providers' total, never a
// self-hosted provider's), the headroom against a start, and the first
// choice whose provider has room.
func buildProvidersCapacity(c agentCount, pa map[string]providerAgents, k config.Capacity, readings map[string]providerReadingResult, h *headroom, builds bool, exclude []string, review bool, now time.Time) *capacityView {
	providers := make(map[string]*providerView, len(k.Providers))
	sumFloor, sumCeiling := 0, 0
	subFloor, subCeiling := 0, 0
	anySubscription := false
	anySelfHosted := false
	for _, name := range k.ProviderNames() {
		pCfg := k.Providers[name]
		pv := buildSingleProviderView(name, pCfg, pa[name], readings[name], now)
		providers[name] = pv
		sumFloor += pv.Floor
		sumCeiling += pv.Ceiling
		// capacity.ceiling caps the subscription providers, not a self-hosted
		// one: a desk that lowers capacity.ceiling to 0 to keep a subscription
		// on its weekly pace still starts on its own model.
		if pCfg.SelfHosted() {
			anySelfHosted = true
		} else {
			anySubscription = true
			subFloor += pv.Floor
			subCeiling += pv.Ceiling
		}
	}
	effCeiling := sumCeiling
	// The total capacity.ceiling caps is the subscription providers': a desk
	// lowers it to 0 to keep a subscription on its weekly pace, and a
	// self-hosted provider's ceiling stands untouched beside it.
	if anySubscription {
		effCeiling = min(k.Ceiling, subCeiling) + (sumCeiling - subCeiling)
	}
	effFloor := min(k.Floor, min(sumFloor, effCeiling))
	mBlocks := h.blocks(builds)
	// selfHostedRoom is the room the self-hosted providers still have: starts
	// they take belong to no subscription, so capacity.ceiling — which a desk
	// lowers to 0 to hold a subscription on its weekly pace — never refuses
	// one of them.
	selfHostedRoom := 0
	for _, name := range k.ProviderNames() {
		if k.Providers[name].SelfHosted() {
			selfHostedRoom += providers[name].Room
		}
	}
	// subscriptionBusy is the busy agents of the subscription providers;
	// machineBusy those weighing against the total. A self-hosted agent asks
	// nothing of a subscription, so capacity.ceiling — which a desk lowers to
	// 0 to hold one on its weekly pace — bounds the subscription side alone
	// and never counts them.
	subscriptionBusy := 0
	for _, name := range k.ProviderNames() {
		if !k.Providers[name].SelfHosted() {
			subscriptionBusy += len(providers[name].Busy)
		}
	}
	// The subscription side stands alone under capacity.ceiling; a self-hosted
	// agent asks nothing of it and never weighs against it.
	machineBusy := subscriptionBusy
	subRoomCap := max(0, min(k.Ceiling, subCeiling)-subscriptionBusy)
	if !anySubscription {
		subRoomCap = max(0, effCeiling-len(c.Busy))
	}
	// A self-hosted start belongs to no subscription, so capacity.ceiling caps
	// none of it: the total it leaves is its own room, not what the
	// subscription side has left.
	machineRoomCap := subRoomCap + selfHostedRoom
	// sumRoom is the room after the caps below, and subHadRoom says a
	// subscription provider had room of its own that the total (capacity.
	// ceiling, or the machine) took away — the one case where the verdict
	// names the total rather than each provider's own block.
	sumRoom := 0
	subHadRoom := false
	for _, name := range k.ProviderNames() {
		pCfg := k.Providers[name]
		pv := providers[name]
		// left is what the total still leaves this provider: a self-hosted
		// provider is bounded by its own ceiling alone, never by the total
		// capacity.ceiling holds the subscription side to.
		selfHosted := pCfg.SelfHosted()
		left := subRoomCap
		if selfHosted {
			left = pv.Room
		}
		if !selfHosted && pv.Room > 0 {
			subHadRoom = true
		}
		if len(mBlocks) > 0 {
			pv.Room = 0
			pv.Blocks = append(pv.Blocks, mBlocks...)
		} else if pv.Room > left {
			pv.Room = left
			if pv.Room == 0 && len(pv.Blocks) == 0 {
				switch {
				case k.Ceiling == 0 && !selfHosted:
					pv.Blocks = []string{ceilingZero}
				case selfHosted:
					pv.Blocks = []string{fmt.Sprintf("%d busy at the ceiling of %d", len(pv.Busy), pv.Ceiling)}
				default:
					pv.Blocks = []string{fmt.Sprintf("%d busy at the ceiling of %d", subscriptionBusy, min(k.Ceiling, subCeiling))}
				}
			}
		}
		for _, bv := range pv.Buckets {
			bv.Room = min(bv.Room, pv.Room)
		}
		pv.Verdict = providerVerdict(pv.Room, pv.Blocks)
		sumRoom += pv.Room
	}

	v := &capacityView{
		agentCount:     c,
		Floor:          effFloor,
		Ceiling:        effCeiling,
		Providers:      providers,
		Headroom:       h,
		Builds:         builds,
		subRoomCap:     subRoomCap,
		selfHostedRoom: selfHostedRoom,
	}
	if line, blocks := h.swapGuard(); line != "" && !blocks {
		v.Notes = append(v.Notes, line)
	}
	if !builds && h.slotBusy() {
		v.Notes = append(v.Notes, noFreeSlot+": a start that builds (--builds) waits for it")
	}

	if len(k.Choices) > 0 {
		v.Choice = evaluateChoices(k, providers, mBlocks, "", exclude, review, now)
	}

	switch {
	case len(mBlocks) > 0:
		v.Room = 0
		switch {
		case k.Ceiling == 0 && selfHostedRoom == 0 && anySubscription && !anySelfHosted:
			v.Blocks = append([]string{ceilingZero}, mBlocks...)
		case machineBusy >= effCeiling && effCeiling > 0:
			v.Blocks = append([]string{totalCeilingBlock(k, subscriptionBusy, subCeiling, effCeiling, anySubscription)}, mBlocks...)
		default:
			v.Blocks = slices.Clone(mBlocks)
		}
	case len(k.Choices) > 0 && v.Choice != nil:
		if v.Choice.Harness == "" {
			v.Room = 0
			v.Blocks = []string{"no choice has room: " + v.Choice.Reason}
		} else {
			choiceRoom := eligibleChoicesRoom(k, providers, v.Choice.Candidates, subRoomCap)
			v.Room = min(machineRoomCap, choiceRoom)
			if v.Room == 0 {
				v.Blocks = []string{totalCeilingBlock(k, subscriptionBusy, subCeiling, effCeiling, anySubscription)}
			}
		}
	default:
		v.Room = min(machineRoomCap, sumRoom)
		if v.Room == 0 {
			// The total capacity.ceiling holds the subscription side to
			// refuses a start only when it took room a subscription provider
			// had; with no self-hosted provider in the pool the total names
			// the verdict. Otherwise every provider keeps its own block (a
			// failed usage read, USAGE HIGH, its own ceiling), and the verdict
			// names each of them.
			if !anySelfHosted && k.Ceiling > 0 && anySubscription && subHadRoom && subRoomCap == 0 {
				v.Blocks = []string{totalCeilingBlock(k, subscriptionBusy, subCeiling, effCeiling, anySubscription)}
			} else if !anySelfHosted && k.Ceiling == 0 && anySubscription && subHadRoom {
				v.Blocks = []string{ceilingZero}
			} else {
				for _, name := range k.ProviderNames() {
					if pv := providers[name]; len(pv.Blocks) > 0 {
						v.Blocks = append(v.Blocks, name+": "+strings.Join(pv.Blocks, "; "))
					}
				}
			}
		}
	}
	return v
}

// totalCeilingBlock is the block naming the total a start hit. With a
// subscription provider in the pool that total is the one capacity.ceiling
// holds the subscription side to; alone, the providers' own ceilings are the
// total, and capacity.ceiling caps none of them.
func totalCeilingBlock(k config.Capacity, subscriptionBusy, subCeiling, effCeiling int, anySubscription bool) string {
	busy, ceiling := subscriptionBusy, effCeiling
	if anySubscription {
		ceiling = min(k.Ceiling, subCeiling)
		if ceiling == 0 {
			// capacity.ceiling is 0: the subscription side is held at the
			// ceiling itself, not at a derived total of busy agents.
			return ceilingZero
		}
	}
	return fmt.Sprintf("%d busy at the ceiling of %d", busy, ceiling)
}

func eligibleChoicesRoom(k config.Capacity, providers map[string]*providerView, candidates []choiceCandidate, subRoomCap int) int {
	provRoom := map[string]int{}
	bucketSum := map[string]int{}
	seenBucket := map[string]bool{}
	for _, cand := range candidates {
		if !cand.OK {
			continue
		}
		pv := providers[cand.Provider]
		if pv == nil {
			continue
		}
		if cand.Bucket != "" && len(pv.Buckets) > 0 {
			if bv := pv.findBucket(cand.Bucket); bv != nil {
				key := cand.Provider + "/" + bv.Name
				if !seenBucket[key] {
					seenBucket[key] = true
					bucketSum[cand.Provider] += bv.Room
				}
				provRoom[cand.Provider] = min(pv.Room, bucketSum[cand.Provider])
				continue
			}
		}
		provRoom[cand.Provider] = pv.Room
	}
	// The subscription providers' room is shared: capacity.ceiling caps them
	// together, so the choices on them together have at most subRoomCap. A
	// self-hosted provider's room stands apart from that cap.
	subTotal, selfTotal := 0, 0
	for name, r := range provRoom {
		if providers[name].Kind == config.KindSelfHosted {
			selfTotal += r
		} else {
			subTotal += r
		}
	}
	return min(subTotal, subRoomCap) + selfTotal
}

func normalizeExcludes(exclude []string) []string {
	var out []string
	for _, raw := range exclude {
		raw = strings.Trim(strings.TrimSpace(raw), "[]")
		for _, part := range strings.Split(raw, ",") {
			if s := strings.Trim(strings.TrimSpace(part), "[]"); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func choiceExcluded(ch config.Choice, excludes, providerNames []string) bool {
	if len(excludes) == 0 {
		return false
	}
	h := strings.ToLower(strings.TrimSpace(ch.Harness))
	m := strings.ToLower(strings.TrimSpace(ch.Model))
	p := strings.ToLower(ch.EffectiveProvider())
	b := strings.ToLower(strings.TrimSpace(ch.Bucket))
	hm := h + "/" + m
	pm := p + "/" + m

	isKnownProviderOrHarness := func(tok string) bool {
		if tok == h || tok == p || tok == claudeComm || tok == harnessAgy || tok == omp.Harness || tok == codex.Harness {
			return true
		}
		return slices.ContainsFunc(providerNames, func(n string) bool { return strings.EqualFold(n, tok) })
	}

	for _, raw := range excludes {
		ex := strings.ToLower(strings.TrimSpace(raw))
		if ex == "" {
			continue
		}
		switch {
		case strings.Contains(ex, "/"):
			if ex == hm || ex == pm || strings.HasPrefix(hm, strings.TrimSuffix(ex, "*")) || strings.HasPrefix(pm, strings.TrimSuffix(ex, "*")) {
				return true
			}
		case ex == h || ex == p || ex == m || (b != "" && ex == b):
			return true
		case !isKnownProviderOrHarness(ex) && strings.HasPrefix(m, strings.TrimSuffix(ex, "*")):
			return true
		}
	}
	return false
}

func evaluateChoices(k config.Capacity, providers map[string]*providerView, machineBlocks []string, stopped string, exclude []string, review bool, now time.Time) *choiceView {
	normEx := normalizeExcludes(exclude)
	cv := &choiceView{
		Review:  review,
		Exclude: normEx,
	}
	skipPaceForReview := review && k.EffectiveReviewSkipPace()
	var reasons []string

	for _, ch := range k.Choices {
		provName := ch.EffectiveProvider()
		pCfg := k.Providers[provName]
		pv := providers[provName]
		bucketName := ""
		if pv != nil && len(pv.Buckets) > 0 {
			bucketName = ch.EffectiveBucket(pCfg)
		}
		cand := choiceCandidate{
			Harness:  ch.Harness,
			Model:    ch.Model,
			Provider: provName,
			Bucket:   bucketName,
		}
		reason := evaluateSingleChoice(ch, pCfg, pv, bucketName, machineBlocks, stopped, normEx, k.ProviderNames(), skipPaceForReview, now)
		if reason == "" {
			cand.OK = true
			if cv.Harness == "" {
				cv.Harness = ch.Harness
				cv.Model = ch.Model
				cv.Provider = provName
				cv.Bucket = bucketName
			}
		} else {
			cand.Reason = reason
			reasons = append(reasons, fmt.Sprintf("%s/%s: %s", ch.Harness, ch.Model, reason))
		}
		cv.Candidates = append(cv.Candidates, cand)
	}
	if cv.Harness == "" && len(reasons) > 0 {
		cv.Reason = strings.Join(reasons, "; ")
	}
	return cv
}

func evaluateSingleChoice(ch config.Choice, pCfg config.Provider, pv *providerView, bucketName string, machineBlocks []string, stopped string, excludes, providerNames []string, skipPaceForReview bool, now time.Time) string {
	if choiceExcluded(ch, excludes, providerNames) {
		return "excluded"
	}
	if stopped != "" {
		return stopped
	}
	if len(machineBlocks) > 0 {
		return strings.Join(machineBlocks, "; ")
	}
	if pv == nil {
		return fmt.Sprintf("provider %s not configured", ch.EffectiveProvider())
	}
	if pv.Error != "" {
		return fmt.Sprintf("provider %s usage unknown (%s)", pv.Name, pv.Error)
	}

	var fh, sd *usageWindow
	if len(pv.Buckets) > 0 {
		bv := pv.findBucket(bucketName)
		if bv == nil {
			return fmt.Sprintf("bucket %q not in %s usage output", bucketName, pv.Name)
		}
		fh, sd = bv.FiveHour, bv.SevenDay
		if bv.High {
			return highBlockText(fh, pv.HighPercent, now)
		}
		if max5h, ok := ch.EffectiveFiveHourMax(pCfg); ok && fh != nil && fh.UsedPercent >= max5h {
			return fmt.Sprintf("5h %s at or past %s", pct(fh.UsedPercent), pct(max5h))
		}
		if pCfg.SevenDayMax > 0 && sd != nil && sd.UsedPercent >= pCfg.SevenDayMax {
			return fmt.Sprintf("7d %s at or past %s", pct(sd.UsedPercent), pct(pCfg.SevenDayMax))
		}
		if bv.Room <= 0 {
			if len(bv.Blocks) > 0 {
				return strings.Join(bv.Blocks, "; ")
			}
			if len(pv.Blocks) > 0 {
				return strings.Join(pv.Blocks, "; ")
			}
			return fmt.Sprintf("no room on %s", pv.Name)
		}
	} else {
		fh, sd = pv.FiveHour, pv.SevenDay
		if pv.High {
			return highBlockText(fh, pv.HighPercent, now)
		}
		if max5h, ok := ch.EffectiveFiveHourMax(pCfg); ok && fh != nil && fh.UsedPercent >= max5h {
			return fmt.Sprintf("5h %s at or past %s", pct(fh.UsedPercent), pct(max5h))
		}
		if pCfg.SevenDayMax > 0 && sd != nil && sd.UsedPercent >= pCfg.SevenDayMax {
			return fmt.Sprintf("7d %s at or past %s", pct(sd.UsedPercent), pct(pCfg.SevenDayMax))
		}
		if pv.Room <= 0 {
			if len(pv.Blocks) > 0 {
				return strings.Join(pv.Blocks, "; ")
			}
			return fmt.Sprintf("no room on %s", pv.Name)
		}
	}

	if !skipPaceForReview && sd != nil {
		if max7d, ok := ch.EffectiveWorkerSevenDayMax(); ok && sd.UsedPercent >= max7d {
			return fmt.Sprintf("7d %s at or past worker cap %s", pct(sd.UsedPercent), pct(max7d))
		}
	}
	if !ch.SkipPace && !skipPaceForReview && sd != nil && sd.Pace != nil {
		margin := ch.EffectivePaceMargin(pCfg)
		over := math.Round((sd.UsedPercent-*sd.Pace)*10) / 10
		if over > margin+1e-9 {
			if margin == 0 {
				return fmt.Sprintf("7d %s ahead of pace %s (+%s)", pct(sd.UsedPercent), pct(*sd.Pace), pct(over))
			}
			return fmt.Sprintf("7d %s ahead of pace %s (+%s > %s margin)", pct(sd.UsedPercent), pct(*sd.Pace), pct(over), pct(margin))
		}
	}
	return ""
}

func (a *app) resolveProvidersCapacity(ctx context.Context, st *state.State, sessions []*claude.Session, k config.Capacity, builds bool, exclude []string, review bool) *capacityView {
	return a.resolveProvidersCapacityForModel(ctx, st, sessions, k, builds, exclude, review, "")
}

func (a *app) resolveProvidersCapacityForModel(ctx context.Context, st *state.State, sessions []*claude.Session, k config.Capacity, builds bool, exclude []string, review bool, wantModel string) *capacityView {
	now := a.now
	if now.IsZero() {
		now = time.Now()
	}
	c := countAgents(st, sessions, now)
	pa := countProviderAgents(st, sessions, k, now)
	readings := make(map[string]providerReadingResult, len(k.Providers))
	for _, name := range k.ProviderNames() {
		p := k.Providers[name]
		if !p.Enabled() {
			continue
		}
		r, err := a.resolveReading(ctx, usageProviderFile(name), p.Usage)
		readings[name] = providerReadingResult{reading: r, err: err, ok: err == nil}
	}
	for _, name := range k.ProviderNames() {
		p := k.Providers[name]
		if !p.SelfHosted() {
			continue
		}
		// A self-hosted provider reads no subscription usage; its ceiling may
		// follow the serving mode of the model it serves.
		r := a.serveMode(ctx, name, p)
		readings[name] = providerReadingResult{serveMode: r.Mode, serveModeErr: r.Err}
	}
	h := a.readHeadroom(ctx, k, a.watchSwap())
	return buildProvidersCapacity(c, pa, filterChoiceModel(k, wantModel), readings, h, builds, exclude, review, now)
}

// withUsage attaches u to v and replaces or prepends the usage block when
// subscription usage is unreadable or USAGE HIGH holds.
func (v *capacityView) withUsage(u *usageView, now time.Time) *capacityView {
	if u == nil {
		return v
	}
	v.Usage = u
	switch {
	case u.Error != "":
		block := "subscription usage unknown (" + u.Error + ")"
		if len(v.Blocks) > 0 && v.Blocks[0] == ceilingZero {
			v.Blocks[0] = block
		} else {
			v.Blocks = append([]string{block}, v.Blocks...)
		}
		v.Room = 0
	case u.High:
		block := u.highBlock(now)
		if len(v.Blocks) > 0 && v.Blocks[0] == ceilingZero {
			v.Blocks[0] = block
		} else {
			v.Blocks = append([]string{block}, v.Blocks...)
		}
		v.Room = 0
	}
	return v
}

// stopped makes the verdict "no start" while the stop s stands.
func (v *capacityView) stopped(a *app, s *state.Stop) *capacityView {
	if s != nil {
		v.Stopped = a.stopText(s)
		v.Blocks, v.Room = append([]string{v.Stopped}, v.Blocks...), 0
		for _, pv := range v.Providers {
			pv.Room = 0
			pv.Blocks = append([]string{v.Stopped}, pv.Blocks...)
			pv.Verdict = providerVerdict(0, pv.Blocks)
		}
		if v.Choice != nil && v.Choice.Harness != "" {
			v.Choice.Harness, v.Choice.Model, v.Choice.Provider, v.Choice.Bucket = "", "", "", ""
			v.Choice.Reason = v.Stopped
		}
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
	if a.store == nil {
		return nil
	}
	var r swapReading
	if found, err := a.store.ReadFile(swapFile, &r); err != nil || !found || a.now.Sub(r.At) > swapFresh {
		return nil
	}
	return &r
}

func (a *app) capacityCmd() *cobra.Command {
	var host string
	var builds, review bool
	var exclude []string
	c := &cobra.Command{
		Use:   "capacity [--host <host>] [--builds] [--review] [--exclude <choice>]",
		Short: "Busy agents against the target, the headroom for a start and the verdict",
		Long: `Count the roster's agents against the supervisor's target (capacity.floor,
capacity.ceiling; unset, 5 and 10) and say whether a new start fits. A
configured capacity.ceiling of 0 stops new starts ("no start: ceiling 0") and
the subscription providers', but not a self-hosted provider's, and suppresses
CAPACITY LOW and CAPACITY FULL in the watch; capacity.floor of 0 suppresses
CAPACITY LOW. When capacity.usage.command is configured,
beekeeper reads the subscription's 5-hour and 7-day usage windows (cached
for capacity.usage.every, default 5m, and re-read once resetsAt passes),
derives floor and ceiling from capacity.usage.fiveHour and
capacity.usage.sevenDay (taking the lower floor and ceiling when both have
bands), prints the windows on the capacity line and under usage in --json,
and refuses unforced starts with "no start: usage high: …" once the 5-hour
window reaches capacity.usage.highPercent (default 95%) until
fiveHour.resetsAt. When capacity.providers and capacity.choices are
configured, beekeeper reads each provider's usage windows (such as Claude's
JSON windows and agy -p /usage's model buckets), counts busy agents against
the provider and bucket they run on, caps the subscription providers' total by
the machine guards and capacity.ceiling, and prints each provider's windows and
verdict plus the first non-excluded choice (--exclude) in capacity.choices whose
provider has room, its 5-hour window below its threshold and its 7-day use
within the weekly cap and pace margin (--review skips the weekly pace check
and worker weekly cap when capacity.reviewSkipPace is true). A provider of
kind self-hosted (a model served on the desk's own machines, such as an omp
agent on a local model) reads no subscription usage and has no pace: its
ceiling of busy sessions is its own, optionally the one its serving mode names
(serveModeCommand and serveModeCeilings), and capacity.ceiling bounds none of
it — a desk that lowers capacity.ceiling to 0 to keep a subscription on its
weekly pace still starts on its own model, with the machine guards as the only
further bound; its line shows the sessions it runs and that ceiling.
Busy is a roster agent
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
			liveCap := a.liveCapacity()
			var v *capacityView
			if host != "" && host != "local" {
				h, err := a.executionHost(host)
				if err != nil {
					return err
				}
				caps, err := a.probeExecution(cmd.Context(), h)
				if err != nil {
					return err
				}
				k := liveCap
				var legacy *usageView
				if len(k.Providers) == 0 {
					k, legacy = a.resolveUsage(cmd.Context(), k)
				}
				v = a.executionCapacity(st, sessions, k, h, caps.Resources, a.executionReadings(cmd.Context(), k), legacy, builds, exclude, review, harnessAuto, "")
			} else if len(liveCap.Providers) > 0 || len(liveCap.Choices) > 0 {
				v = a.resolveProvidersCapacity(cmd.Context(), st, sessions, liveCap, builds, exclude, review).stopped(a, st.Stop)
			} else {
				c := countAgents(st, sessions, a.now)
				k, u := a.resolveUsage(cmd.Context(), liveCap)
				v = newCapacity(c, k, a.readHeadroom(cmd.Context(), k, a.watchSwap()), builds).withUsage(u, a.now).stopped(a, st.Stop)
			}
			if a.json {
				return a.printJSON(struct {
					*capacityView
					Verdict string `json:"verdict"`
				}{v, v.verdict()})
			}
			p := func(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format+"\n", args...) }
			capLine := v.line(config.Capacity{Floor: v.Floor, Ceiling: v.Ceiling})
			if v.Usage != nil {
				capLine += "; " + v.Usage.capacitySuffix(a.now)
			}
			p("capacity: %s", capLine)
			for _, name := range liveCap.ProviderNames() {
				if pv := v.Providers[name]; pv != nil {
					p("provider %s: %s", name, pv.line(a.now))
				}
			}
			if v.Choice != nil {
				p("choice: %s", v.Choice.line())
			}
			if len(v.Parked) > 0 {
				p("parked: %s", strings.Join(v.Parked, ", "))
			}
			if len(v.Waiting) > 0 {
				p("waiting: %s", strings.Join(v.Waiting, ", "))
			}
			if len(v.Idle) > 0 {
				p("idle: %s", strings.Join(v.Idle, ", "))
			}
			p("%s", v.Headroom.line())
			p("%s", v.verdict())
			return nil
		},
	}
	c.Flags().BoolVar(&builds, buildsFlag, false, "the verdict for a start that builds, which waits for a free build slot")
	c.Flags().StringVar(&host, "host", "", "execution host selector; default local")
	c.Flags().BoolVar(&review, reviewFlag, false, "the verdict for a review start, which skips the weekly pace check and worker weekly cap")
	c.Flags().StringSliceVar(&exclude, excludeFlag, nil, "exclude a choice by model, harness/model, provider or harness (repeatable or comma-separated)")
	return c
}

// buildsFlag marks a start whose task builds: the build slot is one of its
// guards. reviewFlag marks a review start, excludeFlag excludes choices.
const (
	buildsFlag  = "builds"
	reviewFlag  = "review"
	excludeFlag = "exclude"
)

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

// startGateWithOptions reads the capacity verdict for a start that builds or not,
// as beekeeper capacity reads it, narrowed to the target harness/model when
// explicit, and judges it (queueStart).
func (a *app) startGateWithOptions(ctx context.Context, name, harness, model string, builds, force bool, exclude []string, review bool) (string, *capacityView, error) {
	effModel := a.effectiveStartModel(harness, model)
	readCap := func(b bool) (*capacityView, error) {
		if harness == harnessAuto {
			return a.startCapacityForAuto(ctx, b, exclude, review, model)
		}
		return a.startCapacityWithOptions(ctx, b, exclude, review)
	}
	v, err := readCap(builds)
	if err != nil {
		return "", nil, err
	}
	v = v.forTarget(a.liveCapacity(), harness, effModel)
	o, wait, err := a.startSlot()
	if err != nil {
		return "", nil, err
	}
	latest := v
	over, err := queueStart(v, builds, force, wait,
		func() (bool, int, error) { return guard.AwaitSlot(o, "agents start "+name) },
		func() (*capacityView, error) {
			next, err := readCap(false)
			if err == nil {
				next = next.forTarget(a.liveCapacity(), harness, effModel)
				latest = next
			}
			return next, err
		})
	return over, latest, err
}

func (a *app) effectiveStartModel(harness, model string) string {
	if m := strings.TrimSpace(model); m != "" {
		return m
	}
	switch strings.TrimSpace(harness) {
	case omp.Harness:
		return strings.TrimSpace(a.cfg.Omp.Model)
	case codex.Harness:
		return strings.TrimSpace(a.cfg.Codex.Model)
	case "", claudeComm:
		return strings.TrimSpace(a.cfg.Agents.Model)
	}
	return ""
}

// forTarget narrows v's Room and Blocks to the resolved provider and bucket
// when an explicit harness (not auto) is given.
func (v *capacityView) forTarget(k config.Capacity, harness, model string) *capacityView {
	if v == nil || v.Stopped != "" || harness == harnessAuto || len(v.Providers) == 0 {
		return v
	}
	prov, bucket := resolveAgentProviderBucket(k, harness, model)
	pv := v.Providers[prov]
	if pv == nil {
		// The target resolves to no configured provider: it belongs to no
		// self-hosted pool, so only the subscription side bounds it — never
		// the self-hosted room riding in v.Room.
		if v.subRoomCap < v.Room {
			v.Room = v.subRoomCap
			if v.Room == 0 {
				v.Blocks = []string{ceilingZero}
			}
		}
		return v
	}
	if bucket != "" && len(pv.Buckets) > 0 {
		if bv := pv.findBucket(bucket); bv != nil {
			if bv.Room == 0 {
				v.Room = 0
				switch {
				case len(bv.Blocks) > 0:
					v.Blocks = []string{fmt.Sprintf("provider %s (%s): %s", prov, bv.Name, strings.Join(bv.Blocks, "; "))}
				case len(pv.Blocks) == 1 && pv.Blocks[0] == noFreeSlot:
					v.Blocks = []string{noFreeSlot}
				case len(pv.Blocks) > 0:
					v.Blocks = []string{fmt.Sprintf("provider %s: %s", prov, strings.Join(pv.Blocks, "; "))}
				default:
					v.Blocks = []string{fmt.Sprintf("provider %s (%s): %s", prov, bv.Name, ceilingZero)}
				}
				return v
			}
			if v.Room == 0 {
				v.Room = bv.Room
				v.Blocks = nil
			} else {
				v.Room = min(v.Room, bv.Room)
			}
			return v
		}
	}
	if pv.Room == 0 {
		v.Room = 0
		switch {
		case len(pv.Blocks) == 1 && pv.Blocks[0] == noFreeSlot:
			v.Blocks = []string{noFreeSlot}
		case len(pv.Blocks) > 0:
			v.Blocks = []string{fmt.Sprintf("provider %s: %s", prov, strings.Join(pv.Blocks, "; "))}
		default:
			v.Blocks = []string{fmt.Sprintf("provider %s: %s", prov, ceilingZero)}
		}
		return v
	}
	if v.Room == 0 {
		v.Room = pv.Room
		v.Blocks = nil
	} else {
		v.Room = min(v.Room, pv.Room)
	}
	return v
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
	return a.startCapacityWithOptions(ctx, builds, nil, false)
}

func (a *app) startCapacityWithOptions(ctx context.Context, builds bool, exclude []string, review bool) (*capacityView, error) {
	return a.startCapacityForAuto(ctx, builds, exclude, review, "")
}

func (a *app) startCapacityForAuto(ctx context.Context, builds bool, exclude []string, review bool, wantModel string) (*capacityView, error) {
	st, err := a.store.Read()
	if err != nil {
		return nil, err
	}
	sessions, _, err := a.sessions()
	if err != nil && !platform.Missing(err) {
		return nil, err
	}
	liveCap := a.liveCapacity()
	if len(liveCap.Providers) > 0 || len(liveCap.Choices) > 0 {
		return a.resolveProvidersCapacityForModel(ctx, st, sessions, liveCap, builds, exclude, review, wantModel).stopped(a, st.Stop), nil
	}
	c := countAgents(st, sessions, a.now)
	k, u := a.resolveUsage(ctx, liveCap)
	return newCapacity(c, k, a.readHeadroom(ctx, k, a.watchSwap()), builds).withUsage(u, a.now).stopped(a, st.Stop), nil
}

func choicesHaveModel(choices []config.Choice, model string) bool {
	m := strings.TrimSpace(model)
	return slices.ContainsFunc(choices, func(ch config.Choice) bool {
		return strings.EqualFold(strings.TrimSpace(ch.Model), m)
	})
}

func filterChoiceModel(k config.Capacity, model string) config.Capacity {
	m := strings.TrimSpace(model)
	if m == "" || len(k.Choices) == 0 {
		return k
	}
	var filtered []config.Choice
	for _, ch := range k.Choices {
		if strings.EqualFold(strings.TrimSpace(ch.Model), m) {
			filtered = append(filtered, ch)
		}
	}
	k.Choices = filtered
	return k
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
	live := w.liveCapacity()
	k, u := w.currentUsage(live, w.now)
	c := countAgents(st, sessions, w.now)
	busy := len(c.Busy)
	// A configured capacity.ceiling of 0 keeps both lines silent even when
	// the derived ceiling is positive (a self-hosted provider's own ceiling
	// rides in the derived total).
	if (u != nil && u.Error != "") || k.Ceiling == 0 || live.Ceiling == 0 || (busy >= k.Floor && busy < k.Ceiling) || st.Stop != nil {
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
	v := newCapacity(c, k, h, false).withUsage(u, w.now)
	w.check("capacity-low", busy < k.Floor && v.Room > 0, "CAPACITY LOW %d of %d: %s; %s", busy, k.Floor, v.verdict(), v.Headroom.line())
	w.check("capacity-full", busy >= k.Ceiling, "CAPACITY FULL %d of %d: %s", busy, k.Ceiling, v.Headroom.line())
}
