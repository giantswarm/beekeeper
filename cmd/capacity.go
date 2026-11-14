package cmd

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/claude"
	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/state"
)

// waitQuiet is how long a session whose tool commands only wait must have
// been quiet before its agent counts as waiting rather than busy: a turn
// that still works writes its transcript between its commands.
const waitQuiet = 5 * time.Minute

// countAgents sorts st's roster at now; sessions say which agents wait on
// their person or in a wait of their own (none known without them).
func countAgents(st *state.State, sessions []*claude.Session, now time.Time, cfg *config.Config) capacity.AgentCount {
	cfg, staleAfter := rosterConfig(cfg)
	c := capacity.AgentCount{Busy: []string{}, Parked: []string{}, Waiting: []string{}, Idle: []string{}, Gone: []string{}}
	guide := st.GuideRole()
	for _, ag := range st.Agents {
		if ag.Execution != nil {
			if !executionReserved(st, ag) {
				c.Idle = append(c.Idle, ag.Name)
			} else if k := keptBy(st, ag, now); k != "" {
				c.Parked = append(c.Parked, fmt.Sprintf("%s (%s)", ag.Name, k))
			} else {
				c.Busy = append(c.Busy, ag.Name)
			}
			continue
		}
		p := ag.Party
		if roleRun(st, p) || guide.Relay.Open(now) && guide.Relay.To.Is(p) {
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
		if last, gone := goneAgent(cfg, st, sessions, ag, staleAfter, now); gone {
			c.Gone = append(c.Gone, fmt.Sprintf("%s (last activity %s)", ag.Name, last.UTC().Format(time.RFC3339)))
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

func countProviderAgents(st *state.State, sessions []*claude.Session, k config.Capacity, now time.Time, cfg *config.Config) map[string]capacity.ProviderAgents {
	cfg, staleAfter := rosterConfig(cfg)
	out := make(map[string]capacity.ProviderAgents, len(k.Providers))
	for _, name := range k.ProviderNames() {
		out[name] = capacity.ProviderAgents{Busy: []string{}, BucketBusy: map[string][]string{}}
	}
	guide := st.GuideRole()
	for _, ag := range st.Agents {
		if ag.Execution != nil {
			if !executionReserved(st, ag) || keptBy(st, ag, now) != "" {
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
		if roleRun(st, p) || guide.Relay.Open(now) && guide.Relay.To.Is(p) {
			continue
		}
		if ag.Task == "" || ag.Done || keptBy(st, ag, now) != "" {
			continue
		}
		s, live := claude.Live(sessions, p)
		if live && (s.Waiting != nil || s.OwnWait(now, waitQuiet) != "") {
			continue
		}
		if _, gone := goneAgent(cfg, st, sessions, ag, staleAfter, now); gone {
			continue
		}
		harness, model := capacity.AgentHarnessModel(st, sessions, ag)
		prov, bucket := capacity.ResolveAgentProviderBucket(k, harness, model)
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

// swapFile is the state's side file the watch keeps its latest swap reading
// in, for capacity; swapFresh is how old a reading may be and still count.
const (
	swapFile  = "swap.json"
	swapFresh = 5 * time.Minute
)

func (a *app) resolveProvidersCapacity(ctx context.Context, st *state.State, sessions []*claude.Session, k config.Capacity, builds bool, exclude []string, kind capacity.StartFor) *capacity.View {
	return a.resolveProvidersCapacityForModel(ctx, st, sessions, k, builds, exclude, kind, "")
}

func (a *app) resolveProvidersCapacityForModel(ctx context.Context, st *state.State, sessions []*claude.Session, k config.Capacity, builds bool, exclude []string, kind capacity.StartFor, wantModel string) *capacity.View {
	now := a.now
	if now.IsZero() {
		now = clk.Now()
	}
	a.syncAgyQuotaFromState(st, now)
	c := countAgents(st, sessions, now, a.cfg)
	pa := countProviderAgents(st, sessions, k, now, a.cfg)
	readings := make(map[string]capacity.ProviderReadingResult, len(k.Providers))
	for _, name := range k.ProviderNames() {
		p := k.Providers[name]
		if !p.Enabled() {
			continue
		}
		r, err := a.resolveReading(ctx, usageProviderFile(name), p.Usage)
		if !a.now.IsZero() {
			now = a.now
		}
		readings[name] = capacity.ProviderReadingResult{Reading: r, Err: err, OK: err == nil}
	}
	for _, name := range k.ProviderNames() {
		p := k.Providers[name]
		if !p.SelfHosted() {
			continue
		}
		// A self-hosted provider reads no subscription usage; its ceiling may
		// follow the serving mode of the model it serves.
		readings[name] = a.serveModeReadings(ctx, name, p)
	}
	withPaceOffs(readings, st, now)
	h := a.readHeadroom(ctx, k, a.watchSwap())
	choicesCap := capacity.FilterChoiceModel(k, wantModel)
	v := capacity.BuildProvidersCapacity(c, pa, choicesCap, readings, h, builds, exclude, kind, now)
	if len(k.Choices) > 0 && len(choicesCap.Choices) == 0 {
		reason := fmt.Sprintf("model %q is not in capacity.choices", strings.TrimSpace(wantModel))
		v.Choice = &capacity.ChoiceView{Review: kind != capacity.ForWorker, ReviewOfSelfHosted: kind == capacity.ForReviewOfSelfHosted, Reason: reason}
		v.Room, v.Blocks = 0, []string{"no choice has room: " + reason}
	}
	return v
}

// capacityStopped makes the verdict "no start" while st's stop or "starts" hold
// stands (noStart).
func capacityStopped(v *capacity.View, a *app, st *state.State) *capacity.View {
	if why := a.noStart(st); why != "" {
		v.Stopped = why
		v.Blocks, v.Room = append([]string{v.Stopped}, v.Blocks...), 0
		for _, pv := range v.Providers {
			pv.Room = 0
			pv.Blocks = append([]string{v.Stopped}, pv.Blocks...)
			pv.Verdict = capacity.ProviderVerdict(0, pv.Blocks)
		}
		if v.Choice != nil && v.Choice.Harness != "" {
			v.Choice.Harness, v.Choice.Model, v.Choice.Provider, v.Choice.Bucket = "", "", "", ""
			v.Choice.Reason = v.Stopped
		}
	}
	return v
}

// liveCapacity returns the current Capacity from the active configuration
// file (cfgPath or loadedCfg) without mutating a.cfg across goroutines; it
// falls back to a.cfg.Capacity when no configuration file path is active or
// on a read or parse error.
func (a *app) liveCapacity() config.Capacity {
	if p := cmp.Or(a.cfgPath, a.loadedCfg); p != "" {
		if c, err := config.Load(p); err == nil {
			c.Capacity.TaskKind, c.Capacity.AuthorModel = a.selectionKind, a.selectionAuthor
			return c.Capacity
		}
	}
	k := a.cfg.Capacity
	k.TaskKind, k.AuthorModel = a.selectionKind, a.selectionAuthor
	return k
}

// readHeadroom reads the machine's headroom now against k; swap is the
// watch's latest reading, nil without a fresh one.
func (a *app) readHeadroom(ctx context.Context, k config.Capacity, swap *capacity.SwapReading) *capacity.Headroom {
	slotDir, slots := a.slotView()
	h := &capacity.Headroom{AvailMinMiB: k.AvailMinMiB, SwapGrowthMaxMiB: k.SwapGrowthMaxMiB, PSIMax: k.SwapPSIMax, Swap: swap, Slots: slots}
	h.Disk = a.watchDisk()
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
func (a *app) watchSwap() *capacity.SwapReading {
	if a.store == nil {
		return nil
	}
	var r capacity.SwapReading
	if found, err := a.store.ReadFile(swapFile, &r); err != nil || !found || a.now.Sub(r.At) > swapFresh {
		return nil
	}
	return &r
}

func (a *app) capacityCmd() *cobra.Command {
	var host, model string
	var builds, review, ofSelfHosted bool
	var exclude []string
	c := &cobra.Command{
		Use:   "capacity [--host <host>] [--builds] [--review] [--review-of-self-hosted] [--exclude <choice>]",
		Short: "Busy agents against the target, the headroom for a start and the verdict",
		Long: `Count the roster's agents against the supervisor's target (capacity.ceiling;
unset, 10) and say whether a new start fits. A configured capacity.ceiling of 0
stops new starts ("no start: ceiling 0") and the subscription providers', but
not a self-hosted provider's, and suppresses CAPACITY FULL in the watch. When
capacity.usage.command is configured, beekeeper reads the subscription's
5-hour and 7-day usage windows (cached for capacity.usage.every, default 5m,
and re-read once resetsAt passes), derives the ceiling from
capacity.usage.fiveHour and capacity.usage.sevenDay (taking the lower ceiling
when both have bands), prints the windows on the capacity line and under usage
in --json,
and refuses unforced starts with "no start: usage high: …" once the 5-hour
window reaches capacity.usage.highPercent (default 95%) until
fiveHour.resetsAt. When capacity.providers and capacity.choices are
configured, beekeeper reads each provider's usage windows (such as Claude's
JSON windows and agy -p /usage's model buckets), counts busy agents against
the provider and bucket they run on, caps the subscription providers' total by
the machine guards and capacity.ceiling, and prints each provider's windows and
verdict plus eligible choices below half their 7-day pace first, then furthest
behind pace (with remaining
7-day budget per hour, lower configured cost, and capacity.choices order
breaking ties), with self-hosted choices as fallbacks. Each
provider must have room, its 5-hour window below its threshold and its 7-day use
within the weekly cap and, for a worker start, at or under the even pace of its
7-day window, unless the person's pace override of the provider (capacity pace
off, shown on its line) lets it run ahead until a time.
If built-in Codex usage is unreadable, its last good windows still bound starts;
without valid saved windows, its configured ceiling applies. The watch reports
the failure to the guide. Explicit account, spend and rate-limit stops remain
blocking until a successful reading clears them.
--review marks a review start, which may use capacity.reviewHeadroom extra
subscription sessions above the worker ceiling and is not refused by the 7-day
pace check (machine guards and provider window stops still apply;
capacity.reviewSkipPace, a choice's skipPace and a positive paceMargin are
ignored), and --review-of-self-hosted
marks the review of a self-hosted model's work, which takes no choice on a
self-hosted provider. capacity pick prints that choice alone. A provider of
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
There is no separate cap on live sessions (busy plus waiting): memory, swap,
load and lab headroom bound new starts instead. A fixed session cap would let
waiters exclude useful work despite available headroom. These guards judge
starts and fresh parked-worker continuations: they wait for room and use
current provider pace. Messages to already busy workers do not claim another
slot. Re-read capacity before starting replacement work.
The supervisor, the guide and a role's successor are not counted.

capacity.local.ceiling, when set, caps the busy agents this machine runs
apart from capacity.ceiling, which bounds the subscriptions across the
desk and its execution hosts: every busy agent whose session runs here
counts, a self-hosted provider's included, while an agent on an execution
host counts against that host's maxBusy and never here. Every local start
is refused at it ("no start: N busy locally at capacity.local.ceiling N"),
the supervisor loop's and one by hand, worker and resume alike; a review
start, which runs only here, may use capacity.reviewHeadroom above it as
above the worker ceiling ("... plus review headroom N"), the machine's
guards still applying. The "local:" line shows it. Unset, there is no
local cap; 0 refuses every
unforced local start. --host verdicts weigh only the choices a host can
run (Claude Code).

The headroom bounds a start: the watch's free disk reading against
watch.diskMinMiB, MemAvailable against capacity.availMinMiB,
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

A task kind of agents.kinds (agents by name regexp) at a cap gets a line
after the verdict, "no start <kind>: <reason>": as many of its agents
busy as its maxBusy, or its last recorded start less than its
minInterval ago. agents start refuses a start of one of its agents with
that reason, exit 3, whatever the verdict and --force; a name several
kinds match is held by each. --json has them in "kindBlocks".
capacity reads the roster, the sessions and the machine and changes
nothing; it makes no GitHub call.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if host != "" && host != executionLocal {
				h, err := a.executionHost(host)
				if err != nil {
					return err
				}
				unlock, err := a.probeLock("capacity-host-" + unsafeKey.ReplaceAllString(string(h.Identity), "_") + ".lock")
				if err != nil {
					return refused("capacity: %v", err)
				}
				defer unlock()
			}
			a.selectionKind = strings.TrimSpace(a.selectionKind)
			if a.selectionKind == capacity.TaskKindReview {
				review = true
			}
			if a.selectionAuthor != "" && !review && !ofSelfHosted {
				return usageErr(authorNeedsReview)
			}
			st, err := a.readSnapshot()
			if err != nil {
				return err
			}
			sessions, _, err := a.sessions()
			if err != nil && !platform.Missing(err) {
				return err
			}
			liveCap := a.liveCapacity()
			var v *capacity.View
			if host != "" && host != executionLocal {
				if v, err = a.hostStartView(cmd.Context(), host, st, sessions, builds, exclude, capacity.StartKind(review, ofSelfHosted), capacity.HarnessAuto, model); err != nil {
					return err
				}
			} else if len(liveCap.Providers) > 0 || len(liveCap.Choices) > 0 {
				v = capacityStopped(a.resolveProvidersCapacityForModel(cmd.Context(), st, sessions, liveCap, builds, exclude, capacity.StartKind(review, ofSelfHosted), model).WithLocal(st, liveCap), a, st)
			} else {
				c := countAgents(st, sessions, a.now, a.cfg)
				k, u := a.resolveUsageForStart(cmd.Context(), liveCap, capacity.StartKind(review, ofSelfHosted))
				v = capacityStopped(capacity.NewStartCapacity(c, k, a.readHeadroom(cmd.Context(), k, a.watchSwap()), builds, capacity.StartKind(review, ofSelfHosted)).WithUsage(u, a.now).WithLocal(st, liveCap), a, st)
			}
			events, err := state.FollowSnapshot(a.store, "capacity-activity", "v1", func(e state.Event) bool {
				return e.Verb == agentWakeEvent || e.Verb == resumedEvent || e.Verb == transcriptResumeEvent
			})
			if err != nil {
				return err
			}
			v.Activity = activityCounts(st, events, liveCap, a.now)
			v.Hosts = a.hostPlacementViews(st)
			v.KindBlocks = kindBlocks(a.cfg, st, sessions, a.cfg.Agents.Kinds, a.now)
			if a.json {
				return a.printJSON(struct {
					*capacity.View
					Verdict string `json:"verdict"`
					Guards  bool   `json:"guards"`
				}{v, v.Verdict(), a.cfg.Hooks.GuardsEnabled()})
			}
			p := func(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format+"\n", args...) }
			capLine := v.Line(config.Capacity{Ceiling: v.Ceiling})
			if v.Usage != nil {
				capLine += "; " + v.Usage.CapacitySuffix(a.now)
			}
			p("capacity: %s", capLine)
			if !a.cfg.Hooks.GuardsEnabled() {
				p("guards: off (hooks.guards=false; relay and result redaction remain active)")
			}
			p("review headroom: %d of %d used above worker ceiling %d (reviews only)", v.ReviewHeadroomUsed, v.ReviewHeadroom, v.WorkerCeiling)
			if v.Local != nil {
				p("local: %s", v.Local.Line())
			}
			for _, host := range v.Hosts {
				p("%s", host.Line())
			}
			for _, name := range liveCap.ProviderNames() {
				if pv := v.Providers[name]; pv != nil {
					p("provider %s: %s", name, pv.Line(a.now))
				}
			}
			for _, provider := range slices.Sorted(maps.Keys(v.Activity)) {
				activity := v.Activity[provider]
				p("activity 24h %s: %d wakes, %d resumes, %d fresh starts", provider, activity.Wakes, activity.Resumes, activity.FreshStarts)
			}
			if v.Choice != nil {
				p("choice: %s", v.Choice.Line())
				if v.Choice.Decision != "" {
					p("decision: %s", v.Choice.Decision)
				}
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
			p("%s", v.Headroom.Line())
			p("%s", v.Verdict())
			for _, b := range v.KindBlocks {
				p("%s", b.Line())
			}
			return nil
		},
	}
	c.Flags().BoolVar(&builds, buildsFlag, false, "the verdict for a start that builds, which waits for a free build slot")
	c.Flags().StringVar(&host, "host", "", "execution host selector; default local")
	c.Flags().StringVar(&model, "model", "", "filter capacity.choices to a model")
	c.Flags().StringVar(&a.selectionKind, "kind", "", "task kind for model policy (default: worker, or review with --review)")
	c.Flags().StringVar(&a.selectionAuthor, authorModelFlag, "", "model that wrote the reviewed change; exclude its configured family")
	c.Flags().BoolVar(&review, reviewFlag, false, "the verdict for a review start, using reserved review headroom")
	c.Flags().BoolVar(&ofSelfHosted, reviewOfSelfHostedFlag, false, "the verdict for the review of a self-hosted model's work: no self-hosted provider")
	c.Flags().StringSliceVar(&exclude, excludeFlag, nil, "exclude a choice by model, harness/model, provider or harness (repeatable or comma-separated)")
	c.AddCommand(a.capacityPickCmd(), a.capacityPaceCmd())
	return c
}

// buildsFlag marks a start whose task builds: the build slot is one of its
// guards. reviewFlag marks a review start, reviewOfSelfHostedFlag the review
// of a self-hosted model's work, excludeFlag excludes choices.
const (
	buildsFlag             = "builds"
	reviewFlag             = capacity.TaskKindReview
	reviewOfSelfHostedFlag = "review-of-self-hosted"
	excludeFlag            = "exclude"
)

func (a *app) capacityPickCmd() *cobra.Command {
	var model string
	var review, ofSelfHosted bool
	c := &cobra.Command{
		Use:   "pick [--review | --review-of-self-hosted]",
		Short: "The harness and model the next start takes, or none",
		Long: `Print the one start capacity's decision allows now, as "<harness> <model>",
or "none". The decision is the one agents start without --harness, the
supervisor loop's reviewers and its next starts take: the eligible subscription
below half its 7-day pace first, then furthest behind pace (then most 7-day
budget left per hour until its
reset, lower configured cost, and configured order breaking ties), with
self-hosted choices used when no subscription is eligible; a self-hosted choice
takes the worker kind (code changes) only when its kinds list it. Each eligible choice
has a configured provider and a free slot under its running-session ceiling,
5-hour and 7-day windows under their stop percent, and for a worker start 7-day
use not ahead of
the even pace of its window, with the
machine's headroom as a hard gate and capacity.ceiling bounding the
subscription providers together. A review may use capacity.reviewHeadroom extra subscription sessions above
the worker ceiling and is not refused by the 7-day pace check; machine guards and provider window stops still apply. --review-of-self-hosted decides the reviewer of a pull request a
self-hosted model's worker wrote: no choice on a self-hosted provider. --json
prints the pick with every choice's reason and eligible subscription's remaining
budget per hour (rate) and distance behind pace, each provider's windows and
verdict, and the headroom. Exit 0 either way; reads only.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a.selectionKind = strings.TrimSpace(a.selectionKind)
			if a.selectionKind == capacity.TaskKindReview {
				review = true
			}
			if a.selectionAuthor != "" && !review && !ofSelfHosted {
				return usageErr(authorNeedsReview)
			}
			kind := capacity.StartKind(review, ofSelfHosted)
			v, err := a.startCapacityForAuto(cmd.Context(), false, nil, kind, model)
			if err != nil {
				return err
			}
			p := capacity.NewPick(v)
			if a.json {
				return a.printJSON(p)
			}
			_, err = fmt.Fprintln(a.out, p.Pick)
			return err
		},
	}
	c.Flags().StringVar(&model, "model", "", "filter capacity.choices to a model")
	c.Flags().StringVar(&a.selectionKind, "kind", "", "task kind for model policy (default: worker, or review with --review)")
	c.Flags().StringVar(&a.selectionAuthor, authorModelFlag, "", "model that wrote the reviewed change; exclude its configured family")
	c.Flags().BoolVar(&review, reviewFlag, false, "decide a review start using reserved review headroom")
	c.Flags().BoolVar(&ofSelfHosted, reviewOfSelfHostedFlag, false, "decide the reviewer of a self-hosted model's work: no self-hosted provider")
	return c
}

// gateStart judges capacity's verdict v for a start: room lets it through
// (nothing to say), none refuses it (exit 3, the verdict's guards named)
// unless force, which overrides capacity for the event log. Model policy
// and the desk's stop remain refusals, including SSH reservation rechecks.
func gateStart(v *capacity.View, force bool) (string, error) {
	for _, b := range v.Blocks {
		if strings.HasPrefix(b, capacity.ModelPolicyPrefix) {
			return "", refused("%s", b)
		}
	}
	switch {
	case v.Stopped != "":
		return "", refused("%s", v.Verdict())
	case v.Room > 0:
		return "", nil
	case force:
		return v.Verdict(), nil
	}
	return "", refused("%s (beekeeper capacity; --force starts anyway)", v.Verdict())
}

// startGateWithOptions reads the capacity verdict for a start that builds or not,
// as beekeeper capacity reads it, narrowed to the target harness/model when
// explicit, and judges it (queueStart).
func (a *app) startGateWithOptions(ctx context.Context, name, harness, model string, builds, force bool, exclude []string, kind capacity.StartFor) (string, *capacity.View, error) {
	effModel := a.effectiveStartModel(harness, model)
	readCap := func(b bool) (*capacity.View, error) {
		if harness == capacity.HarnessAuto {
			return a.startCapacityForAuto(ctx, b, exclude, kind, model)
		}
		return a.startCapacityWithOptions(ctx, b, exclude, kind)
	}
	v, err := readCap(builds)
	if err != nil {
		return "", nil, err
	}
	v = v.ForTarget(a.liveCapacity(), harness, effModel)
	o, wait, err := a.startSlot()
	if err != nil {
		return "", nil, err
	}
	latest := v
	over, err := queueStart(v, builds, force, wait,
		func() (bool, int, error) { return guard.AwaitSlot(o, "agents start "+name) },
		func() (*capacity.View, error) {
			next, err := readCap(false)
			if err == nil {
				next = next.ForTarget(a.liveCapacity(), harness, effModel)
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
func queueStart(v *capacity.View, builds, force bool, wait string, await func() (bool, int, error), again func() (*capacity.View, error)) (string, error) {
	if !builds || force || !v.OnlySlot() {
		return gateStart(v, force)
	}
	turn, ahead, err := await()
	if err != nil {
		return "", err
	}
	if !turn {
		return "", refused("%s, %d ahead in the build slot queue after %s (beekeeper capacity; --force starts anyway)", v.Verdict(), ahead, wait)
	}
	if v, err = again(); err != nil {
		return "", err
	}
	return gateStart(v, force)
}

// startCapacity is capacity's verdict for a start now.
func (a *app) startCapacity(ctx context.Context, builds bool) (*capacity.View, error) {
	return a.startCapacityWithOptions(ctx, builds, nil, capacity.ForWorker)
}

func (a *app) startCapacityWithOptions(ctx context.Context, builds bool, exclude []string, kind capacity.StartFor) (*capacity.View, error) {
	return a.startCapacityForAuto(ctx, builds, exclude, kind, "")
}

func (a *app) startCapacityForAuto(ctx context.Context, builds bool, exclude []string, kind capacity.StartFor, wantModel string) (*capacity.View, error) {
	st, err := a.readSnapshot()
	if err != nil {
		return nil, err
	}
	sessions, _, err := a.sessions()
	if err != nil && !platform.Missing(err) {
		return nil, err
	}
	liveCap := a.liveCapacity()
	if len(liveCap.Providers) > 0 || len(liveCap.Choices) > 0 {
		return capacityStopped(a.resolveProvidersCapacityForModel(ctx, st, sessions, liveCap, builds, exclude, kind, wantModel).WithLocal(st, liveCap), a, st), nil
	}
	c := countAgents(st, sessions, a.now, a.cfg)
	k, u := a.resolveUsageForStart(ctx, liveCap, kind)
	return capacityStopped(capacity.NewStartCapacity(c, k, a.readHeadroom(ctx, k, a.watchSwap()), builds, kind).WithUsage(u, a.now).WithLocal(st, liveCap), a, st), nil
}

// keepSwap keeps the machine sample's swap reading for the poll and, in the
// state's side file, for capacity.
func (w *watcher) keepSwap(r *capacity.SwapReading) {
	w.mu.Lock()
	w.swap = r
	w.mu.Unlock()
	_ = w.a.store.WriteFile(swapFile, r)
}

// capacity says CAPACITY FULL once at a positive ceiling, with the headroom,
// and ends with one ENDED line when the count recovers; a ceiling of 0 clears
// it and stays silent. The machine is read only at that edge.
func (w *watcher) capacity(ctx context.Context, st *state.State, sessions []*claude.Session) {
	live := w.a.liveCapacity()
	k, u := w.currentUsage(live, w.a.now)
	c := countAgents(st, sessions, w.a.now, w.a.cfg)
	busy := len(c.Busy)
	// A configured capacity.ceiling of 0 keeps the line silent even when
	// the derived ceiling is positive (a self-hosted provider's own ceiling
	// rides in the derived total).
	w.clear("capacity-low")
	if (u != nil && u.Error != "") || k.Ceiling == 0 || live.Ceiling == 0 || busy < k.Ceiling || st.Stop != nil || startsHold(st, w.a.now) != nil {
		w.clear("capacity-full")
		return
	}
	w.mu.Lock()
	swap := w.swap
	w.mu.Unlock()
	var h *capacity.Headroom
	if w.readHeadroom != nil {
		h = w.readHeadroom(ctx, swap)
	} else {
		h = w.a.readHeadroom(ctx, k, swap)
	}
	// The watch weighs a start that does not build: a busy slot is a note.
	v := capacity.NewCapacity(c, k, h, false).WithUsage(u, w.a.now)
	w.check("capacity-full", busy >= k.Ceiling, "CAPACITY FULL %d of %d: %s", busy, k.Ceiling, v.Headroom.Line())
}
