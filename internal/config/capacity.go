package config

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultCeiling is capacity.ceiling when unset; an explicit 0 stays 0.
const DefaultCeiling = 10

// Defaults of capacity.usage and capacity.providers.<name> when a usage
// command is set.
const (
	DefaultUsageEvery                = 5 * time.Minute
	DefaultUsageReportEvery          = 30 * time.Minute
	DefaultUsageTimeout              = 90 * time.Second
	DefaultUsageStaleAfter           = 15 * time.Minute
	DefaultUsageHighPercent          = 95.0
	DefaultUsageUnderuseRatio        = 0.5
	DefaultUsageUnderuseMinPace      = 10.0
	DefaultUsageUnderuseMinRemaining = 24 * time.Hour
)

// DefaultClaudeUsageCommand reads plan windows without a model turn.
var DefaultClaudeUsageCommand = []string{ClaudeHarness, "-p", "/usage", "--output-format", "json"}

// DefaultAgyUsageCommand is capacity.providers.agy.command when unset, even
// with agy: {}: agy's local usage report, read without a model turn.
var DefaultAgyUsageCommand = []string{AgyHarness, "-p", "/usage"}

// DefaultCodexUsageCommand is capacity.providers.codex.command when unset:
// beekeeper probes account usage when local events age out and otherwise reads
// the freshest token_count rate_limits event across Codex
// rollout transcripts in-process.
var DefaultCodexUsageCommand = []string{CodexHarness, "usage"}

// KindSubscription and KindSelfHosted are capacity.providers.<name>.kind. A
// subscription provider has a usage reader and a pace; a self-hosted provider
// (a model served on the desk's own machines, such as an omp agent on a local
// model) has neither: only a ceiling of sessions at once, its own.
const (
	KindSubscription = "subscription"
	KindSelfHosted   = "self-hosted"
)

// The agent harness names, the subscription ones besides OmpHarness: a
// capacity.providers entry of one of these names is that subscription's
// provider, never a self-hosted one.
const (
	OmpHarness    = "omp"
	ClaudeHarness = "claude"
	AgyHarness    = "agy"
	CodexHarness  = "codex"
)

// DefaultSelfHostedCeiling is the ceiling of busy sessions a self-hosted
// provider runs with when it sets no ceiling of its own: the machine guards
// still bound every start, so an unset ceiling is generous rather than tight.
const DefaultSelfHostedCeiling = 10

// DefaultServeModeStale is how long a cached serving-mode reading stays fresh
// before capacity.providers.<name>.serveModeCommand runs again.
const DefaultServeModeStale = time.Minute

// DefaultServeModeSwitchTimeout is how long a serving-mode switch command may
// run before beekeeper ends it and leaves the mode as its reader reports it.
const DefaultServeModeSwitchTimeout = time.Minute

// ServeMode is one serving mode of a self-hosted provider: the machine's
// nodes it occupies while it runs, and the sessions of the model it sustains.
type ServeMode struct {
	// Nodes are the machine's nodes the mode occupies: a claim of one of
	// them is the generic constraint "needs node N exclusively", and the
	// model never runs in this mode while a constraint holds one. An
	// empty list occupies no node and no constraint can hold it back.
	Nodes []string `yaml:"nodes"`
	// Sessions is the ceiling of busy sessions the model serves while the
	// mode runs: what the provider's ceiling drops to during a drain into
	// this mode, and stands at once the switch left it.
	Sessions int `yaml:"sessions"`
}

// DefaultAgyGeminiBucket and DefaultAgyClaudeBucket are the usage bucket names
// `agy -p /usage` prints for Gemini and Claude/GPT models.
const (
	DefaultAgyGeminiBucket = "Gemini Models"
	DefaultAgyClaudeBucket = "Claude and GPT models"
)

// TaskPolicy constrains model choices for a kind of task. Classes and costs
// are desk-maintained relative values; no provider price is assumed.
type TaskPolicy struct {
	MinClass        int     `yaml:"minClass"`
	MaxCost         float64 `yaml:"maxCost"`
	DifferentFamily bool    `yaml:"differentFamily"`
}

// Capacity is the supervisor's ceiling and model selection policy.
// Any legacy floor key in YAML is ignored.
type Capacity struct {
	TaskPolicies map[string]TaskPolicy `yaml:"taskPolicies"`
	TaskKind     string                `yaml:"-"`
	AuthorModel  string                `yaml:"-"`
	AuthorFamily string                `yaml:"-"`
	// Ceiling is the most busy agents the supervisor starts (10). Zero is
	// honoured: ceiling 0 refuses every unforced start.
	Ceiling    int `yaml:"ceiling"`
	ceilingSet bool
	// ReviewHeadroom reserves extra subscription sessions for reviews. Zero disables it.
	ReviewHeadroom    int `yaml:"reviewHeadroom"`
	reviewHeadroomSet bool
	// AvailMinMiB is the MemAvailable a start needs (20 GiB), and the floor
	// under which disk swap's growth refuses a start.
	AvailMinMiB int `yaml:"availMinMiB"`
	// SwapGrowthMaxMiB is disk swap's growth per hour, over the watch's
	// readings of Watch.SwapWindow, that counts (256): a growth over it
	// refuses a start under memory pressure (PSI over SwapPSIMax, or
	// MemAvailable under AvailMinMiB) and is a warning without.
	SwapGrowthMaxMiB int `yaml:"swapGrowthMaxMiB"`
	// SwapPSIMax is the memory pressure ("full avg60" of
	// /proc/pressure/memory, in percent) up to which disk swap's growth is
	// a warning rather than a refusal (0: any pressure refuses).
	SwapPSIMax float64 `yaml:"swapPSIMax"`
	// Usage derives Ceiling from the subscription's 5-hour and 7-day usage
	// windows read by a configured command; unset, Ceiling stands as
	// configured.
	Usage Usage `yaml:"usage"`
	// Providers configures per-provider (e.g. claude, agy, codex) usage
	// readers, window thresholds and ceiling targets. Busy agents count
	// against the provider they run on, while the machine guards and
	// Capacity.Ceiling cap the total.
	Providers     map[string]Provider `yaml:"providers"`
	providerOrder []string
	// Choices is the ordered list of (harness, model) pairs that
	// `agents start --harness auto` and `beekeeper capacity` evaluate:
	// a start takes the eligible subscription furthest behind its 7-day
	// pace (with 7-day budget left per hour, lower configured cost, and
	// order breaking ties) and self-hosted choices as fallbacks. Each
	// provider must have room, its 5-hour window below its threshold and
	// its 7-day use within the weekly cap and, for a worker start, not
	// ahead of the even pace.
	Choices []Choice `yaml:"choices"`
	// ReviewSkipPace is read and ignored: review starts already skip the
	// 7-day pace check while worker starts keep it.
	ReviewSkipPace bool `yaml:"reviewSkipPace"`
	// Local bounds the busy agents the coordinator's own machine runs,
	// apart from Ceiling, which bounds the subscriptions across the desk
	// and its execution hosts.
	Local LocalCapacity `yaml:"local"`
}

// LocalCapacity is capacity.local: the coordinator machine's own cap.
type LocalCapacity struct {
	// Ceiling is the most busy agents this machine runs (capacity.local.
	// ceiling): every local start, the loop's and one by hand, worker,
	// reviewer and resume alike, is refused at it. Every busy agent whose
	// session runs here counts, a self-hosted provider's included; an
	// agent on an execution host counts against that host's maxBusy and
	// never against it. Unset: no local cap, capacity.ceiling and the
	// machine guards alone bound a local start. 0 refuses every unforced
	// local start.
	Ceiling *int `yaml:"ceiling"`
}

// Limit is capacity.local.ceiling and whether it is set.
func (l LocalCapacity) Limit() (int, bool) {
	if l.Ceiling == nil {
		return 0, false
	}
	return *l.Ceiling, true
}

// Provider configures one provider (such as claude, agy or codex): its own
// ceiling of busy agents and its usage reader and thresholds. A provider of
// Kind self-hosted (a model served on the desk's own machines) sets no usage
// reader: its ceiling alone bounds its busy sessions, and capacity.ceiling
// bounds only the subscription providers' total.
type Provider struct {
	// Kind is KindSubscription (the default: a usage reader and a pace) or
	// KindSelfHosted (no usage reader; a ceiling of sessions at once).
	Kind             string `yaml:"kind"`
	Ceiling          int    `yaml:"ceiling"`
	ceilingSet       bool
	ServeModeCommand []string `yaml:"serveModeCommand"`
	// ServeModeCeilings maps a serving mode read by ServeModeCommand to the
	// ceiling it stands for; a mode naming none falls back to Ceiling.
	ServeModeCeilings map[string]int `yaml:"serveModeCeilings"`
	// ServeModeModes is every serving mode the platform's tool can switch
	// the model into: the nodes each occupies and the sessions it sustains.
	// With it configured, a task or lease may claim one of the nodes
	// exclusively, and beekeeper keeps the model out of every mode that
	// occupies a held node (see ServeMode).
	ServeModeModes map[string]ServeMode `yaml:"serveModeModes"`
	// ServeModeSwitchCommand switches the model into the mode named as its
	// last argument: beekeeper relays to the platform's own tool and holds
	// no control of the model itself.
	ServeModeSwitch []string `yaml:"serveModeSwitchCommand"`
	// ServeModeSwitchTimeout is how long a switch may take before beekeeper
	// ends it and leaves the mode as its reader reports it (default 1m).
	ServeModeSwitchTimeout Duration `yaml:"serveModeSwitchTimeout"`
	// ServeModeStale is how long a cached serving-mode reading stays fresh
	// before ServeModeCommand runs again (default 1m).
	ServeModeStale Duration `yaml:"serveModeStaleAfter"`
	Usage          `yaml:",inline"`
}

// SelfHosted reports whether this provider is a model served on the desk's
// own machines: no usage reader, no pace, a ceiling of its own.
func (p Provider) SelfHosted() bool {
	return strings.EqualFold(strings.TrimSpace(p.Kind), KindSelfHosted)
}

// HasServeMode reports whether this provider's ceiling follows a serving
// mode read by serveModeCommand.
func (p Provider) HasServeMode() bool {
	return p.SelfHosted() && len(p.ServeModeCommand) > 0
}

// EffectiveServeModeStale is how long a cached serving-mode reading stays
// fresh before beekeeper runs serveModeCommand again (default 1m).
func (p Provider) EffectiveServeModeStale() time.Duration {
	if p.ServeModeStale.Duration > 0 {
		return p.ServeModeStale.Duration
	}
	return DefaultServeModeStale
}

// ServeModeCeiling is the ceiling the serving mode mode stands for, and
// whether the provider names one for it.
func (p Provider) ServeModeCeiling(mode string) (int, bool) {
	if m, ok := p.serveMode(mode); ok {
		return m.Sessions, true
	}
	if c, ok := p.ServeModeCeilings[mode]; ok {
		return c, true
	}
	for k, c := range p.ServeModeCeilings {
		if strings.EqualFold(k, mode) {
			return c, true
		}
	}
	return 0, false
}

// HasServeModeSwitch reports whether the provider can be switched into
// another serving mode: the platform's own switch command, and the modes it
// knows to switch between.
func (p Provider) HasServeModeSwitch() bool {
	return p.HasServeMode() && len(p.ServeModeSwitch) > 0 && len(p.ServeModeModes) > 0
}

// EffectiveServeModeSwitchTimeout is how long a switch of this provider's
// serving mode may take (default 1m).
func (p Provider) EffectiveServeModeSwitchTimeout() time.Duration {
	if p.ServeModeSwitchTimeout.Duration > 0 {
		return p.ServeModeSwitchTimeout.Duration
	}
	return DefaultServeModeSwitchTimeout
}

// serveMode is the mode named mode, matched case-insensitively, and whether
// the provider knows it.
func (p Provider) serveMode(mode string) (ServeMode, bool) {
	if m, ok := p.ServeModeModes[mode]; ok {
		return m, true
	}
	for k, m := range p.ServeModeModes {
		if strings.EqualFold(k, mode) {
			return m, true
		}
	}
	return ServeMode{}, false
}

// ServeModeNodes are the machine's nodes the serving mode mode occupies, and
// whether the provider knows the mode.
func (p Provider) ServeModeNodes(mode string) ([]string, bool) {
	m, ok := p.serveMode(mode)
	return m.Nodes, ok
}

// ServeModeReading is one read of a self-hosted provider's serving mode: when
// it was read, the mode the command answered with, and why the read failed, if
// it did. The ceiling the mode names is the provider's, resolved by
// ServeModeCeiling.
type ServeModeReading struct {
	At   time.Time `json:"at,omitzero"`
	Mode string    `json:"mode,omitempty"`
	Err  string    `json:"error,omitempty"`
}

// WithServeMode returns the provider with r applied: the ceiling the mode it
// names, falling back to Ceiling while the mode names none or the read failed.
func (p Provider) WithServeMode(r ServeModeReading) Provider {
	if r.Mode != "" && r.Err == "" {
		if c, ok := p.ServeModeCeiling(r.Mode); ok {
			p.Ceiling = c
		}
	}
	return p
}

// Choice is one (harness, model) candidate in Capacity.Choices for
// `agents start --harness auto` and `beekeeper capacity`.
type Choice struct {
	// Effort is the session default when this capacity choice is selected.
	Effort string `yaml:"effort,omitempty" json:"effort,omitempty"`
	// Kinds limits this choice to task kinds; empty permits all kinds, but
	// a choice on a self-hosted provider takes task kind worker (code
	// changes) only when Kinds lists it.
	Kinds  []string `yaml:"kinds,omitempty" json:"kinds,omitempty"`
	Class  int      `yaml:"class,omitempty" json:"class,omitempty"`
	Cost   float64  `yaml:"cost,omitempty" json:"cost,omitempty"`
	Family string   `yaml:"family,omitempty" json:"family,omitempty"`
	// ReviewReserve keeps this percent of the weekly window for reviews.
	ReviewReserve float64 `yaml:"reviewReserve,omitempty" json:"reviewReserve,omitempty"`
	// Harness is the agent harness to start ("claude", "agy", "codex", "omp").
	Harness string `yaml:"harness" json:"harness"`
	// Model is the model selector passed to the harness.
	Model string `yaml:"model" json:"model"`
	// Provider names the entry in Capacity.Providers whose room and windows
	// govern this choice (default: Harness, or "claude" when Harness is "").
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`
	// Bucket selects the named usage bucket within Provider when the
	// provider reports multiple buckets (such as `agy -p /usage`'s
	// "Gemini Models" and "Claude and GPT models"). Empty: inferred from
	// Provider.Buckets or the model name.
	Bucket string `yaml:"bucket,omitempty" json:"bucket,omitempty"`
	// FiveHourMax overrides Provider.FiveHourMax for this choice when set:
	// the choice requires fiveHour.usedPercent < FiveHourMax.
	FiveHourMax    float64 `yaml:"fiveHourMax,omitempty" json:"fiveHourMax,omitempty"`
	fiveHourMaxSet bool
	// SevenDayMax is the weekly usedPercent cap of every start on this
	// choice: a start requires sevenDay.usedPercent < SevenDayMax.
	SevenDayMax    float64 `yaml:"sevenDayMax,omitempty" json:"sevenDayMax,omitempty"`
	sevenDayMaxSet bool
	// PaceMargin is how many percentage points sevenDay.usedPercent must
	// stay under the 7-day window's elapsed share (pace) to allow this
	// choice, as a negative number (default: Provider.PaceMargin, else 0);
	// a positive margin counts as 0: no start runs ahead of the pace.
	PaceMargin    float64 `yaml:"paceMargin,omitempty" json:"paceMargin,omitempty"`
	paceMarginSet bool
	// SkipPace is read and ignored: no start runs ahead of the pace.
	SkipPace bool `yaml:"skipPace,omitempty" json:"skipPace,omitempty"`
}

// EffectiveProvider returns the provider name in Capacity.Providers that
// governs this choice.
func (c Choice) EffectiveProvider() string {
	return cmp.Or(strings.TrimSpace(c.Provider), strings.TrimSpace(c.Harness), ClaudeHarness)
}

// EffectiveBucket returns the usage bucket name for this choice on p.
func (c Choice) EffectiveBucket(p Provider) string {
	if b := strings.TrimSpace(c.Bucket); b != "" {
		return b
	}
	return p.BucketFor(c.Model)
}

// EffectiveFiveHourMax returns the 5-hour usedPercent stop threshold for this
// choice on p, and whether one is configured.
func (c Choice) EffectiveFiveHourMax(p Provider) (float64, bool) {
	if c.fiveHourMaxSet || c.FiveHourMax > 0 {
		return c.FiveHourMax, true
	}
	if p.fiveHourMaxSet || p.FiveHourMax > 0 {
		return p.FiveHourMax, true
	}
	return 0, false
}

// EffectiveWorkerSevenDayMax returns the choice's worker-specific 7-day
// usedPercent cap, and whether one is configured.
func (c Choice) EffectiveWorkerSevenDayMax() (float64, bool) {
	if c.sevenDayMaxSet || c.SevenDayMax > 0 {
		return c.SevenDayMax, true
	}
	return 0, false
}

// EffectivePaceMargin returns the percentage-point margin by which 7-day usage
// may exceed the even weekly pace for this choice on p.
func (c Choice) EffectivePaceMargin(p Provider) float64 {
	if c.paceMarginSet || c.PaceMargin != 0 {
		return c.PaceMargin
	}
	if p.paceMarginSet || p.PaceMargin != 0 {
		return p.PaceMargin
	}
	return 0
}

// UnmarshalYAML records whether fiveHourMax, sevenDayMax and paceMargin were set.
func (c *Choice) UnmarshalYAML(n *yaml.Node) error {
	type plain Choice
	if err := n.Decode((*plain)(c)); err != nil {
		return err
	}
	var set struct {
		FiveHourMax *float64 `yaml:"fiveHourMax"`
		SevenDayMax *float64 `yaml:"sevenDayMax"`
		PaceMargin  *float64 `yaml:"paceMargin"`
	}
	if err := n.Decode(&set); err != nil {
		return err
	}
	c.fiveHourMaxSet = set.FiveHourMax != nil
	c.sevenDayMaxSet = set.SevenDayMax != nil
	c.paceMarginSet = set.PaceMargin != nil
	return nil
}

// Usage configures the opt-in subscription usage reader of Capacity or a Provider.
type Usage struct {
	// Command prints the current usage windows on stdout (JSON fiveHour
	// and/or sevenDay, or `agy -p /usage` tab-separated lines); empty,
	// beekeeper reads no subscription usage.
	Command []string `yaml:"command"`
	// Every is how often a cached reading stays fresh before beekeeper
	// runs Command again (default 5m; a window past its resetsAt re-reads
	// at once).
	Every Duration `yaml:"every"`
	// Timeout bounds one run of Command (default 90s).
	Timeout Duration `yaml:"timeout"`
	// StaleAfter is how old a usage reading may grow before it counts as
	// stale, yielding ceiling 0 and room 0 for that provider (default 15m).
	StaleAfter Duration `yaml:"staleAfter"`
	// ReportEvery is how often the watch prints a USAGE line (default 30m;
	// negative: never, watch --once aside).
	ReportEvery    Duration `yaml:"reportEvery"`
	reportEverySet bool
	// HighPercent is the 5-hour window's usedPercent from which USAGE HIGH
	// holds until fiveHour.resetsAt: unforced starts, resumes and
	// auto-resume wait, and each busy worker is told once to finish its
	// step and park (default 95).
	HighPercent    float64 `yaml:"highPercent"`
	highPercentSet bool
	// FiveHourMax and SevenDayMax are hard usedPercent stops for a window:
	// at or past the threshold the window yields ceiling 0.
	FiveHourMax    float64 `yaml:"fiveHourMax"`
	fiveHourMaxSet bool
	SevenDayMax    float64 `yaml:"sevenDayMax"`
	sevenDayMaxSet bool
	// PaceMargin is the default percentage-point margin by which 7-day
	// usedPercent must stay under the even weekly pace, as a negative number
	// (default 0); a positive margin counts as 0.
	PaceMargin    float64 `yaml:"paceMargin"`
	paceMarginSet bool
	// UnderuseRatio is the fraction of the 7-day window's even pace below
	// which usage counts as heavily underused (sevenDay.usedPercent <
	// underuseRatio * sevenDay.pace) once sevenDay.pace reaches
	// UnderuseMinPace and more than UnderuseMinRemaining remains before
	// sevenDay.resetsAt (default 0.5; 0 or negative disables).
	UnderuseRatio    float64 `yaml:"underuseRatio"`
	underuseRatioSet bool
	// UnderuseMinPace is the minimum 7-day pace percent before underuse is
	// checked (default 10).
	UnderuseMinPace    float64 `yaml:"underuseMinPace"`
	underuseMinPaceSet bool
	// UnderuseMinRemaining is the minimum time remaining before
	// sevenDay.resetsAt for underuse to fire (default 24h).
	UnderuseMinRemaining    Duration `yaml:"underuseMinRemaining"`
	underuseMinRemainingSet bool
	// FiveHour and SevenDay map a window's usedPercent to a ceiling: the
	// first band with usedPercent < upTo applies, and at or past the last
	// band's upTo the window yields 0. When both windows have bands, the
	// lower ceiling applies.
	FiveHour []UsageBand `yaml:"fiveHour"`
	SevenDay []UsageBand `yaml:"sevenDay"`
	// PaceCaps lower Ceiling when the 7-day window's usedPercent exceeds the
	// even weekly pace by at least OverBy percentage points.
	PaceCaps []PaceCap `yaml:"paceCaps"`
	// Buckets maps a model name or prefix to a bucket name in multi-bucket
	// usage output (such as `agy -p /usage`).
	Buckets map[string]string `yaml:"buckets"`
}

// UsageBand is one usage band of a subscription window: while the window's
// usedPercent is below UpTo, its target is Ceiling.
type UsageBand struct {
	UpTo       float64 `yaml:"upTo"`
	Ceiling    int     `yaml:"ceiling"`
	ceilingSet bool
}

// PaceCap caps Ceiling when the 7-day window's usedPercent exceeds the even
// weekly pace by at least OverBy percentage points.
type PaceCap struct {
	OverBy     float64 `yaml:"overBy"`
	Ceiling    int     `yaml:"ceiling"`
	ceilingSet bool
}

// Enabled reports whether a subscription usage command is configured.
func (u Usage) Enabled() bool { return len(u.Command) > 0 }

// HighPercentSet reports whether highPercent was explicitly set.
func (u Usage) HighPercentSet() bool { return u.highPercentSet }

// ReportEverySet reports whether reportEvery was explicitly set.
func (u Usage) ReportEverySet() bool { return u.reportEverySet }

// EffectiveEvery is Every, or DefaultUsageEvery when unset.
func (u Usage) EffectiveEvery() time.Duration {
	if u.Every.Duration > 0 {
		return u.Every.Duration
	}
	return DefaultUsageEvery
}

// EffectiveTimeout is Timeout, or DefaultUsageTimeout when unset.
func (u Usage) EffectiveTimeout() time.Duration {
	if u.Timeout.Duration > 0 {
		return u.Timeout.Duration
	}
	return DefaultUsageTimeout
}

// EffectiveStaleAfter is StaleAfter, or max(DefaultUsageStaleAfter, 3*EffectiveEvery()) when unset.
func (u Usage) EffectiveStaleAfter() time.Duration {
	if u.StaleAfter.Duration > 0 {
		return u.StaleAfter.Duration
	}
	return max(DefaultUsageStaleAfter, 3*u.EffectiveEvery())
}

// EffectiveHighPercent is HighPercent, or DefaultUsageHighPercent when unset.
func (u Usage) EffectiveHighPercent() float64 {
	if u.HighPercent > 0 {
		return u.HighPercent
	}
	return DefaultUsageHighPercent
}

// EffectiveUnderuseRatio is UnderuseRatio when explicitly set (clamped to >=0,
// so 0 or negative disables underuse detection), or DefaultUsageUnderuseRatio
// when unset.
func (u Usage) EffectiveUnderuseRatio() float64 {
	if u.underuseRatioSet {
		return max(0, u.UnderuseRatio)
	}
	return DefaultUsageUnderuseRatio
}

// EffectiveUnderuseMinPace is UnderuseMinPace when explicitly set (clamped to
// >=0), or DefaultUsageUnderuseMinPace when unset.
func (u Usage) EffectiveUnderuseMinPace() float64 {
	if u.underuseMinPaceSet {
		return max(0, u.UnderuseMinPace)
	}
	return DefaultUsageUnderuseMinPace
}

// EffectiveUnderuseMinRemaining is UnderuseMinRemaining when explicitly set
// (clamped to >=0), or DefaultUsageUnderuseMinRemaining when unset.
func (u Usage) EffectiveUnderuseMinRemaining() time.Duration {
	if u.underuseMinRemainingSet {
		return max(0, u.UnderuseMinRemaining.Duration)
	}
	return DefaultUsageUnderuseMinRemaining
}

// BucketFor returns the usage bucket name for model: an exact or prefix match
// in u.Buckets if configured, else "Gemini Models" for gemini* models and
// "Claude and GPT models" otherwise.
func (u Usage) BucketFor(model string) string {
	m := strings.TrimSpace(model)
	if len(u.Buckets) > 0 {
		if b, ok := u.Buckets[m]; ok {
			return b
		}
		lower := strings.ToLower(m)
		for _, k := range slices.Sorted(maps.Keys(u.Buckets)) {
			prefix := strings.TrimSuffix(strings.ToLower(k), "*")
			if prefix != "" && strings.HasPrefix(lower, prefix) {
				return u.Buckets[k]
			}
		}
	}
	if strings.HasPrefix(strings.ToLower(m), "gemini") {
		return DefaultAgyGeminiBucket
	}
	return DefaultAgyClaudeBucket
}

func (u Usage) hasFields() bool {
	return u.Every.Duration != 0 || u.Timeout.Duration != 0 || u.StaleAfter.Duration != 0 ||
		u.ReportEvery.Duration != 0 || u.highPercentSet || u.fiveHourMaxSet ||
		u.sevenDayMaxSet || u.paceMarginSet || u.underuseRatioSet ||
		u.underuseMinPaceSet || u.underuseMinRemainingSet || len(u.FiveHour) > 0 ||
		len(u.SevenDay) > 0 || len(u.PaceCaps) > 0 || len(u.Buckets) > 0
}

// UnmarshalYAML records whether highPercent, fiveHourMax, sevenDayMax,
// paceMargin, underuseRatio, underuseMinPace, underuseMinRemaining and
// reportEvery were explicitly set.
func (u *Usage) UnmarshalYAML(n *yaml.Node) error {
	type plain Usage
	if err := n.Decode((*plain)(u)); err != nil {
		return err
	}
	var set struct {
		ReportEvery          *Duration `yaml:"reportEvery"`
		HighPercent          *float64  `yaml:"highPercent"`
		FiveHourMax          *float64  `yaml:"fiveHourMax"`
		SevenDayMax          *float64  `yaml:"sevenDayMax"`
		PaceMargin           *float64  `yaml:"paceMargin"`
		UnderuseRatio        *float64  `yaml:"underuseRatio"`
		UnderuseMinPace      *float64  `yaml:"underuseMinPace"`
		UnderuseMinRemaining *Duration `yaml:"underuseMinRemaining"`
	}
	if err := n.Decode(&set); err != nil {
		return err
	}
	u.reportEverySet = set.ReportEvery != nil
	u.highPercentSet = set.HighPercent != nil
	u.fiveHourMaxSet = set.FiveHourMax != nil
	u.sevenDayMaxSet = set.SevenDayMax != nil
	u.paceMarginSet = set.PaceMargin != nil
	u.underuseRatioSet = set.UnderuseRatio != nil
	u.underuseMinPaceSet = set.UnderuseMinPace != nil
	u.underuseMinRemainingSet = set.UnderuseMinRemaining != nil
	return nil
}

// UnmarshalYAML records whether ceiling was explicitly set on the band.
func (b *UsageBand) UnmarshalYAML(n *yaml.Node) error {
	type plain UsageBand
	if err := n.Decode((*plain)(b)); err != nil {
		return err
	}
	var set struct {
		Ceiling *int `yaml:"ceiling"`
	}
	if err := n.Decode(&set); err != nil {
		return err
	}
	b.ceilingSet = set.Ceiling != nil
	return nil
}

// UnmarshalYAML records whether ceiling was explicitly set on the pace cap.
func (p *PaceCap) UnmarshalYAML(n *yaml.Node) error {
	type plain PaceCap
	if err := n.Decode((*plain)(p)); err != nil {
		return err
	}
	var set struct {
		Ceiling *int `yaml:"ceiling"`
	}
	if err := n.Decode(&set); err != nil {
		return err
	}
	p.ceilingSet = set.Ceiling != nil
	return nil
}

// UnmarshalYAML decodes kind, ceiling and either inline or nested usage
// fields on a provider.
func (p *Provider) UnmarshalYAML(n *yaml.Node) error {
	var fc struct {
		Kind             *string              `yaml:"kind"`
		Ceiling          *int                 `yaml:"ceiling"`
		Usage            *Usage               `yaml:"usage"`
		ServeModeStale   *Duration            `yaml:"serveModeStaleAfter"`
		ServeModeCmd     *[]string            `yaml:"serveModeCommand"`
		ServeModeCeiling map[string]int       `yaml:"serveModeCeilings"`
		ServeModeModes   map[string]ServeMode `yaml:"serveModeModes"`
		ServeModeSwitch  *[]string            `yaml:"serveModeSwitchCommand"`
		ServeModeSwitchT *Duration            `yaml:"serveModeSwitchTimeout"`
	}
	if err := n.Decode(&fc); err != nil {
		return err
	}
	if err := p.Usage.UnmarshalYAML(n); err != nil {
		return err
	}
	if fc.Usage != nil {
		p.Usage = *fc.Usage
	}
	if fc.Kind != nil {
		p.Kind = *fc.Kind
	}
	if fc.ServeModeStale != nil {
		p.ServeModeStale = *fc.ServeModeStale
	}
	if fc.ServeModeCmd != nil {
		p.ServeModeCommand = *fc.ServeModeCmd
	}
	if fc.ServeModeCeiling != nil {
		p.ServeModeCeilings = fc.ServeModeCeiling
	}
	if fc.ServeModeModes != nil {
		p.ServeModeModes = fc.ServeModeModes
	}
	if fc.ServeModeSwitch != nil {
		p.ServeModeSwitch = *fc.ServeModeSwitch
	}
	if fc.ServeModeSwitchT != nil {
		p.ServeModeSwitchTimeout = *fc.ServeModeSwitchT
	}
	if fc.Ceiling != nil {
		p.Ceiling, p.ceilingSet = *fc.Ceiling, true
	}
	return nil
}

func (u *Usage) defaults(home string) {
	if !u.Enabled() {
		return
	}
	for i := range u.Command {
		u.Command[i] = homePath(home, u.Command[i])
	}
	setDur(&u.Every, DefaultUsageEvery)
	setDur(&u.Timeout, DefaultUsageTimeout)
	setDur(&u.StaleAfter, max(DefaultUsageStaleAfter, 3*u.Every.Duration))
	setDur(&u.ReportEvery, DefaultUsageReportEvery)
	if !u.highPercentSet {
		u.HighPercent = DefaultUsageHighPercent
	}
	if !u.underuseRatioSet {
		u.UnderuseRatio = DefaultUsageUnderuseRatio
	}
	if !u.underuseMinPaceSet {
		u.UnderuseMinPace = DefaultUsageUnderuseMinPace
	}
	if !u.underuseMinRemainingSet {
		u.UnderuseMinRemaining = Duration{Duration: DefaultUsageUnderuseMinRemaining}
	}
}

func (u Usage) validateNamedEach(prefix string, report func(error)) {
	if !u.Enabled() {
		if u.hasFields() {
			report(fmt.Errorf("%s: no command: set %s.command, or nothing of %s", prefix, prefix, prefix))
		}
		return
	}
	if strings.TrimSpace(u.Command[0]) == "" {
		report(fmt.Errorf("%s.command[0]: empty executable", prefix))
	}
	if u.Every.Duration <= 0 {
		report(fmt.Errorf("%s.every: %s is not positive", prefix, u.Every.Duration))
	}
	if u.Timeout.Duration <= 0 {
		report(fmt.Errorf("%s.timeout: %s is not positive", prefix, u.Timeout.Duration))
	}
	if u.StaleAfter.Duration < 0 {
		report(fmt.Errorf("%s.staleAfter: %s is negative", prefix, u.StaleAfter.Duration))
	}
	if !(u.HighPercent > 0 && u.HighPercent <= 100) {
		report(fmt.Errorf("%s.highPercent: %g is not in (0, 100]", prefix, u.HighPercent))
	}
	if u.fiveHourMaxSet && !(u.FiveHourMax > 0 && u.FiveHourMax <= 100) {
		report(fmt.Errorf("%s.fiveHourMax: %g is not in (0, 100]", prefix, u.FiveHourMax))
	}
	if u.sevenDayMaxSet && !(u.SevenDayMax > 0 && u.SevenDayMax <= 100) {
		report(fmt.Errorf("%s.sevenDayMax: %g is not in (0, 100]", prefix, u.SevenDayMax))
	}
	if u.paceMarginSet && (u.PaceMargin < -100 || u.PaceMargin > 100) {
		report(fmt.Errorf("%s.paceMargin: %g is not in [-100, 100]", prefix, u.PaceMargin))
	}
	if u.underuseRatioSet && u.UnderuseRatio > 1 {
		report(fmt.Errorf("%s.underuseRatio: %g is above 1", prefix, u.UnderuseRatio))
	}
	if u.underuseMinPaceSet && (u.UnderuseMinPace < 0 || u.UnderuseMinPace > 100) {
		report(fmt.Errorf("%s.underuseMinPace: %g is not in [0, 100]", prefix, u.UnderuseMinPace))
	}
	if u.underuseMinRemainingSet && u.UnderuseMinRemaining.Duration < 0 {
		report(fmt.Errorf("%s.underuseMinRemaining: %s is negative", prefix, u.UnderuseMinRemaining.Duration))
	}
	for _, w := range []struct {
		name  string
		bands []UsageBand
	}{
		{prefix + ".fiveHour", u.FiveHour},
		{prefix + ".sevenDay", u.SevenDay},
	} {
		prev := 0.0
		for i, b := range w.bands {
			switch {
			case !(b.UpTo > 0 && b.UpTo <= 100):
				report(fmt.Errorf("%s[%d]: upTo %g is not in (0, 100]", w.name, i, b.UpTo))
			case b.UpTo <= prev:
				report(fmt.Errorf("%s[%d]: upTo %g is not above %g: list bands in ascending order", w.name, i, b.UpTo, prev))
			case !b.ceilingSet:
				report(fmt.Errorf("%s[%d]: ceiling is required", w.name, i))
			case b.Ceiling < 0:
				report(fmt.Errorf("%s[%d]: ceiling %d is negative", w.name, i, b.Ceiling))
			}
			prev = b.UpTo
		}
	}
	prevOver := 0.0
	for i, pc := range u.PaceCaps {
		name := prefix + ".paceCaps"
		switch {
		case !(pc.OverBy > 0 && pc.OverBy <= 100):
			report(fmt.Errorf("%s[%d]: overBy %g is not in (0, 100]", name, i, pc.OverBy))
		case pc.OverBy <= prevOver:
			report(fmt.Errorf("%s[%d]: overBy %g is not above %g: list paceCaps in ascending order", name, i, pc.OverBy, prevOver))
		case !pc.ceilingSet:
			report(fmt.Errorf("%s[%d]: ceiling is required", name, i))
		case pc.Ceiling < 0:
			report(fmt.Errorf("%s[%d]: ceiling %d is negative", name, i, pc.Ceiling))
		}
		prevOver = pc.OverBy
	}
}

// Derive returns Capacity with Ceiling derived from the subscription usage
// windows' effective usedPercent values (nil when a window was not reported)
// and reports whether USAGE HIGH holds on the 5-hour window.
func (k Capacity) Derive(fiveHour, sevenDay *float64) (derived Capacity, high bool) {
	return k.DeriveWithPace(fiveHour, sevenDay, nil)
}

// DeriveWithPace returns Capacity with Ceiling derived from the 5-hour and
// 7-day windows and the 7-day even pace.
func (k Capacity) DeriveWithPace(fiveHour, sevenDay, pace *float64) (derived Capacity, high bool) {
	derived = k
	if !k.Usage.Enabled() {
		return derived, false
	}
	p := Provider{
		Ceiling:    k.Ceiling,
		ceilingSet: k.ceilingSet,
		Usage:      k.Usage,
	}
	dp, high := p.DeriveWithPace(fiveHour, sevenDay, pace)
	derived.Ceiling = dp.Ceiling
	return derived, high
}

// Derive returns Provider with Ceiling derived from the subscription usage
// windows' effective usedPercent values and reports whether USAGE HIGH holds on
// the 5-hour window.
func (p Provider) Derive(fiveHour, sevenDay *float64) (derived Provider, high bool) {
	return p.DeriveWithPace(fiveHour, sevenDay, nil)
}

// DeriveWithPace returns Provider with Ceiling derived from the 5-hour and
// 7-day windows and the 7-day even pace.
func (p Provider) DeriveWithPace(fiveHour, sevenDay, pace *float64) (derived Provider, high bool) {
	derived = p
	if !p.Enabled() {
		return derived, false
	}
	ceiling, banded := p.Ceiling, false
	apply := func(pct *float64, bands []UsageBand) {
		if pct == nil || len(bands) == 0 {
			return
		}
		bc := matchBand(*pct, bands)
		if !banded {
			ceiling, banded = bc, true
			return
		}
		ceiling = min(ceiling, bc)
	}
	apply(fiveHour, p.FiveHour)
	apply(sevenDay, p.SevenDay)
	if banded && p.ceilingSet {
		ceiling = min(ceiling, p.Ceiling)
	}
	if sevenDay != nil && pace != nil && len(p.PaceCaps) > 0 {
		over := *sevenDay - *pace
		for _, pc := range p.PaceCaps {
			if over >= pc.OverBy-1e-9 {
				ceiling = min(ceiling, pc.Ceiling)
			}
		}
	}
	if (p.fiveHourMaxSet || p.FiveHourMax > 0) && fiveHour != nil && *fiveHour >= p.FiveHourMax {
		ceiling = 0
	}
	if fiveHour != nil && *fiveHour >= 100 {
		ceiling = 0
	}
	if (p.sevenDayMaxSet || p.SevenDayMax > 0) && sevenDay != nil && *sevenDay >= p.SevenDayMax {
		ceiling = 0
	}
	if sevenDay != nil && *sevenDay >= 100 {
		ceiling = 0
	}
	if fiveHour != nil && *fiveHour >= p.EffectiveHighPercent() {
		derived.Ceiling = 0
		return derived, true
	}
	derived.Ceiling = ceiling
	return derived, false
}

func matchBand(used float64, bands []UsageBand) (ceiling int) {
	for _, b := range bands {
		if used < b.UpTo {
			return b.Ceiling
		}
	}
	return 0
}

// ProviderNames returns the configured providers in their YAML declaration
// order (or "claude" first, then sorted keys when constructed in Go).
func (k Capacity) ProviderNames() []string {
	if len(k.Providers) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(k.Providers))
	var out []string
	for _, name := range k.providerOrder {
		if _, ok := k.Providers[name]; ok && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	if _, ok := k.Providers[ClaudeHarness]; ok && !seen[ClaudeHarness] {
		seen[ClaudeHarness] = true
		out = append(out, ClaudeHarness)
	}
	for _, name := range slices.Sorted(maps.Keys(k.Providers)) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// UnmarshalYAML records whether ceiling and reviewHeadroom were set, and
// preserves the YAML key order of providers.
func (k *Capacity) UnmarshalYAML(n *yaml.Node) error {
	type plain Capacity
	if err := n.Decode((*plain)(k)); err != nil {
		return err
	}
	var set struct {
		Ceiling        *int `yaml:"ceiling"`
		ReviewHeadroom *int `yaml:"reviewHeadroom"`
	}
	if err := n.Decode(&set); err != nil {
		return err
	}
	k.ceilingSet = set.Ceiling != nil
	k.reviewHeadroomSet = set.ReviewHeadroom != nil
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == "providers" && n.Content[i+1].Kind == yaml.MappingNode {
			pn := n.Content[i+1]
			for j := 0; j+1 < len(pn.Content); j += 2 {
				k.providerOrder = append(k.providerOrder, pn.Content[j].Value)
			}
		}
	}
	return nil
}

// serveModeNodeNames are the machine's nodes named by any self-hosted
// provider's serving modes: the resources a claim of one of them is the
// generic constraint "needs node N exclusively". A node a mode occupies is
// leasable even where resources does not list it.
func (c Capacity) serveModeNodeNames() []string {
	set := map[string]bool{}
	for _, p := range c.Providers {
		if !p.SelfHosted() {
			continue
		}
		for _, m := range p.ServeModeModes {
			for _, n := range m.Nodes {
				set[n] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(set))
}
