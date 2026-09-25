// Package config loads beekeeper's machine configuration: where its state
// lives, which shared resources sessions lease, the GitHub budget floor, the
// thresholds of the machine watch and which installations' alerts it reads.
//
// The file is $XDG_CONFIG_HOME/beekeeper/config.yaml (or --config, or
// $BEEKEEPER_CONFIG). Every field is optional; a missing file is the
// defaults. The defaults are the numbers proven on an 86 GiB workstation
// running a Claude Desktop scope capped at 48 GiB: tune them to the machine.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/notify"
)

// Browser is the resource every machine has: the one Chrome the Claude in
// Chrome extension drives.
const Browser = "browser"

// Config is the parsed configuration with the defaults applied.
type Config struct {
	// StateDir holds state.json, events.jsonl and the last snapshot.
	StateDir string `yaml:"stateDir"`
	// LeaseDir holds one directory per held lease (mkdir is the lock).
	LeaseDir string `yaml:"leaseDir"`
	// Resources are the environments sessions lease besides the browser:
	// kind labs and shared installations.
	Resources []string `yaml:"resources"`
	// GrantTTL is how long a grant stays claimable once its resource is free.
	GrantTTL Duration `yaml:"grantTTL"`

	GitHub   GitHub   `yaml:"github"`
	Watch    Watch    `yaml:"watch"`
	Overlaps Overlaps `yaml:"overlaps"`
	Claude   Claude   `yaml:"claude"`
	Memcap   Memcap   `yaml:"memcap"`
	Lanes    []Lane   `yaml:"lanes"`
	Merge    Merge    `yaml:"merge"`
	Alerts   Alerts   `yaml:"alerts"`
	Notify   Notify   `yaml:"notify"`
	Metrics  Metrics  `yaml:"metrics"`
	// Supervisor is what `handover --prompt` tells a successor supervisor,
	// how long a relay to it stays open and how long a shift lasts.
	Supervisor Supervisor `yaml:"supervisor"`
}

// Supervisor configures the successor's session prompt (the instructions it
// follows, a skill or a file, not both, and the scope it supervises), the
// shift and the relay.
type Supervisor struct {
	// Skill is the name of the skill the successor runs (supervise).
	Skill string `yaml:"skill"`
	// Instructions is a file (~/ allowed) whose content opens the prompt
	// instead.
	Instructions string `yaml:"instructions"`
	// Scope says what the supervisor watches, in a sentence or two; empty,
	// the prompt names the resources, lanes and installations configured.
	Scope string `yaml:"scope"`
	// Shift is how long a supervisor serves before the watch reports the
	// relay due at a quiet moment; zero never reports it.
	Shift Duration `yaml:"shift"`
	// RelayTTL is how long a relay stays open for the successor's start.
	RelayTTL Duration `yaml:"relayTTL"`
	// RestartGrace is how long the grant rule holds after beekeeper first
	// saw the supervisor's CLI gone: a CLI back under the same session
	// within it is a restart and keeps the role; past it, the rule lifts.
	RestartGrace Duration `yaml:"restartGrace"`
}

// Lane is a set of repositories whose merges roll the same components of an
// installation: one merge at a time, the next once they rolled. A repository
// in no lane is a lane of its own, with no installation to wait for.
type Lane struct {
	Name string `yaml:"name"`
	// Repositories are owner/repo; a bare name matches it under any owner.
	// A repository's name is also the chart name of its HelmReleases.
	Repositories []string `yaml:"repositories"`
	// Installation is where the lane's HelmReleases run; empty: none to wait for.
	Installation string `yaml:"installation"`
	// Context is the kubeconfig context of the installation (default: the
	// one named after it, or ending in -<installation>).
	Context string `yaml:"context"`
}

// Merge configures the gate on devctl pr merge.
type Merge struct {
	// Cap is the most devctl processes the machine runs when a merge starts.
	Cap int `yaml:"cap"`
	// QueueTTL is how long a queued merge keeps its place after its run ended,
	// and how long after it the place holds up the merges behind it.
	QueueTTL Duration `yaml:"queueTTL"`
	// SeedTTL is how long a place queued on a session's behalf is kept from
	// its seeding or the session's last arrival, and a failed run's from the
	// failure.
	SeedTTL Duration `yaml:"seedTTL"`
	// Settle is how long a lane waits after a merge whose release is unknown.
	Settle Duration `yaml:"settle"`
	// SettleTimeout is how long a lane waits for a release to roll before
	// the next merge is refused.
	SettleTimeout Duration `yaml:"settleTimeout"`
	// BudgetFresh is how old the last budget reading may be to gate on.
	BudgetFresh Duration `yaml:"budgetFresh"`
	// StallAfter is how long a lane's first arrived merge may wait behind
	// places whose merges are not in the gate before the lane is stalled.
	StallAfter Duration `yaml:"stallAfter"`
}

// Notify configures what `watch --notify` sends to the desktop.
type Notify struct {
	// Kinds are the kinds that notify (notify.Kinds, the default: due,
	// oom-line, oom-kill, budget, stale-lease, no-supervisor).
	Kinds []string `yaml:"kinds"`
	// QuietHours ("22:00-07:00", local time) hold every notification but a
	// critical one until they end.
	QuietHours string `yaml:"quietHours"`
	// Urgency is a kind's urgency (low, normal, critical); oom-line and
	// oom-kill are critical, the others normal.
	Urgency map[string]string `yaml:"urgency"`
	// Repeat is how often a lasting condition (oom-line, budget) notifies
	// again while it lasts.
	Repeat Duration `yaml:"repeat"`
}

// Policy is the notify package's view of the section.
func (n Notify) Policy() notify.Policy {
	q, _ := notify.ParseQuietHours(n.QuietHours) // validated on load
	return notify.Policy{Kinds: n.Kinds, Quiet: q, Urgency: n.Urgency, Repeat: n.Repeat.Duration}
}

// Alerts configures the reading of the installations' Alertmanagers: watch
// reads them every Every and prints NEW and RESOLVED lines, snapshot adds
// the current set as a section.
type Alerts struct {
	// Installations are read always; a held lease whose name resolves to a
	// kube context is read too.
	Installations []Installation `yaml:"installations"`
	// Ignore are alert names that never appear; setting it replaces the
	// default (Heartbeat, InhibitionOutsideWorkingHours, Watchdog).
	Ignore []string `yaml:"ignore"`
	// Team is the team whose alerts are marked in capitals and counted.
	Team string `yaml:"team"`
	// Collapse is the number of changes of one alertname in one run above
	// which they are one line with a count.
	Collapse int `yaml:"collapse"`
	// Every is the watch's reading interval.
	Every Duration `yaml:"every"`
	// Timeout bounds the reading of one installation, port-forwards included.
	Timeout Duration `yaml:"timeout"`
	// Kubectl is the kubectl binary.
	Kubectl string `yaml:"kubectl"`
	// Flap is the flap damper.
	Flap Flap `yaml:"flap"`
}

// Flap holds back an alert that changes too often: its Changes-th NEW or
// RESOLVED within Window is one FLAPPING line, and its changes print nothing
// until it has been stable for Window.
type Flap struct {
	Changes int      `yaml:"changes"`
	Window  Duration `yaml:"window"`
}

// Installation is an installation and, optionally, the kube context that
// reaches it and the lowest severity printed of its alerts; written as a
// name alone or as {name, context, floor}. Without a context it is
// teleport.giantswarm.io-<name>, else <name>, else the one ending in @<name>.
type Installation struct {
	Name    string `yaml:"name"`
	Context string `yaml:"context"`
	// Floor is the lowest severity printed (alerts.Severities: none, info,
	// warning, notify, critical, page); empty prints every alert.
	Floor string `yaml:"floor"`
}

// UnmarshalYAML accepts a plain name.
func (i *Installation) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&i.Name)
	}
	type plain Installation
	return n.Decode((*plain)(i))
}

// DefaultIgnore are the alerts that always fire or only route others.
var DefaultIgnore = []string{"Heartbeat", "InhibitionOutsideWorkingHours", "Watchdog"}

// GitHub configures the budget reading.
type GitHub struct {
	// Floor is the remaining core budget under which GitHub work stops.
	Floor int `yaml:"floor"`
	// ProbeRepo is the repository whose conditional GET reads the budget
	// headers; any repository the token can read.
	ProbeRepo string `yaml:"probeRepo"`
}

// Overlaps tunes which sessions count as working on the same thing.
type Overlaps struct {
	// Ignore lists repositories whose issues and name form no overlap:
	// issue trackers and notebooks every session writes to.
	Ignore []string `yaml:"ignore"`
	// ActiveWithin leaves out sessions idle for longer.
	ActiveWithin Duration `yaml:"activeWithin"`
}

// Watch holds the thresholds of `beekeeper watch` (MiB unless noted).
type Watch struct {
	Interval        Duration `yaml:"interval"`
	Repeat          Duration `yaml:"repeat"`
	BudgetEvery     Duration `yaml:"budgetEvery"`
	AvailMinMiB     int      `yaml:"availMinMiB"`
	SwapMaxMiB      int      `yaml:"swapMaxMiB"`
	ScopeAnonMaxMiB int      `yaml:"scopeAnonMaxMiB"`
	ScopeMaxMiB     int      `yaml:"scopeMaxMiB"`
	LoadMax         float64  `yaml:"loadMax"`
	PSIMax          float64  `yaml:"psiMax"`
	TmpMaxMiB       int      `yaml:"tmpMaxMiB"`
	DiskMinMiB      int      `yaml:"diskMinMiB"`
}

// Metrics prices the tokens of the sessions' transcripts and sets the
// thresholds at which the watch reports a runaway session.
type Metrics struct {
	// Models maps a model id, or the prefix of one, to its price and context
	// window. A configured entry replaces the default of the same key; a
	// model no entry matches has no cost ("cost unknown").
	Models map[string]Model `yaml:"models"`
	// Runaway holds the watch's thresholds; a negative one turns its
	// figure off.
	Runaway Runaway `yaml:"runaway"`
}

// Model is a model's price in US dollars per million tokens and its context
// window in tokens.
type Model struct {
	Input        float64 `yaml:"input" json:"input"`
	Output       float64 `yaml:"output" json:"output"`
	CacheWrite5m float64 `yaml:"cacheWrite5m" json:"cacheWrite5m"`
	CacheWrite1h float64 `yaml:"cacheWrite1h" json:"cacheWrite1h"`
	CacheRead    float64 `yaml:"cacheRead" json:"cacheRead"`
	// Fast multiplies every price of a request in fast mode; zero (the
	// defaults): a fast request's cost is unknown.
	Fast          float64 `yaml:"fast" json:"fast,omitempty"`
	ContextWindow int     `yaml:"contextWindow" json:"contextWindow"`
}

// Runaway are the per-session figures over which the watch prints one line.
type Runaway struct {
	// GitHubCallsPerHour: gh and devctl commands and GitHub MCP tool calls
	// in the last hour.
	GitHubCallsPerHour int `yaml:"githubCallsPerHour"`
	// SameErrorRepeats: the same failing tool call in the last hour.
	SameErrorRepeats int `yaml:"sameErrorRepeats"`
	// ContextFill: the last request's context over the model's window.
	ContextFill float64 `yaml:"contextFill"`
}

// DefaultModels are the Claude API list prices (platform.claude.com/docs/en/
// about-claude/pricing): cache writes cost 1.25 times the input price for
// the 5-minute TTL and twice for the 1-hour TTL, cache reads a tenth,
// except where a model's own rate differs (Opus 5.5 $0.20, Fable 5.1 $0.25).
var DefaultModels = map[string]Model{
	"claude-fable-5-1":  {Input: 10, Output: 50, CacheWrite5m: 12.5, CacheWrite1h: 20, CacheRead: 0.25, ContextWindow: 1_000_000},
	"claude-fable-5":    {Input: 10, Output: 50, CacheWrite5m: 12.5, CacheWrite1h: 20, CacheRead: 1, ContextWindow: 1_000_000},
	"claude-opus-5-5":   {Input: 4, Output: 20, CacheWrite5m: 5, CacheWrite1h: 8, CacheRead: 0.2, ContextWindow: 1_000_000},
	"claude-opus-5":     {Input: 5, Output: 25, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.5, ContextWindow: 1_000_000},
	"claude-opus-4-8":   {Input: 5, Output: 25, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.5, ContextWindow: 1_000_000},
	"claude-opus-4-7":   {Input: 5, Output: 25, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.5, ContextWindow: 1_000_000},
	"claude-opus-4-6":   {Input: 5, Output: 25, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.5, ContextWindow: 1_000_000},
	"claude-sonnet-5":   {Input: 2, Output: 10, CacheWrite5m: 2.5, CacheWrite1h: 4, CacheRead: 0.2, ContextWindow: 1_000_000},
	"claude-sonnet-4-6": {Input: 3, Output: 15, CacheWrite5m: 3.75, CacheWrite1h: 6, CacheRead: 0.3, ContextWindow: 1_000_000},
	"claude-haiku-4-5":  {Input: 1, Output: 5, CacheWrite5m: 1.25, CacheWrite1h: 2, CacheRead: 0.1, ContextWindow: 200_000},
}

// Model returns the entry of model: its id, or its dated snapshot
// ("claude-haiku-4-5-20251001"). Another model of the same family is not
// priced like it.
func (m Metrics) Model(model string) (Model, bool) {
	if p, ok := m.Models[model]; ok {
		return p, true
	}
	if i := len(model) - len("-20060102"); i > 0 && model[i] == '-' && isDigits(model[i+1:]) {
		p, ok := m.Models[model[:i]]
		return p, ok
	}
	return Model{}, false
}

func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// Claude locates what Claude Code and the desktop app keep on disk.
type Claude struct {
	ProjectsDir string `yaml:"projectsDir"`
	DesktopDir  string `yaml:"desktopDir"`
}

// Memcap locates the build slots of the memcap wrapper.
type Memcap struct {
	SlotDir string `yaml:"slotDir"`
	Slots   int    `yaml:"slots"`
}

// Duration is a time.Duration written as "30s", "10m" in YAML.
type Duration struct{ time.Duration }

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	d.Duration = v
	return nil
}

// Path returns the configuration file beekeeper reads.
func Path(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if env := os.Getenv("BEEKEEPER_CONFIG"); env != "" {
		return env, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "beekeeper", "config.yaml"), nil
}

// Load reads the file at path and applies the defaults.
func Load(path string) (*Config, error) {
	c := &Config{}
	raw, err := os.ReadFile(filepath.Clean(path))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := yaml.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	if err := c.defaults(); err != nil {
		return nil, err
	}
	return c, c.validate()
}

func (c *Config) defaults() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		state = filepath.Join(home, ".local", "state")
	}
	setStr(&c.StateDir, filepath.Join(state, "beekeeper"))
	setStr(&c.LeaseDir, filepath.Join(c.StateDir, "leases"))
	setDur(&c.GrantTTL, 30*time.Minute)
	setDur(&c.Supervisor.RelayTTL, 15*time.Minute)
	setDur(&c.Supervisor.RestartGrace, time.Minute)

	setDur(&c.Overlaps.ActiveWithin, time.Hour)

	setInt(&c.GitHub.Floor, 2500)
	setStr(&c.GitHub.ProbeRepo, "giantswarm/devctl")

	w := &c.Watch
	setDur(&w.Interval, 30*time.Second)
	setDur(&w.Repeat, 10*time.Minute)
	setDur(&w.BudgetEvery, 5*time.Minute)
	setInt(&w.AvailMinMiB, 10240)
	setInt(&w.SwapMaxMiB, 10000)
	setInt(&w.ScopeAnonMaxMiB, 28000)
	setInt(&w.ScopeMaxMiB, 45000)
	if w.LoadMax == 0 {
		w.LoadMax = 45
	}
	if w.PSIMax == 0 {
		w.PSIMax = 10
	}
	setInt(&w.TmpMaxMiB, 20000)
	setInt(&w.DiskMinMiB, 102400)

	setStr(&c.Claude.ProjectsDir, filepath.Join(home, ".claude", "projects"))
	cfg, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	setStr(&c.Claude.DesktopDir, filepath.Join(cfg, "Claude", "claude-code-sessions"))

	if c.Metrics.Models == nil {
		c.Metrics.Models = map[string]Model{}
	}
	for k, v := range DefaultModels {
		if _, ok := c.Metrics.Models[k]; !ok {
			c.Metrics.Models[k] = v
		}
	}
	rw := &c.Metrics.Runaway
	setInt(&rw.GitHubCallsPerHour, 1000)
	setInt(&rw.SameErrorRepeats, 10)
	if rw.ContextFill == 0 {
		rw.ContextFill = 0.9
	}

	setInt(&c.Merge.Cap, 5)
	setDur(&c.Merge.QueueTTL, 15*time.Minute)
	setDur(&c.Merge.SeedTTL, 12*time.Hour)
	setDur(&c.Merge.Settle, 5*time.Minute)
	setDur(&c.Merge.SettleTimeout, 30*time.Minute)
	setDur(&c.Merge.BudgetFresh, time.Minute)
	setDur(&c.Merge.StallAfter, 5*time.Minute)

	setStr(&c.Memcap.SlotDir, filepath.Join(state, "memcap", "slots"))
	setInt(&c.Memcap.Slots, 2)
	al := &c.Alerts
	if al.Ignore == nil {
		al.Ignore = slices.Clone(DefaultIgnore)
	}
	setInt(&al.Collapse, 3)
	setDur(&al.Every, 5*time.Minute)
	setDur(&al.Timeout, time.Minute)
	setStr(&al.Kubectl, "kubectl")
	setInt(&al.Flap.Changes, 4)
	setDur(&al.Flap.Window, time.Hour)
	if c.Notify.Kinds == nil {
		c.Notify.Kinds = slices.Clone(notify.Kinds)
	}
	setDur(&c.Notify.Repeat, 30*time.Minute)
	if rest, ok := strings.CutPrefix(c.Supervisor.Instructions, "~/"); ok {
		c.Supervisor.Instructions = filepath.Join(home, rest)
	}
	return nil
}

func (c *Config) validate() error {
	if c.Supervisor.Skill != "" && c.Supervisor.Instructions != "" {
		return errors.New("supervisor: set skill or instructions, not both")
	}
	for i, in := range c.Alerts.Installations {
		if in.Name == "" {
			return fmt.Errorf("alerts.installations[%d]: an installation needs a name", i)
		}
		if in.Floor != "" && !slices.Contains(alerts.Severities, in.Floor) {
			return fmt.Errorf("alerts.installations[%d]: floor %q is none of %s", i, in.Floor, strings.Join(alerts.Severities, ", "))
		}
	}
	if n := c.Alerts.Flap.Changes; n < 0 || n == 1 {
		return fmt.Errorf("alerts.flap.changes: %d; an alert is flapping from its second change on at the earliest", n)
	}
	if err := c.Notify.validate(); err != nil {
		return err
	}
	for _, r := range c.Resources {
		if r == "" || r == Browser || filepath.Base(r) != r || r[0] == '.' {
			return fmt.Errorf("resources: %q is not a valid resource name", r)
		}
	}
	names, repos := map[string]bool{}, map[string]string{}
	for i, l := range c.Lanes {
		if l.Name == "" || len(l.Repositories) == 0 || strings.Contains(l.Name, "/") {
			return fmt.Errorf("lanes[%d]: a lane needs a name without a slash and repositories", i)
		}
		if names[l.Name] {
			return fmt.Errorf("lanes: %q is named twice", l.Name)
		}
		names[l.Name] = true
		for _, r := range l.Repositories {
			if prev, ok := repos[r]; ok {
				return fmt.Errorf("lanes: %s is in both %q and %q", r, prev, l.Name)
			}
			repos[r] = l.Name
		}
	}
	return nil
}

func (n Notify) validate() error {
	for _, k := range n.Kinds {
		if !slices.Contains(notify.Kinds, k) {
			return fmt.Errorf("notify.kinds: %q is none of %s", k, strings.Join(notify.Kinds, ", "))
		}
	}
	for k, u := range n.Urgency {
		if !slices.Contains(notify.Kinds, k) {
			return fmt.Errorf("notify.urgency: %q is none of %s", k, strings.Join(notify.Kinds, ", "))
		}
		if !slices.Contains(notify.Urgencies, u) {
			return fmt.Errorf("notify.urgency.%s: %q is none of %s", k, u, strings.Join(notify.Urgencies, ", "))
		}
	}
	if _, err := notify.ParseQuietHours(n.QuietHours); err != nil {
		return fmt.Errorf("notify.quietHours: %w", err)
	}
	return nil
}

// LaneOf is the lane a repository's merges queue in: the configured lane
// that lists it, else a lane of its own named after it.
func (c *Config) LaneOf(repo string) Lane {
	name := repo[strings.LastIndex(repo, "/")+1:]
	for _, l := range c.Lanes {
		for _, r := range l.Repositories {
			if strings.EqualFold(r, repo) || (!strings.Contains(r, "/") && strings.EqualFold(r, name)) {
				return l
			}
		}
	}
	return Lane{Name: repo, Repositories: []string{repo}}
}

// LaneNamed is the configured lane of that name.
func (c *Config) LaneNamed(name string) (Lane, bool) {
	for _, l := range c.Lanes {
		if l.Name == name {
			return l, true
		}
	}
	return Lane{}, false
}

// Leasable returns every resource a session can lease, the browser last.
func (c *Config) Leasable() []string {
	return append(slices.Clone(c.Resources), Browser)
}

// IsLeasable reports whether name is a configured resource or the browser.
func (c *Config) IsLeasable(name string) bool {
	return slices.Contains(c.Leasable(), name)
}

func setStr(p *string, v string) {
	if *p == "" {
		*p = v
	}
}

func setInt(p *int, v int) {
	if *p == 0 {
		*p = v
	}
}

func setDur(p *Duration, v time.Duration) {
	if p.Duration == 0 {
		p.Duration = v
	}
}
