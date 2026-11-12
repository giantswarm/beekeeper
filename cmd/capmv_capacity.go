package cmd

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/state"
)

// capacityView is what capacity shows: the roster's busy, parked and idle
// agents against the target, the headroom a new start needs and the verdict.
type capacityView struct {
	Hosts              []hostPlacementView          `json:"hosts,omitempty"`
	Activity           map[string]*providerActivity `json:"activity24h,omitempty"`
	WorkerCeiling      int                          `json:"workerCeiling"`
	ReviewHeadroom     int                          `json:"reviewHeadroom"`
	ReviewHeadroomUsed int                          `json:"reviewHeadroomUsed"`
	Review             bool                         `json:"review"`
	reviewLimitReached bool
	agentCount
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
	// have, which capacity.ceiling never bounds. unassignedBusy counts the
	// busy sessions of no provider of the pool: forTarget bounds a target
	// that resolves to no provider of its own by capacity.ceiling minus
	// those alone — never by the busy agents of a self-hosted provider,
	// which weigh on its own ceiling only.
	subRoomCap     int
	selfHostedRoom int
	unassignedBusy int
	// Notes is what a start that fits should know: disk swap growing
	// without memory pressure, the busy build slot a start without builds
	// does not wait for.
	Notes []string `json:"notes,omitempty"`
	// Stopped says the standing beekeeper stop or "starts" hold, which no
	// --force passes.
	Stopped string `json:"stopped,omitempty"`
	// Local is capacity.local.ceiling against the busy agents this machine
	// runs; nil while it is unset or the verdict is an execution host's.
	Local *localView `json:"local,omitempty"`
	// KindBlocks are the task kinds (agents.kinds) at a cap: a start of
	// one of their agents is refused whatever the verdict.
	KindBlocks []kindBlock `json:"kindBlocks,omitempty"`
}

// localView is the coordinator machine's own cap (capacity.local.ceiling):
// the busy agents whose sessions run here, a self-hosted provider's
// included, and the room the cap leaves a local start. An agent on an
// execution host counts against that host's maxBusy, never here.
// ReviewHeadroom is capacity.reviewHeadroom a review start may use above a
// positive local ceiling, as above the worker ceiling; zero for any other
// start.
type localView struct {
	Busy           []string `json:"busy"`
	Ceiling        int      `json:"ceiling"`
	ReviewHeadroom int      `json:"reviewHeadroom,omitempty"`
	Room           int      `json:"room"`
}

// line is the local cap's line in capacity's text.
func (l *localView) line() string {
	s := fmt.Sprintf("%d busy of ceiling %d", len(l.Busy), l.Ceiling)
	if l.ReviewHeadroom > 0 {
		s += fmt.Sprintf(" plus review headroom %d", l.ReviewHeadroom)
	}
	if len(l.Busy) > 0 {
		s += ": " + strings.Join(l.Busy, ", ")
	}
	return s
}

// block is what the local cap says when it refuses a start.
func (l *localView) block() string {
	if l.Ceiling == 0 {
		return "capacity.local.ceiling 0"
	}
	s := fmt.Sprintf("%d busy locally at capacity.local.ceiling %d", len(l.Busy), l.Ceiling)
	if l.ReviewHeadroom > 0 {
		s += fmt.Sprintf(" plus review headroom %d", l.ReviewHeadroom)
	}
	return s
}

// localBusy is the busy agents of busy whose sessions run on this machine:
// an agent with a managed execution runs on its host and is left out.
func localBusy(st *state.State, busy []string) []string {
	remote := map[string]bool{}
	for _, ag := range st.Agents {
		if ag.Execution != nil {
			remote[strings.ToLower(ag.Name)] = true
		}
	}
	out := []string{}
	for _, n := range busy {
		if !remote[strings.ToLower(n)] {
			out = append(out, n)
		}
	}
	return out
}

// withLocal bounds a local start's verdict by capacity.local.ceiling, when
// set: the busy agents running here against it, apart from capacity.ceiling
// and the hosts' own maxBusy. A review start may use capacity.reviewHeadroom
// above it as above the worker ceiling: reviews run only here, so a cap
// full of workers would otherwise hold back the review that lets them
// merge. A local ceiling of 0 stays closed (reviewCeiling).
func (v *capacityView) withLocal(st *state.State, k config.Capacity) *capacityView {
	n, ok := k.Local.Limit()
	if v == nil || !ok {
		return v
	}
	busy := localBusy(st, v.Busy)
	l := &localView{Busy: busy, Ceiling: n}
	limit := n
	if v.Review && n > 0 {
		l.ReviewHeadroom, limit = k.ReviewHeadroom, reviewCeiling(n, k.ReviewHeadroom)
	}
	l.Room = max(0, limit-len(busy))
	v.Local = l
	return v.capLocal()
}

// capLocal applies the local cap v carries to its room and blocks: room
// for no more starts than it leaves, and its block first when it leaves
// none. It holds again after forTarget narrows the room to one target.
func (v *capacityView) capLocal() *capacityView {
	if v == nil || v.Local == nil {
		return v
	}
	if v.Local.Room > 0 {
		v.Room = min(v.Room, v.Local.Room)
		return v
	}
	v.Room = 0
	if b := v.Local.block(); !slices.Contains(v.Blocks, b) {
		v.Blocks = append([]string{b}, v.Blocks...)
	}
	return v
}

// bucketView is one usage bucket's windows and derived target within a
// multi-bucket provider such as agy.
type bucketView struct {
	Name      string       `json:"name"`
	Busy      []string     `json:"busy"`
	Ceiling   int          `json:"ceiling"`
	Room      int          `json:"room"`
	FiveHour  *usageWindow `json:"fiveHour,omitempty"`
	SevenDay  *usageWindow `json:"sevenDay,omitempty"`
	Pace      *float64     `json:"pace,omitempty"`
	High      bool         `json:"high,omitempty"`
	Underused bool         `json:"underused,omitempty"`
	Blocks    []string     `json:"blocks,omitempty"`
}

func (b *bucketView) windowsLine(now time.Time) string {
	return windowsPairLine(b.FiveHour, b.SevenDay, now)
}

// providerView is one provider's busy agents, usage windows, derived target
// and capacity verdict.
type providerView struct {
	Fallback    string                 `json:"fallback,omitempty"`
	Name        string                 `json:"name"`
	Busy        []string               `json:"busy"`
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
	Underused   bool     `json:"underused,omitempty"`
	Stale       bool     `json:"stale,omitempty"`
	Error       string   `json:"error,omitempty"`
	Blocks      []string `json:"blocks,omitempty"`
	Verdict     string   `json:"verdict"`
	// Kind is "self-hosted" for a provider with no subscription behind it: no
	// usage reader, no pace, a ceiling of sessions at once of its own.
	Kind             string `json:"kind,omitempty"`
	ServingMode      string `json:"servingMode,omitempty"`
	ServingModeError string `json:"servingModeError,omitempty"`
	// DrainTo is the mode a serving-mode switch waits to run into: while
	// it stands, the provider's ceiling is that mode's and no session
	// starts on the model.
	DrainTo string `json:"drainTo,omitempty"`

	NoPlanWindows       bool `json:"noPlanWindows,omitempty"`
	supplementalBuckets bool
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

func (p *providerView) windowsSummary(now time.Time) (summary string) {
	defer func() {
		summary = withFallback(summary, p.Fallback, p.At, now)
	}()
	if p.Kind == config.KindSelfHosted {
		if p.ServingModeError != "" {
			return "serving mode unknown (" + truncate(p.ServingModeError, 120) + ")"
		}
		if p.ServingMode != "" {
			line := "serving mode " + p.ServingMode
			if p.DrainTo != "" {
				line += " (draining to " + p.DrainTo + ")"
			}
			return line
		}
		return ""
	}
	if p.NoPlanWindows {
		return claudeNoPlanWindows
	}
	if p.Error != "" {
		return "usage unknown (" + truncate(p.Error, 120) + ")"
	}
	if len(p.Buckets) > 0 {
		var parts []string
		if p.supplementalBuckets {
			parts = append(parts, "usage "+windowsPairLine(p.FiveHour, p.SevenDay, now))
		}
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
	s := fmt.Sprintf("%d busy of ceiling %d", len(p.Busy), p.Ceiling)
	if len(p.Busy) > 0 {
		s += ": " + strings.Join(p.Busy, ", ")
	}
	if ws := p.windowsSummary(now); ws != "" {
		s += "; " + ws
	}
	return s + "; " + p.Verdict
}

func (p *providerView) watchSummary(now time.Time) string {
	s := fmt.Sprintf("%d busy of ceiling %d", len(p.Busy), p.Ceiling)
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
	Effort     string   `json:"effort,omitempty"`
	Harness    string   `json:"harness"`
	Model      string   `json:"model"`
	Provider   string   `json:"provider"`
	Bucket     string   `json:"bucket,omitempty"`
	Class      int      `json:"class,omitempty"`
	Cost       float64  `json:"cost,omitempty"`
	Family     string   `json:"family,omitempty"`
	OK         bool     `json:"ok"`
	Reason     string   `json:"reason,omitempty"`
	BehindPace *float64 `json:"behindPace,omitempty"`
	Rate       *float64 `json:"rate,omitempty"`
}

// choiceView is the result of evaluating capacity.choices for a start.
type choiceView struct {
	Effort   string `json:"effort,omitempty"`
	Harness  string `json:"harness,omitempty"`
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
	Bucket   string `json:"bucket,omitempty"`
	TaskKind string `json:"taskKind,omitempty"`
	Decision string `json:"decision,omitempty"`
	Review   bool   `json:"review,omitempty"`
	// ReviewOfSelfHosted says the start reviews a self-hosted model's work:
	// no choice on a self-hosted provider is eligible.
	ReviewOfSelfHosted bool              `json:"reviewOfSelfHosted,omitempty"`
	Exclude            []string          `json:"exclude,omitempty"`
	Reason             string            `json:"reason,omitempty"`
	Candidates         []choiceCandidate `json:"candidates,omitempty"`
}

const (
	harnessAuto = "auto"
	harnessAgy  = "agy"
)

// startFor is the start a decision is for: a worker's, a review's, or the
// review of a pull request a self-hosted model's worker wrote. Worker starts
// keep the strict pace check; review starts inside the reserved review
// headroom are not refused by pace, and the last takes no choice on a
// self-hosted provider.
type startFor int

const (
	forWorker startFor = iota
	forReview
	forReviewOfSelfHosted
)

// startKind is the start for the review and review-of-self-hosted flags.
func startKind(review, ofSelfHosted bool) startFor {
	switch {
	case ofSelfHosted:
		return forReviewOfSelfHosted
	case review:
		return forReview
	}
	return forWorker
}

// selfHostedReviewer is a self-hosted choice's reason for the review of a
// self-hosted model's work.
const selfHostedReviewer = "the work of a self-hosted model gets a reviewer on a provider that is not self-hosted"

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
		return s + effortText(c.Effort) + ")"
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
	Gone    []string `json:"gone"`
}

type providerAgents struct {
	Unconfirmed []string
	Busy        []string
	BucketBusy  map[string][]string
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

// ompChoiceModel reports whether model, as an omp agent's start records it
// (the provider/id omp lists), is ch's omp model named by its bare id, as
// capacity.choices names it and capacity pick returns it. The models file's
// provider need not carry the capacity provider's name, so only the id
// after it is compared.
func ompChoiceModel(ch config.Choice, model string) bool {
	id := strings.ToLower(strings.TrimSpace(ch.Model))
	if !strings.EqualFold(strings.TrimSpace(ch.Harness), omp.Harness) || id == "" {
		return false
	}
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(model)), "/"+id)
}

// choiceModelMatches compares a configured selector with a requested or
// recorded model, including omp's provider-qualified form of a bare id.
func choiceModelMatches(ch config.Choice, model string) bool {
	return strings.EqualFold(strings.TrimSpace(ch.Model), strings.TrimSpace(model)) || ompChoiceModel(ch, model)
}

func resolveAgentProviderBucket(k config.Capacity, harness, model string) (prov, bucket string) {
	h := cmp.Or(strings.TrimSpace(harness), claudeComm)
	m := strings.TrimSpace(model)
	for _, ch := range k.Choices {
		if !strings.EqualFold(ch.Harness, h) {
			continue
		}
		if m != "" && (strings.EqualFold(ch.Model, m) || strings.HasPrefix(strings.ToLower(m), strings.ToLower(ch.Model)) || ompChoiceModel(ch, m)) {
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
	s := fmt.Sprintf("%d busy of ceiling %d", len(c.Busy), k.Ceiling)
	if len(c.Busy) > 0 {
		s += ": " + strings.Join(c.Busy, ", ")
	}
	s += fmt.Sprintf("; %d parked, %d waiting, %d idle", len(c.Parked), len(c.Waiting), len(c.Idle))
	if len(c.Gone) > 0 {
		s += "; gone: " + strings.Join(c.Gone, ", ")
	}
	return s
}

// headroom is what bounds a new start: MemAvailable, disk swap's growth
// over the watch's readings weighed with memory pressure and MemAvailable,
// the 1-minute load against the watch's limit, the kind labs against their
// cap, and for a start that builds the free build slots.
type headroom struct {
	// Disk is the watch's latest reading, sharing its thresholds and source.
	Disk         *watchDiskReading `json:"disk,omitempty"`
	AvailableMiB int               `json:"availableMiB"`
	AvailMinMiB  int               `json:"availMinMiB"`
	MemErr       string            `json:"memError,omitempty"`
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
	if h.Disk != nil && h.Disk.Low {
		out = append(out, "disk pressure: "+h.diskFigure())
	}
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

func (h *headroom) diskFigure() string {
	return fmt.Sprintf("free disk / %d MiB (floor %d MiB)", h.Disk.FreeMiB, h.Disk.MinMiB)
}

// line says the headroom in one line.
func (h *headroom) line() string {
	mem := "MemAvailable unknown"
	if h.MemErr == "" {
		mem = fmt.Sprintf("MemAvailable %.1f GiB (floor %d GiB)", float64(h.AvailableMiB)/1024, gib(h.AvailMinMiB))
	}
	mem += ", " + h.psiFigure()
	if h.Disk != nil {
		mem += ", " + h.diskFigure()
	}
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

// reviewCeiling never opens a deliberately stopped or usage-stopped pool.
func reviewCeiling(ceiling, extra int) int {
	if ceiling == 0 {
		return 0
	}
	return ceiling + extra
}

// reviewDiskShare divides the disk floor for a review start: a review
// builds little and is what lets work merge and free its worktrees, so a
// disk just under the floor must not stop every review. The watch prunes
// from the full floor on, whatever the review's own floor.
const reviewDiskShare = 2

// forStart is the headroom a start of kind is held to.
func (h *headroom) forStart(kind startFor) *headroom {
	if kind == forWorker || h == nil || h.Disk == nil || !h.Disk.Low {
		return h
	}
	review, disk := *h, *h.Disk
	disk.MinMiB /= reviewDiskShare
	disk.Low = disk.FreeMiB < disk.MinMiB
	review.Disk = &disk
	return &review
}

func newStartCapacity(c agentCount, k config.Capacity, h *headroom, builds bool, kind startFor) *capacityView {
	h = h.forStart(kind)
	base := k.Ceiling
	if kind != forWorker {
		k.Ceiling = reviewCeiling(base, k.ReviewHeadroom)
	}
	return newCapacity(c, k, h, builds).withReviewHeadroom(k.ReviewHeadroom, base, len(c.Busy), kind)
}

func (v *capacityView) withReviewHeadroom(extra, ceiling, busy int, kind startFor) *capacityView {
	v.WorkerCeiling = ceiling
	v.ReviewHeadroom, v.ReviewHeadroomUsed, v.Review = extra, max(0, busy-ceiling), kind != forWorker
	if v.Review && ceiling > 0 && busy >= ceiling+extra {
		v.Room = 0
		v.reviewLimitReached = true
		v.Blocks = append(v.Blocks, fmt.Sprintf("review headroom exhausted: %d busy at ceiling %d plus review headroom %d", busy, ceiling, extra))
	}
	return v
}

// newCapacity weighs the count against the target and the headroom for a
// start that builds or not: only one that builds waits for the build slot,
// one that does not is told the slot is busy; disk swap growing without
// pressure is a note, not a block.
func newCapacity(c agentCount, k config.Capacity, h *headroom, builds bool) *capacityView {
	v := &capacityView{agentCount: c, Ceiling: k.Ceiling, Headroom: h, Builds: builds}
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
	// pendingMode is the mode a serving-mode switch waits to run into:
	// while it stands, the provider drains to that mode's ceiling and no
	// session starts on the model.
	pendingMode string
	// hasConstraintCeiling is whether constraintCeiling says anything:
	// set when the constraints held name a mode and the provider runs in
	// one the constraints allow.
	hasConstraintCeiling bool
	// constraintCeiling is the ceiling the constraints held right now
	// allow: the sessions the mode they permit sustains.
	constraintCeiling int
}

// windowUnderused reports whether sd's 7-day usage is heavily behind its pace
// at now according to u's underuse thresholds.
func windowUnderused(sd *usageWindow, u config.Usage, now time.Time) bool {
	ratio := u.EffectiveUnderuseRatio()
	if ratio <= 0 || sd == nil || sd.Pace == nil {
		return false
	}
	if *sd.Pace+1e-9 < u.EffectiveUnderuseMinPace() {
		return false
	}
	if !sd.ResetsAt.IsZero() && sd.ResetsAt.Sub(now) <= u.EffectiveUnderuseMinRemaining() {
		return false
	}
	return sd.UsedPercent+1e-9 < *sd.Pace*ratio
}

func buildSingleProviderView(name string, pCfg config.Provider, pa providerAgents, res providerReadingResult, now time.Time) *providerView {
	return buildProviderForStart(name, pCfg, pa, res, now, 0, forWorker)
}

func buildProviderForStart(name string, pCfg config.Provider, pa providerAgents, res providerReadingResult, now time.Time, extra int, kind startFor) *providerView {
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
	// Every subscription needs a reading, even when no reader was configured.
	pv.HighPercent = pCfg.EffectiveHighPercent()
	if !res.ok || res.err != nil {
		errMsg := "usage not read"
		if res.err != nil {
			errMsg = res.err.Error()
		}
		pv.Ceiling, pv.Room = 0, 0
		pv.Error = errMsg
		pv.Stale = strings.Contains(errMsg, "stale")
		pv.Blocks = []string{"subscription usage unknown (" + errMsg + ")"}
		pv.Verdict = providerVerdict(0, pv.Blocks)
		return pv
	}
	r := res.reading
	pv.At = r.At
	pv.Fallback = r.Fallback
	if r.NoPlanWindows {
		pv.NoPlanWindows = true
		pv.Blocks = []string{claudeNoPlanWindows}
		pv.Verdict = providerVerdict(0, pv.Blocks)
		return pv
	}
	if len(r.Buckets) > 0 && r.FiveHour == nil && r.SevenDay == nil {
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
			var pace *float64
			if kind == forWorker {
				pace = sdPace(sd)
			}
			dp, high := pCfg.DeriveWithPace(pctPtr(fh), pctPtr(sd), pace)
			dp.Ceiling = reviewCeiling(dp.Ceiling, extra)
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
			case fh != nil && fh.UsedPercent >= 100:
				bBlocks = append(bBlocks, fmt.Sprintf("5h %s at or past 100%%", pct(fh.UsedPercent)))
			case pCfg.SevenDayMax > 0 && sd != nil && sd.UsedPercent >= pCfg.SevenDayMax:
				bBlocks = append(bBlocks, fmt.Sprintf("7d %s at or past %s", pct(sd.UsedPercent), pct(pCfg.SevenDayMax)))
			case sd != nil && sd.UsedPercent >= 100:
				bBlocks = append(bBlocks, fmt.Sprintf("7d %s at or past 100%%", pct(sd.UsedPercent)))
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
			underused := windowUnderused(sd, pCfg.Usage, now)
			if underused {
				pv.Underused = true
			}
			maxBucketCeiling = max(maxBucketCeiling, dp.Ceiling)
			pv.Buckets[bName] = &bucketView{
				Name:      bName,
				Busy:      bBusy,
				Ceiling:   dp.Ceiling,
				Room:      bRoom,
				FiveHour:  fh,
				SevenDay:  sd,
				Pace:      sdPace(sd),
				High:      high,
				Underused: underused,
				Blocks:    bBlocks,
			}
		}
		if maxBucketCeiling == 0 {
			pv.Ceiling, pv.Room = 0, 0
			pv.High = allHigh
			if allHigh {
				pv.FiveHour = firstHighFH
			}
			pv.Blocks = bucketBlocks
			pv.Verdict = providerVerdict(0, pv.Blocks)
			return pv
		}
		pv.Ceiling = min(reviewCeiling(pCfg.Ceiling, extra), maxBucketCeiling)
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
	pv.Underused = windowUnderused(pv.SevenDay, pCfg.Usage, now)
	var pace *float64
	if kind == forWorker {
		pace = pv.Pace
	}
	dp, high := pCfg.DeriveWithPace(pctPtr(pv.FiveHour), pctPtr(pv.SevenDay), pace)
	pv.Ceiling, pv.High = reviewCeiling(dp.Ceiling, extra), high
	switch {
	case pv.High:
		pv.Blocks = []string{highBlockText(pv.FiveHour, pv.HighPercent, now)}
	case pCfg.FiveHourMax > 0 && pv.FiveHour != nil && pv.FiveHour.UsedPercent >= pCfg.FiveHourMax:
		pv.Blocks = []string{fmt.Sprintf("5h %s at or past %s", pct(pv.FiveHour.UsedPercent), pct(pCfg.FiveHourMax))}
	case pv.FiveHour != nil && pv.FiveHour.UsedPercent >= 100:
		pv.Blocks = []string{fmt.Sprintf("5h %s at or past 100%%", pct(pv.FiveHour.UsedPercent))}
	case pCfg.SevenDayMax > 0 && pv.SevenDay != nil && pv.SevenDay.UsedPercent >= pCfg.SevenDayMax:
		pv.Blocks = []string{fmt.Sprintf("7d %s at or past %s", pct(pv.SevenDay.UsedPercent), pct(pCfg.SevenDayMax))}
	case pv.SevenDay != nil && pv.SevenDay.UsedPercent >= 100:
		pv.Blocks = []string{fmt.Sprintf("7d %s at or past 100%%", pct(pv.SevenDay.UsedPercent))}
	case pv.Ceiling == 0:
		pv.Blocks = []string{ceilingZero}
	case len(pv.Busy) >= pv.Ceiling:
		pv.Blocks = []string{fmt.Sprintf("%d busy at the ceiling of %d", len(pv.Busy), pv.Ceiling)}
	default:
		pv.Room = pv.Ceiling - len(pv.Busy)
	}
	// Model weeks supplement, rather than replace, the aggregate plan windows.
	if res.ok && len(res.reading.Buckets) > 0 && (res.reading.FiveHour != nil || res.reading.SevenDay != nil) {
		pv.supplementalBuckets = true
		pv.Buckets = make(map[string]*bucketView)
		pv.bucketOrder = bucketNames(res.reading)
		for _, name := range pv.bucketOrder {
			b := res.reading.Buckets[name]
			pv.Buckets[name] = &bucketView{Name: name, SevenDay: effectiveSevenDayWindow(b.SevenDay, now)}
		}
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
	if res.hasConstraintCeiling && res.constraintCeiling < eff.Ceiling {
		// A constraint holds a node the running mode occupies: the
		// provider's ceiling is the mode the constraints allow, even
		// before the switch into it ran.
		eff.Ceiling = res.constraintCeiling
	}
	if res.pendingMode != "" {
		// A pending switch to a mode the constraints allow lowers the
		// provider's ceiling to that mode's while the model drains.
		if c, ok := pCfg.ServeModeCeiling(res.pendingMode); ok && c < eff.Ceiling {
			eff.Ceiling = c
		}
	}
	pv.Ceiling = eff.Ceiling
	pv.ServingMode = res.serveMode
	if res.pendingMode != "" && !strings.EqualFold(res.pendingMode, res.serveMode) {
		pv.DrainTo = res.pendingMode
	}
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
func buildProvidersCapacity(c agentCount, pa map[string]providerAgents, k config.Capacity, readings map[string]providerReadingResult, h *headroom, builds bool, exclude []string, kind startFor, now time.Time) *capacityView {
	h = h.forStart(kind)
	baseCeiling := k.Ceiling
	extra := 0
	if kind != forWorker {
		extra = k.ReviewHeadroom
		k.Ceiling = reviewCeiling(k.Ceiling, extra)
	}
	providers := make(map[string]*providerView, len(k.Providers))
	sumCeiling := 0
	subCeiling := 0
	anySubscription := false
	anySelfHosted := false
	for _, name := range k.ProviderNames() {
		pCfg := k.Providers[name]
		pv := buildProviderForStart(name, pCfg, pa[name], readings[name], now, extra, kind)
		providers[name] = pv
		sumCeiling += pv.Ceiling
		// capacity.ceiling caps the subscription providers, not a self-hosted
		// one: a desk that lowers capacity.ceiling to 0 to keep a subscription
		// on its weekly pace still starts on its own model.
		if pCfg.SelfHosted() {
			anySelfHosted = true
		} else {
			anySubscription = true
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
	// agent asks nothing of it and never weighs against it. With no
	// subscription provider in the pool it is bounded by capacity.ceiling
	// alone — a desk that lowers it to 0 holds every start that is not on a
	// self-hosted provider, and a self-hosted provider's ceiling bounds none
	// of them.
	machineBusy := subscriptionBusy
	// subscriptionCap is what capacity.ceiling still leaves the starts that
	// are on no self-hosted provider: with a subscription provider in the
	// pool, its providers' ceiling and busy agents; with none, capacity.ceiling
	// minus the busy agents that resolve to no provider of the pool — an agent
	// of the pool belongs to its provider's own target, never to this one.
	subscriptionCap := max(0, min(k.Ceiling, subCeiling)-subscriptionBusy)
	// unassignedBusy is capacity's own count of the busy sessions of no
	// provider of the pool, the weight forTarget names in the verdict of a
	// start that resolves to no provider: with a subscription provider in
	// the pool every busy agent weighs on its side, with none only those of
	// no provider of the pool.
	unassignedBusy := 0
	if !anySubscription {
		assigned := map[string]bool{}
		for _, name := range k.ProviderNames() {
			for _, n := range providers[name].Busy {
				assigned[n] = true
			}
		}
		for _, n := range c.Busy {
			if !assigned[n] {
				unassignedBusy++
			}
		}
		subscriptionCap = max(0, k.Ceiling-unassignedBusy)
	}
	subRoomCap := subscriptionCap
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
		// capacity.ceiling holds the subscription side to. With no
		// subscription provider in the pool there is no subscription side to
		// narrow; capacity.ceiling bounds the starts that run on no
		// self-hosted provider, and forTarget judges one of those.
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
		Ceiling:        effCeiling,
		Providers:      providers,
		Headroom:       h,
		Builds:         builds,
		subRoomCap:     subRoomCap,
		selfHostedRoom: selfHostedRoom,
		unassignedBusy: unassignedBusy,
	}
	if line, blocks := h.swapGuard(); line != "" && !blocks {
		v.Notes = append(v.Notes, line)
	}
	if !builds && h.slotBusy() {
		v.Notes = append(v.Notes, noFreeSlot+": a start that builds (--builds) waits for it")
	}

	if len(k.Choices) > 0 {
		v.Choice = evaluateChoices(k, providers, mBlocks, "", exclude, kind, now)
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
			// A self-hosted choice stands outside capacity.ceiling: what the
			// cap leaves the choices is its own room plus what the cap holds
			// the choices of no provider entry to — never the other way, a
			// self-hosted provider at its ceiling holding a desk with a free
			// capacity.ceiling from every start.
			v.Room = min(choiceRoom, machineRoomCap)
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
			switch {
			case !anySelfHosted && k.Ceiling > 0 && anySubscription && subHadRoom && subRoomCap == 0:
				v.Blocks = []string{totalCeilingBlock(k, subscriptionBusy, subCeiling, effCeiling, anySubscription)}
			case !anySelfHosted && k.Ceiling == 0 && anySubscription && subHadRoom:
				v.Blocks = []string{ceilingZero}
			default:
				for _, name := range k.ProviderNames() {
					if pv := providers[name]; len(pv.Blocks) > 0 {
						v.Blocks = append(v.Blocks, name+": "+strings.Join(pv.Blocks, "; "))
					}
				}
			}
		}
	}
	return v.withReviewHeadroom(k.ReviewHeadroom, baseCeiling, max(subscriptionBusy, unassignedBusy), kind)
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
		if cand.Bucket != "" && len(pv.Buckets) > 0 && !pv.supplementalBuckets {
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

// evaluateChoices prioritizes eligible subscriptions below half their 7-day
// pace, then ranks by distance behind pace and budget left per hour until reset
// (remaining percent / hours to reset) and lower configured cost. Configured
// order breaks ties; self-hosted choices are fallbacks. Worker starts keep the
// strict 7-day pace check; review starts inside the reserved review headroom
// are not refused by pace.
func evaluateChoices(k config.Capacity, providers map[string]*providerView, machineBlocks []string, stopped string, exclude []string, kind startFor, now time.Time) *choiceView {
	normEx := normalizeExcludes(exclude)
	cv := &choiceView{
		Review:             kind != forWorker,
		ReviewOfSelfHosted: kind == forReviewOfSelfHosted,
		Exclude:            normEx,
	}
	if k.TaskKind != "" || len(k.TaskPolicies) > 0 {
		cv.TaskKind = k.TaskKind
		if cv.TaskKind == "" {
			cv.TaskKind = workerRole
			if kind != forWorker {
				cv.TaskKind = taskKindReview
			}
		}
		cv.Decision = "task " + cv.TaskKind + ": " + choiceRule
	}
	var reasons []string
	bestSubscription := false
	bestUnderused := false
	bestBehind := math.Inf(-1)
	bestRate := math.Inf(-1)
	bestCost := math.Inf(1)

	for _, ch := range k.Choices {
		provName := ch.EffectiveProvider()
		pCfg := k.Providers[provName]
		pv := providers[provName]
		bucketName := ""
		if pv != nil && len(pv.Buckets) > 0 && !pv.supplementalBuckets {
			bucketName = ch.EffectiveBucket(pCfg)
		}
		cand := choiceCandidate{
			Class: ch.Class, Cost: ch.Cost, Family: ch.Family, Effort: ch.Effort,
			Harness:  ch.Harness,
			Model:    ch.Model,
			Provider: provName,
			Bucket:   bucketName,
		}
		reason := modelPolicyReason(k, ch, pv, bucketName, kind)
		if reason == "" {
			reason = evaluateSingleChoice(ch, pCfg, pv, bucketName, machineBlocks, stopped, normEx, k.ProviderNames(), kind, now)
		}
		if reason == "" {
			cand.OK = true
			subscription := !pCfg.SelfHosted()
			underused := false
			behind := 0.0
			rate := 0.0
			if subscription {
				sd := pv.SevenDay
				if len(pv.Buckets) > 0 && !pv.supplementalBuckets {
					sd = pv.findBucket(bucketName).SevenDay
				}
				if sd != nil && sd.Pace != nil {
					underused = sd.UsedPercent < *sd.Pace*0.5
					behind = *sd.Pace - sd.UsedPercent
					rounded := math.Round(behind*10) / 10
					cand.BehindPace = &rounded
				}
				if r, ok := windowRate(sd, now); ok {
					rate = r
					rounded := math.Round(r*10) / 10
					cand.Rate = &rounded
				}
			}
			if cv.Harness == "" || subscription && (!bestSubscription || underused && !bestUnderused || underused == bestUnderused && (behind > bestBehind+1e-9 || math.Abs(behind-bestBehind) <= 1e-9 && (rate > bestRate+1e-9 || math.Abs(rate-bestRate) <= 1e-9 && ch.Cost > 0 && ch.Cost < bestCost))) {
				bestSubscription, bestUnderused, bestBehind, bestRate, bestCost = subscription, underused, behind, rate, ch.Cost
				cv.Harness = ch.Harness
				cv.Model = ch.Model
				cv.Effort = ch.Effort
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

// windowRate returns w's remaining budget per hour until its 7-day reset at
// now (remaining percent / hours to reset), and whether hours to reset are
// known.
func windowRate(w *usageWindow, now time.Time) (float64, bool) {
	if w == nil {
		return 0, false
	}
	hours := 0.0
	switch {
	case !w.ResetsAt.IsZero() && now.Before(w.ResetsAt):
		hours = w.ResetsAt.Sub(now).Hours()
	case w.ResetsAt.IsZero() && w.Pace != nil && *w.Pace < 100:
		hours = (100 - *w.Pace) / 100 * sevenDaySpan.Hours()
	default:
		return 0, false
	}
	if hours <= 0 {
		return 0, false
	}
	return max(0, 100-w.UsedPercent) / hours, true
}

func evaluateSingleChoice(ch config.Choice, pCfg config.Provider, pv *providerView, bucketName string, machineBlocks []string, stopped string, excludes, providerNames []string, kind startFor, now time.Time) string {
	if choiceExcluded(ch, excludes, providerNames) {
		return "excluded"
	}
	if kind == forReviewOfSelfHosted && pCfg.SelfHosted() {
		return selfHostedReviewer
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
	if len(pv.Buckets) > 0 && !pv.supplementalBuckets {
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
		if fh != nil && fh.UsedPercent >= 100 {
			return fmt.Sprintf("5h %s at or past 100%%", pct(fh.UsedPercent))
		}
		if pCfg.SevenDayMax > 0 && sd != nil && sd.UsedPercent >= pCfg.SevenDayMax {
			return fmt.Sprintf("7d %s at or past %s", pct(sd.UsedPercent), pct(pCfg.SevenDayMax))
		}
		if sd != nil && sd.UsedPercent >= 100 {
			return fmt.Sprintf("7d %s at or past 100%%", pct(sd.UsedPercent))
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
		if fh != nil && fh.UsedPercent >= 100 {
			return fmt.Sprintf("5h %s at or past 100%%", pct(fh.UsedPercent))
		}
		if pCfg.SevenDayMax > 0 && sd != nil && sd.UsedPercent >= pCfg.SevenDayMax {
			return fmt.Sprintf("7d %s at or past %s", pct(sd.UsedPercent), pct(pCfg.SevenDayMax))
		}
		if sd != nil && sd.UsedPercent >= 100 {
			return fmt.Sprintf("7d %s at or past 100%%", pct(sd.UsedPercent))
		}
		if pv.Room <= 0 {
			if len(pv.Blocks) > 0 {
				return strings.Join(pv.Blocks, "; ")
			}
			return fmt.Sprintf("no room on %s", pv.Name)
		}
	}

	if sd != nil {
		if max7d, ok := ch.EffectiveWorkerSevenDayMax(); ok && sd.UsedPercent >= max7d {
			return fmt.Sprintf("7d %s at or past worker cap %s", pct(sd.UsedPercent), pct(max7d))
		}
	}
	// No worker start runs ahead of the even pace: a margin, the choice's or
	// its provider's, may only tighten it, and skipPace no longer waives it.
	// Desk reviews inside the reserved review headroom are not refused by the
	// pace check, only by a provider at its hard limit.
	if kind == forWorker && sd != nil && sd.Pace != nil {
		margin := min(ch.EffectivePaceMargin(pCfg), pCfg.PaceMargin, 0)
		over := math.Round((sd.UsedPercent-*sd.Pace)*10) / 10
		if over > margin+1e-9 {
			if over > 0 {
				return fmt.Sprintf("7d %s ahead of pace %s (+%s)", pct(sd.UsedPercent), pct(*sd.Pace), pct(over))
			}
			return fmt.Sprintf("7d %s within %s points of pace %s", pct(sd.UsedPercent), strconv.FormatFloat(-margin, 'f', -1, 64), pct(*sd.Pace))
		}
	}
	return ""
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

// pickView is capacity pick's answer: the start's harness and model, or
// none, with the decision behind it.
type pickView struct {
	Pick     string        `json:"pick"`
	Harness  string        `json:"harness,omitempty"`
	Model    string        `json:"model,omitempty"`
	Provider string        `json:"provider,omitempty"`
	Bucket   string        `json:"bucket,omitempty"`
	Reason   string        `json:"reason,omitempty"`
	Capacity *capacityView `json:"capacity,omitempty"`
	Verdict  string        `json:"verdict,omitempty"`
}

// pickNone is capacity pick's line when no start fits.
const pickNone = "none"

// newPick is v's start: its choice while v has room and no stop stands, else
// none with what stops it.
func newPick(v *capacityView) *pickView {
	p := &pickView{Pick: pickNone, Capacity: v, Verdict: v.verdict()}
	ch := v.Choice
	switch {
	case v.Stopped != "":
		p.Reason = v.Stopped
	case ch == nil:
		p.Reason = "no capacity.choices configured"
	case ch.Harness == "" || ch.Model == "":
		p.Reason = cmp.Or(ch.Reason, "no choice in capacity.choices")
	case v.Room <= 0:
		p.Reason = strings.Join(v.Blocks, "; ")
	default:
		p.Pick = ch.Harness + " " + ch.Model
		p.Harness, p.Model, p.Provider, p.Bucket = ch.Harness, ch.Model, ch.Provider, ch.Bucket
	}
	return p
}

// forTarget narrows v's Room and Blocks to the resolved provider and bucket
// when an explicit harness (not auto) is given; a local cap v carries
// (capacity.local.ceiling) still bounds the narrowed room.
func (v *capacityView) forTarget(k config.Capacity, harness, model string) *capacityView {
	return v.forPoolTarget(k, harness, model).capLocal()
}

func (v *capacityView) forPoolTarget(k config.Capacity, harness, model string) *capacityView {
	if v == nil || v.Stopped != "" || harness == harnessAuto {
		return v
	}
	task := cmp.Or(k.TaskKind, workerRole)
	if k.TaskKind == "" && v.Review {
		task = taskKindReview
	}
	policy := k.TaskPolicies[task]
	if policy.MinClass > 0 || policy.MaxCost > 0 || policy.DifferentFamily || k.AuthorModel != "" || v.Review && k.TaskPolicies[taskKindReview].DifferentFamily {
		known := slices.ContainsFunc(k.Choices, func(ch config.Choice) bool {
			return ch.Harness == harness && choiceModelMatches(ch, model)
		})
		if !known {
			v.Room, v.Blocks = 0, []string{"model policy: target has no configured capability, cost or family metadata"}
			return v
		}
	}
	if v.Choice != nil {
		for _, cand := range v.Choice.Candidates {
			if cand.Harness == harness && choiceModelMatches(config.Choice{Harness: cand.Harness, Model: cand.Model}, model) && strings.HasPrefix(cand.Reason, modelPolicyPrefix) {
				v.Room, v.Blocks = 0, []string{cand.Reason}
				return v
			}
		}
	}
	if v.reviewLimitReached || len(v.Providers) == 0 {
		return v
	}

	prov, bucket := resolveAgentProviderBucket(k, harness, model)
	pv := v.Providers[prov]
	if pv == nil {
		// A harness the desk's providers claim none of — claude, agy, codex
		// on a desk with only a self-hosted provider: capacity.ceiling
		// itself bounds it, and only the busy sessions of no provider of the
		// pool weigh on it (v.unassignedBusy); the busy agents of a
		// self-hosted provider belong to its own ceiling, never to this
		// target. The machine's guards weigh on every target alike.
		if mBlocks := v.Headroom.blocks(v.Builds); len(mBlocks) > 0 {
			v.Room = 0
			v.Blocks = mBlocks
			return v
		}
		v.Room = v.subRoomCap
		switch {
		case v.subRoomCap > 0:
			v.Blocks = nil
		case k.Ceiling == 0:
			v.Blocks = []string{ceilingZero}
		default:
			v.Blocks = []string{fmt.Sprintf("%d busy at the ceiling of %d", v.unassignedBusy, k.Ceiling)}
		}
		return v
	}
	if bucket != "" && len(pv.Buckets) > 0 && !pv.supplementalBuckets {
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

func choicesHaveModel(choices []config.Choice, model string) bool {
	m := strings.TrimSpace(model)
	return slices.ContainsFunc(choices, func(ch config.Choice) bool {
		return choiceModelMatches(ch, m)
	})
}

func filterChoiceModel(k config.Capacity, model string) config.Capacity {
	m := strings.TrimSpace(model)
	if k.AuthorFamily == "" {
		for _, ch := range k.Choices {
			if choiceModelMatches(ch, k.AuthorModel) {
				k.AuthorFamily = ch.Family
				break
			}
		}
	}
	if m == "" || len(k.Choices) == 0 {
		return k
	}
	var filtered []config.Choice
	for _, ch := range k.Choices {
		if choiceModelMatches(ch, m) {
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
