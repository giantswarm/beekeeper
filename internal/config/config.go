// Package config loads beekeeper's machine configuration: where its state
// lives, which shared resources sessions lease, the GitHub budget floor, the
// thresholds of the machine watch and which installations' alerts it reads.
//
// The file is $XDG_CONFIG_HOME/beekeeper/config.yaml (or --config, or
// $BEEKEEPER_CONFIG). Every field is optional; a missing file is the
// defaults unless explicitly named by $BEEKEEPER_CONFIG. The defaults are
// neutral: memory thresholds are fractions of the machine's RAM, swap or
// filesystem, and every organisation or desk choice
// (the production installation, the installations, the lanes, the role
// names) is unset until configured. docs/examples/config.yaml is a complete
// desk's configuration.
package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/notify"
)

// Projects separates the person's clones from session checkouts.
type Projects struct {
	Dir       string   `yaml:"dir"`
	Worktrees []string `yaml:"worktrees"`
}

// Home names places for session state, logs and backups.
type Home struct {
	Places []string `yaml:"places"`
}

// Browser is the resource every machine has: the one Chrome the Claude in
// Chrome extension drives.
const Browser = "browser"

// ModelServer is the resource of the host's model server: a claim carries
// the GiB its models may hold.
const ModelServer = "model-server"

// Delivery configures observation of issue delivery. A zero limit is off.
type Delivery struct {
	WIPLimit     int      `yaml:"wipLimit"`
	EpicLimit    int      `yaml:"epicLimit"`
	EpicParallel int      `yaml:"epicParallel"`
	StallAfter   Duration `yaml:"stallAfter"`
}

// Config is the parsed configuration with the defaults applied.
type Config struct {
	Delivery Delivery `yaml:"delivery"`
	Projects Projects `yaml:"projects"`
	Home     Home     `yaml:"home"`
	// StateDir holds state.json, events.jsonl and the last snapshot.
	StateDir string `yaml:"stateDir"`
	// Fatal adds machine-specific outage probes to the independent sentinel.
	Fatal Fatal `yaml:"fatal"`
	// LeaseDir holds one directory per held lease (mkdir is the lock).
	LeaseDir string `yaml:"leaseDir"`
	// LeaseRound is the default round when a lease purpose names no duration (1h).
	LeaseRound Duration `yaml:"leaseRound"`
	// Identity is whose agents run here: every party this machine's
	// commands record carries it.
	Identity Identity `yaml:"identity"`
	// Serve configures the central instance (beekeeper serve).
	Serve Serve `yaml:"serve"`
	// Resources are the environments sessions lease besides the browser:
	// kind labs and shared installations.
	Resources []string `yaml:"resources"`
	// Labs maps a lab lease (one of Resources) to the kind cluster it
	// stands for: `lease list`, a claim and `free` name the cluster, and a
	// running kind cluster no lab lease maps is reported as unmapped.
	Labs map[string]string `yaml:"labs"`
	// GrantTTL is how long a grant stays claimable once its resource is free.
	GrantTTL Duration `yaml:"grantTTL"`
	// Shell is the shell the hook runs a rewritten command in (default:
	// $SHELL, else sh).
	Shell string `yaml:"shell"`
	// MaxKindClusters is how many kind clusters the machine runs at most
	// (default: one per 40 GiB of RAM, at least one).
	MaxKindClusters int `yaml:"maxKindClusters"`

	Kube Kube `yaml:"kube"`
	// Teleport is the login the kube contexts reach the installations
	// through, and its keeper.
	Teleport Teleport `yaml:"teleport"`

	GitHub   GitHub    `yaml:"github"`
	Watch    Watch     `yaml:"watch"`
	Disk     Disk      `yaml:"disk"`
	Overlaps Overlaps  `yaml:"overlaps"`
	Claude   Claude    `yaml:"claude"`
	Omp      Omp       `yaml:"omp"`
	Agy      Agy       `yaml:"agy"`
	Codex    Codex     `yaml:"codex"`
	Desktop  Desktop   `yaml:"desktop"`
	Memcap   Memcap    `yaml:"memcap"`
	Run      RunConfig `yaml:"run"`
	Lanes    []Lane    `yaml:"lanes"`
	Merge    Merge     `yaml:"merge"`
	// Units configures the transient user services beekeeper starts.
	Units  Units  `yaml:"units"`
	Alerts Alerts `yaml:"alerts"`
	// Upgrades paces the reading of the installations' Cluster API
	// upgrades.
	Upgrades Upgrades `yaml:"upgrades"`
	Notify   Notify   `yaml:"notify"`
	Metrics  Metrics  `yaml:"metrics"`
	Ollama   Ollama   `yaml:"ollama"`
	// Lemonade is the host's Lemonade Server, a second model server under
	// the same model-server lease and ollama's budgets.
	Lemonade Lemonade `yaml:"lemonade"`
	// Supervisor configures the supervisor loop.
	Supervisor Supervisor `yaml:"supervisor"`
	// Guide is the role that walks the person through the decisions
	// waiting on them.
	Guide Guide `yaml:"guide"`
	// Agents configures how a registered agent near its context limit is
	// handed over to a fresh session.
	Agents Agents `yaml:"agents"`
	// Capacity is how many agents the supervisor keeps busy and the
	// headroom a new start needs.
	Capacity Capacity `yaml:"capacity"`
	// Stop is what beekeeper stop stops besides beekeeper's own units.
	Stop Stop `yaml:"stop"`
	// Reporter is the one-off status reporter session the standby watch
	// starts once per interval.
	Reporter Reporter `yaml:"reporter"`
	// Doctor is what `beekeeper doctor` and the watch fix by themselves.
	Doctor Doctor `yaml:"doctor"`
	// Outbound is what the hook refuses to let leave the machine and where
	// the watch looks for credentials left exposed on disk.
	Outbound Outbound `yaml:"outbound"`
	// Hooks says where beekeeper's Claude Code hooks act.
	Hooks Hooks `yaml:"hooks"`
	// Scan is where the transcript value scanner's index takes its values
	// from.
	Scan Scan `yaml:"scan"`
	// Secret configures beekeeper secret.
	Secret Secret `yaml:"secret"`
	// Sandbox is the agent sandbox beekeeper renders into Claude Code's
	// managed settings (beekeeper sandbox).
	Sandbox Sandbox `yaml:"sandbox"`
	// Board is the project board `beekeeper board` picks work from.
	Board Board `yaml:"board"`
	// Apps is where the organisation declares the GitHub Apps whose
	// consent `beekeeper app allow` records.
	Apps Apps `yaml:"apps"`
	// Plans are the repositories whose pull requests a note for a person
	// names only once their stage check is green, and where an epic's plan
	// pull request is found.
	Plans Plans `yaml:"plans"`
	// Central is the central instance (beekeeper serve behind muster) that
	// holds the leases of the shared installations and the roster of every
	// machine's agents; without its context, all of it stays on the machine.
	Central Central `yaml:"central"`
	// Feedback is the Slack feedback watch: the person's replies in the
	// threads of the reporter's posts, read through muster as the person.
	Feedback Feedback `yaml:"feedback"`
	// Sink dispatches a GitHub workflow on selected events.
	Sink Sink `yaml:"sink"`
}

// Stop configures beekeeper stop.
type Stop struct {
	// Units are more user units (a timer of the desk's own) that beekeeper
	// stop stops while they run and beekeeper resume starts again.
	Units []string `yaml:"units"`
}

// Fatal configures additional probes for critical machine components.
type Fatal struct {
	// Checks are fatal health checks, never arbitrary worker reports. Exit 0
	// is healthy, 1 is failed, 2 is deliberately disabled, others unknown.
	Checks []FatalCheck `yaml:"checks"`
	// Repositories are repositories whose default branch CI is critical.
	Repositories []string `yaml:"repositories"`
	// Checkouts maps repair repositories to source checkouts; otherwise the
	// source is projects.dir/<repository-name>. Repairs use managed worktrees.
	Checkouts map[string]string `yaml:"checkouts"`
}

type FatalCheck struct {
	Name  string   `yaml:"name"`
	Probe []string `yaml:"probe"`
	// Harness excludes the broken harness from repair selection.
	Harness string `yaml:"harness"`
	// Repository owns this check's repair (default giantswarm/beekeeper).
	Repository string `yaml:"repository"`
}

// Doctor configures the known faults the doctor probes and remedies.
type Doctor struct {
	Faults []Fault `yaml:"faults"`
	// GoCacheMaxGiB caps the Go build cache every session's builds share:
	// over it the watch trims the least recently used entries down to
	// three quarters of it, while no go build runs or once the cache is
	// past it by a quarter. Default 20; negative: off.
	GoCacheMaxGiB int `yaml:"goCacheMaxGiB"`
	// GoCacheEvery is how often the cache's size is read (default 1h);
	// a trim a build held back is tried again every five minutes.
	GoCacheEvery Duration `yaml:"goCacheEvery"`
	// FreeEvery is how often the watch frees what dead sessions and dead go
	// commands leave in the tmpfs /tmp (free --apply's automatic sections;
	// default 1h); negative: never.
	FreeEvery Duration `yaml:"freeEvery"`
	// DockerImagesMaxGiB caps the machine's Docker images: over it the
	// watch removes the unused images created longest ago down to three
	// quarters of it, never one a container or a kind lab's node uses, and
	// never while an image import or build runs or a lab lease is fresh.
	// Default 40; negative: off.
	DockerImagesMaxGiB int `yaml:"dockerImagesMaxGiB"`
	// DockerBuildCacheMaxGiB caps Docker's build cache likewise, pruned
	// by Docker's own least-recently-used order. Default 20; negative: off.
	DockerBuildCacheMaxGiB int `yaml:"dockerBuildCacheMaxGiB"`
	// DockerEvery is how often the watch reads Docker's sizes (default 1h);
	// a prune an import held back is tried again at the next sample.
	DockerEvery Duration `yaml:"dockerEvery"`
	// WorktreeRoots are the folders (~/ allowed) under which the watch
	// looks for the linked git worktrees of finished workers (default
	// ~/.local/state/beekeeper/worktrees).
	WorktreeRoots []string `yaml:"worktreeRoots"`
	// WorktreesEvery is how often the watch removes the worktrees whose
	// pull request merged or closed, and the build output of those that
	// still hold uncommitted or unpushed work and of any other untouched
	// for six hours (default 6h); negative: never.
	WorktreesEvery Duration `yaml:"worktreesEvery"`
}

// DockerImagesMax is the Docker images' cap in bytes; 0 is off.
func (d Doctor) DockerImagesMax() int64 { return gib(d.DockerImagesMaxGiB) }

// DockerBuildCacheMax is the Docker build cache's cap in bytes; 0 is off.
func (d Doctor) DockerBuildCacheMax() int64 { return gib(d.DockerBuildCacheMaxGiB) }

// gib is n GiB in bytes, 0 for a negative n.
func gib(n int) int64 { return int64(max(n, 0)) << 30 }

// GoCacheMax is the Go build cache's cap in bytes; 0 is off.
func (d Doctor) GoCacheMax() int64 {
	if d.GoCacheMaxGiB < 0 {
		return 0
	}
	return int64(d.GoCacheMaxGiB) << 30
}

// Fault is a known fault with a known remedy.
type Fault struct {
	Name string `yaml:"name"`
	// Probe is a shell command that exits 0 while the fault is absent.
	Probe string `yaml:"probe"`
	// Remedy is the shell command that fixes the fault.
	Remedy string `yaml:"remedy"`
	// Unattended lets the watch run the remedy by itself; otherwise a
	// sighting is one note for guide.person.
	Unattended bool `yaml:"unattended"`
}

// Notify configures what `watch --notify` sends to the desktop.
type Notify struct {
	// Kinds are the kinds that notify (notify.Kinds, the default: due,
	// budget, stale-lease, no-supervisor). The machine's lines never
	// notify: the supervisor's watch says them.
	Kinds []string `yaml:"kinds"`
	// QuietHours ("22:00-07:00", local time) hold every notification but a
	// critical one until they end.
	QuietHours string `yaml:"quietHours"`
	// Urgency is a kind's urgency (low, normal, critical); no-supervisor
	// is critical, the others normal.
	Urgency map[string]string `yaml:"urgency"`
	// Repeat is how often a lasting condition (budget, no-supervisor)
	// notifies again while it lasts.
	Repeat Duration `yaml:"repeat"`
}

// Policy is the notify package's view of the section.
func (n Notify) Policy() notify.Policy {
	q, _ := notify.ParseQuietHours(n.QuietHours) // validated on load
	return notify.Policy{Kinds: n.Kinds, Quiet: q, Urgency: n.Urgency, Repeat: n.Repeat.Duration}
}

// Overlaps tunes which sessions count as working on the same thing.
type Overlaps struct {
	// Ignore lists repositories whose issues and name form no overlap:
	// issue trackers and notebooks every session writes to.
	Ignore []string `yaml:"ignore"`
	// ActiveWithin leaves out sessions idle for longer.
	ActiveWithin Duration `yaml:"activeWithin"`
}

// Duration is a time.Duration written as "30s", "10m" in YAML.
type Duration struct{ time.Duration }

// UnmarshalYAML parses a Go duration string, or whole days as "7d".
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n >= 0 {
			d.Duration = time.Duration(n) * 24 * time.Hour
			return nil
		}
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	d.Duration = v
	return nil
}

// Tokens is a token count written as "400k", "1m" or "400000" in YAML.
type Tokens int64

// UnmarshalYAML parses a count with an optional k or m suffix.
func (t *Tokens) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := ParseTokens(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*t = v
	return nil
}

// ParseTokens parses "400k", "1m" or "400000" (case-insensitive).
func ParseTokens(s string) (Tokens, error) {
	num, mult := strings.ToLower(strings.TrimSpace(s)), 1.0
	switch {
	case strings.HasSuffix(num, "k"):
		num, mult = strings.TrimSuffix(num, "k"), 1e3
	case strings.HasSuffix(num, "m"):
		num, mult = strings.TrimSuffix(num, "m"), 1e6
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || !(f*mult >= 1 && f*mult < 1e12) {
		return 0, fmt.Errorf("%q is not a positive token count (400k, 1m, 400000)", s)
	}
	return Tokens(f * mult), nil
}

// Path returns the configuration file beekeeper reads.
func Path(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if env := os.Getenv("BEEKEEPER_CONFIG"); env != "" {
		if _, err := os.Stat(filepath.Clean(env)); err != nil { //nolint:gosec // the configuration file explicitly selected by the caller
			return "", err
		}
		return env, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "beekeeper", "config.yaml"), nil
}

// Load reads the file at path and applies the defaults. A configuration
// that is only incomplete loads: Incomplete names what is missing. A missing
// file explicitly named by BEEKEEPER_CONFIG is an error, never defaults.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	switch {
	case errors.Is(err, os.ErrNotExist):
		if env := os.Getenv("BEEKEEPER_CONFIG"); env != "" && filepath.Clean(env) == filepath.Clean(path) {
			return nil, err
		}
		raw = nil
	case err != nil:
		return nil, err
	}
	return parse(path, raw)
}

// parse is the configuration raw holds, read from path, with the defaults
// applied and validated.
func parse(path string, raw []byte) (*Config, error) {
	c := &Config{Supervisor: Supervisor{HostsFirst: true}}
	if err := yaml.Unmarshal(raw, c); err != nil {
		var doc yaml.Node
		if yaml.Unmarshal(raw, &doc) == nil && len(doc.Content) > 0 {
			var findings, invalid []string
			root := effectiveNode(doc.Content[0], "", map[*yaml.Node]bool{}, &findings)
			checkNode(root, reflect.TypeFor[Config](), "", &findings, &invalid)
			if len(findings) > 0 {
				return nil, fmt.Errorf("%s: %s: %w", path, strings.Join(findings, "; "), err)
			}
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.defaults(); err != nil {
		return nil, err
	}
	return c, c.validate()
}

// Incomplete names the references whose source section is missing: each
// fails the commands that follow it, and leaves every other command
// working, so an edit that adds a reference and its section one after the
// other never makes the configuration unloadable in between.
func (c *Config) Incomplete() []string {
	var out []string
	for i, id := range c.Secret.AgeIdentities {
		switch {
		case strings.HasPrefix(id.Ref, "store://") && len(c.Secret.Store.Read) == 0:
			out = append(out, fmt.Sprintf("secret.ageIdentities[%d]: ref %q: a store:// reference takes secret.store.read", i, id.Ref))
		case id.Ref == "store://" && len(c.Secret.Store.Search) == 0:
			out = append(out, fmt.Sprintf("secret.ageIdentities[%d]: ref %q: store:// without an entry takes secret.store.search", i, id.Ref))
		}
	}
	return out
}

func userHomeDir() (string, error) {
	if h := os.Getenv("BEEKEEPER_EXECUTION_HOME"); filepath.IsAbs(h) {
		return filepath.Clean(h), nil
	}
	return os.UserHomeDir()
}

func (c *Config) defaults() error {
	setDur(&c.Delivery.StallAfter, 24*time.Hour)
	if c.Delivery.EpicParallel == 0 {
		c.Delivery.EpicParallel = 3
	}
	home, err := userHomeDir()
	if err != nil {
		return err
	}
	setStr(&c.Projects.Dir, filepath.Join(home, "projects"))
	c.Projects.Dir = filepath.Clean(homePath(home, c.Projects.Dir))
	if c.Projects.Worktrees == nil {
		c.Projects.Worktrees = []string{filepath.Join(c.Projects.Dir, "worktrees")}
	}
	if c.Home.Places == nil {
		c.Home.Places = []string{filepath.Join(home, ".local", "state")}
	}
	for _, places := range [][]string{c.Projects.Worktrees, c.Home.Places} {
		for i, p := range places {
			places[i] = filepath.Clean(homePath(home, p))
		}
	}
	if c.Agents.Roots == nil {
		c.Agents.Roots = slices.Clone(c.Projects.Worktrees)
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		state = filepath.Join(home, ".local", "state")
	}
	setStr(&c.StateDir, filepath.Join(state, "beekeeper"))
	if c.Fatal.Repositories == nil {
		c.Fatal.Repositories = []string{"giantswarm/beekeeper"}
	}
	setStr(&c.Disk.Prune, DiskPruneAuto)
	if c.Disk.TrashDirs == nil {
		c.Disk.TrashDirs = []string{filepath.Join(c.StateDir, "trash"), filepath.Join(state, "trash")}
	}
	for i, p := range c.Disk.TrashDirs {
		c.Disk.TrashDirs[i] = filepath.Clean(homePath(home, p))
	}
	setDur(&c.Disk.Every, time.Hour)
	setInt(&c.Disk.SccacheMaxGiB, 10)
	h := &c.Disk.Holders
	// The state home is stateDir's parent; a stateDir right under the
	// filesystem root is its own home, never the root itself.
	root := filepath.Dir(c.StateDir)
	if filepath.Clean(root) == string(filepath.Separator) {
		root = c.StateDir
	}
	setStr(&h.Root, root)
	h.Root = filepath.Clean(homePath(home, h.Root))
	setDur(&h.StaleAfter, 72*time.Hour)
	setDur(&h.AsideRetention, 7*24*time.Hour)
	if h.AsideDirs == nil {
		h.AsideDirs = []string{"aside", "moved-aside"}
	}
	setInt(&h.WarnGiB, 10)
	setDur(&h.Every, 6*time.Hour)
	for i, p := range c.Disk.Checkouts {
		c.Disk.Checkouts[i] = filepath.Clean(homePath(home, p))
	}
	for i, e := range c.Disk.Caches {
		if e.Path != "" {
			c.Disk.Caches[i].Path = filepath.Clean(homePath(home, e.Path))
		}
		if e.Name == "" {
			c.Disk.Caches[i].Name = c.Disk.Caches[i].Path
		}
	}
	for i, o := range c.Disk.BuildOutputs {
		c.Disk.BuildOutputs[i] = BuildOutput{Path: filepath.Clean(homePath(home, o.Path)), Worktree: filepath.Clean(homePath(home, o.Worktree))}
	}
	setStr(&c.LeaseDir, filepath.Join(c.StateDir, "leases"))
	setDur(&c.GrantTTL, 30*time.Minute)
	setDur(&c.LeaseRound, time.Hour)
	if c.Identity.Host == "" {
		if h, err := os.Hostname(); err == nil {
			c.Identity.Host, _, _ = strings.Cut(h, ".")
		}
	}
	if c.Supervisor.ReviewRounds == 0 {
		c.Supervisor.ReviewRounds = 1
	}
	if c.Supervisor.UnmergedLimit == 0 {
		c.Supervisor.UnmergedLimit = DefaultUnmergedLimit
	}
	if c.Supervisor.GraphQLLimit == 0 {
		c.Supervisor.GraphQLLimit = DefaultLoopGraphQLLimit
	}
	if c.Supervisor.ClassifierConfidence == 0 {
		c.Supervisor.ClassifierConfidence = 0.8
	}
	setDur(&c.Supervisor.LoopInterval, 30*time.Second)
	setDur(&c.Supervisor.ReleaseEvery, 10*time.Minute)
	setDur(&c.Supervisor.PostMergeWindow, 2*time.Hour)
	setDur(&c.Supervisor.FillRetryEvery, 5*time.Minute)
	setDur(&c.Supervisor.ReserveFor, DefaultReserveFor)
	setDur(&c.Supervisor.UnmergedWindow, DefaultUnmergedWindow)
	setDur(&c.Supervisor.NoteDue, 24*time.Hour)
	if c.Supervisor.Ownership.Team != "" {
		setStr(&c.Supervisor.Ownership.TeamFiles, DefaultTeamFiles)
	}
	setDur(&c.Supervisor.Ownership.OwnerWait, 24*time.Hour)
	c.Supervisor.ReviewBrief = homePath(home, c.Supervisor.ReviewBrief)
	c.Supervisor.WorkerBrief = homePath(home, c.Supervisor.WorkerBrief)
	setStr(&c.Serve.Gateway.TokenFile, "/var/run/secrets/klaus-gateway/token")
	c.Serve.Gateway.TokenFile = homePath(home, c.Serve.Gateway.TokenFile)
	setStr(&c.Serve.Gateway.AnswerTool, "x_beekeeper_note_answer")
	setStr(&c.Serve.Gateway.SendTool, "x_beekeeper_send_message")
	setStr(&c.Serve.Identity.TokenFile, "/var/run/secrets/dex/token")
	c.Serve.Identity.TokenFile = homePath(home, c.Serve.Identity.TokenFile)
	setStr(&c.Central.Server, "beekeeper")
	setStr(&c.Central.Muster, "muster")
	setDur(&c.Central.Timeout, 30*time.Second)
	setStr(&c.Feedback.Server, "slack")
	setDur(&c.Feedback.Every, 5*time.Minute)
	setDur(&c.Feedback.Window, 24*time.Hour)
	// The guide's context is the person's conversation: it relays early.
	if c.Guide.RelayAt == 0 {
		c.Guide.RelayAt = 200_000
	}
	c.Guide.defaults(home)
	setDur(&c.Guide.WaitingTTL, 2*time.Hour)
	if c.Agents.RelayAt == 0 {
		c.Agents.RelayAt = defaultRelayAt
	}
	setDur(&c.Agents.LastStepGrace, 30*time.Minute)
	if c.Agents.LastStepCeiling == 0 {
		c.Agents.LastStepCeiling = c.Agents.RelayAt + c.Agents.RelayAt/4
	}
	setDur(&c.Agents.NoteWait, 3*time.Minute)
	setDur(&c.Agents.StaleAfter, 24*time.Hour)
	if c.Agents.Shell.Unalias == nil {
		c.Agents.Shell.Unalias = DefaultUnalias
	}
	setStr(&c.Agents.Shell.Globs, GlobsLiteral)
	setStr(&c.Agents.Mode, ModeDesktop)
	c.Reporter.defaults(home, c.Guide.Person)
	c.Outbound.defaults(home)
	for i, d := range c.Hooks.Scope.Dirs {
		c.Hooks.Scope.Dirs[i] = filepath.Clean(homePath(home, d))
	}
	for i, d := range c.Hooks.PersonFolders {
		if d == "~" {
			d = home
		}
		c.Hooks.PersonFolders[i] = filepath.Clean(homePath(home, d))
	}
	setInt(&c.Scan.MinLength, 12)
	c.Secret.TokenFile = homePath(home, c.Secret.TokenFile)
	for i, f := range c.Secret.Files {
		c.Secret.Files[i] = homePath(home, f)
	}
	for i, f := range c.Secret.FileRoots {
		c.Secret.FileRoots[i] = filepath.Clean(homePath(home, f))
	}
	for i, f := range c.Kube.Kubeconfig {
		c.Kube.Kubeconfig[i] = homePath(home, f)
	}
	for i, f := range c.Alerts.Kubeconfig {
		c.Alerts.Kubeconfig[i] = homePath(home, f)
	}
	for in, s := range c.Apps.Secrets {
		s.File = homePath(home, s.File)
		c.Apps.Secrets[in] = s
	}
	c.Sandbox.defaults(home)
	c.Run.Remote.Kubeconfig = homePath(home, c.Run.Remote.Kubeconfig)
	for i := range c.Scan.SOPS {
		c.Scan.SOPS[i] = homePath(home, c.Scan.SOPS[i])
	}
	setStr(&c.Plans.Check, DefaultPlansCheck)
	c.Sink.defaults()
	c.Teleport.defaults(home)
	if b := c.Teleport.Bot; b.Enabled() {
		c.Kube.Kubeconfig = append([]string{b.Kubeconfig}, c.Kube.Kubeconfig...)
		c.Alerts.Kubeconfig = append([]string{b.Kubeconfig}, c.Alerts.Kubeconfig...)
	}
	setStr(&c.Shell, os.Getenv("SHELL"))
	setStr(&c.Shell, "sh")

	setDur(&c.Overlaps.ActiveWithin, time.Hour)
	setDur(&c.Board.StaleAfter, 365*24*time.Hour)
	setDur(&c.Board.ArchiveAfter, DefaultBoardArchiveAfter)
	setDur(&c.Board.TidyEvery, time.Hour)
	setDur(&c.Board.EscalateEvery, 7*24*time.Hour)
	setDur(&c.Board.Claims.Refresh, 6*time.Hour)
	setDur(&c.Board.Claims.StaleAfter, 24*time.Hour)

	setInt(&c.GitHub.Floor, 2500)
	setInt(&c.GitHub.UrgentBound, 200)
	setStr(&c.GitHub.ProbeRepo, "giantswarm/beekeeper")
	if c.GitHub.AllowedOrgs == nil {
		c.GitHub.AllowedOrgs = []string{"giantswarm"}
	}

	w := &c.Watch
	setDur(&w.Interval, 30*time.Second)
	setDur(&w.BatchInterval, 5*time.Minute)
	setDur(&w.Repeat, 10*time.Minute)
	setDur(&w.BudgetEvery, 5*time.Minute)
	setDur(&w.ThroughputAfter, 2*time.Hour)
	setDur(&w.SwapWindow, 10*time.Minute)
	setDur(&w.OOMDWithin, 30*time.Minute)
	setDur(&w.DiskFillWithin, 2*time.Hour)
	setInt(&c.Doctor.GoCacheMaxGiB, 20)
	setDur(&c.Doctor.GoCacheEvery, time.Hour)
	setDur(&c.Doctor.FreeEvery, time.Hour)
	setInt(&c.Doctor.DockerImagesMaxGiB, 40)
	setInt(&c.Doctor.DockerBuildCacheMaxGiB, 20)
	setDur(&c.Doctor.DockerEvery, time.Hour)
	setDur(&c.Doctor.WorktreesEvery, 6*time.Hour)
	if c.Doctor.WorktreeRoots == nil {
		c.Doctor.WorktreeRoots = []string{"~/.local/state/beekeeper/worktrees"}
	}
	for i, r := range c.Doctor.WorktreeRoots {
		c.Doctor.WorktreeRoots[i] = filepath.Clean(homePath(home, r))
	}
	setStr(&c.Ollama.Unit, "ollama")
	setInt(&c.Ollama.BudgetGiB, 12)
	setInt(&c.Ollama.MaxBudgetGiB, 24)
	if c.Ollama.LabTests == nil {
		c.Ollama.LabTests = []string{"models-test"}
	}
	if w.LoadPerCoreMax == 0 {
		w.LoadPerCoreMax = 1.5
	}
	if w.PSIMax == 0 {
		w.PSIMax = 10
	}
	if w.CPUPSIMax == 0 {
		w.CPUPSIMax = 40
	}
	if w.ForkRateMax == 0 {
		w.ForkRateMax = 50
	}
	setInt(&w.StackMax, 3)
	setInt(&w.ToolProcsMax, 1000)
	if w.Tools == nil {
		w.Tools = []string{"kubectl", "helm", "tsh", "gh", "flux", "devctl"}
	}

	setStr(&c.Claude.ProjectsDir, filepath.Join(home, ".claude", "projects"))
	setStr(&c.Claude.DesktopApp, DefaultDesktopApp)
	setStr(&c.Claude.SessionsDir, filepath.Join(home, ".claude", "sessions"))
	setStr(&c.Omp.SessionsDir, filepath.Join(home, ".omp", "agent", "sessions"))
	setStr(&c.Omp.ModelsFile, filepath.Join(home, ".omp", "agent", "models.yml"))
	c.Omp.SessionsDir = homePath(home, c.Omp.SessionsDir)
	c.Omp.ModelsFile = homePath(home, c.Omp.ModelsFile)
	setStr(&c.Agy.BrainDir, filepath.Join(home, ".gemini", "antigravity-cli", "brain"))
	setStr(&c.Agy.ProjectsDir, filepath.Join(home, ".gemini", "config", "projects"))
	setStr(&c.Agy.ConversationsDir, filepath.Join(home, ".gemini", "antigravity-cli", "conversations"))
	c.Agy.BrainDir = homePath(home, c.Agy.BrainDir)
	c.Agy.ProjectsDir = homePath(home, c.Agy.ProjectsDir)
	c.Agy.ConversationsDir = homePath(home, c.Agy.ConversationsDir)
	setStr(&c.Codex.SessionsDir, filepath.Join(home, ".codex", "sessions"))
	setStr(&c.Codex.AuthFile, filepath.Join(home, ".codex", "auth.json"))
	setStr(&c.Codex.ConfigFile, filepath.Join(home, ".codex", "config.toml"))
	c.Codex.SessionsDir = homePath(home, c.Codex.SessionsDir)
	c.Codex.AuthFile = homePath(home, c.Codex.AuthFile)
	c.Codex.ConfigFile = homePath(home, c.Codex.ConfigFile)
	cfg, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	setStr(&c.Claude.DesktopDir, filepath.Join(cfg, "Claude", "claude-code-sessions"))
	setStr(&c.Claude.DesktopLog, filepath.Join(cfg, "Claude", "logs", "main.log"))
	setDur(&c.Desktop.TypingQuiet, 30*time.Second)

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
	setInt(&c.Merge.SessionCap, 2)
	setDur(&c.Merge.QueueTTL, DefaultQueueTTL)
	setDur(&c.Merge.SeedTTL, 12*time.Hour)
	setDur(&c.Merge.Settle, 5*time.Minute)
	setDur(&c.Merge.SettleTimeout, 30*time.Minute)
	setDur(&c.Merge.BudgetFresh, time.Minute)
	setDur(&c.Merge.ResetFirst, 5*time.Minute)
	setDur(&c.Merge.StallAfter, 5*time.Minute)
	setDur(&c.Merge.HungAfter, 45*time.Minute)
	setDur(&c.Merge.CITimeout, 2*time.Hour)
	setDur(&c.Merge.BusyBudget, 4*time.Hour)
	setDur(&c.Merge.ToolLimit, 9*time.Minute)
	setDur(&c.Secret.UnlockWait, 8*time.Minute)
	setDur(&c.Secret.SessionLifetime, 12*time.Hour)

	setStr(&c.Memcap.SlotDir, filepath.Join(state, "memcap", "slots"))
	setInt(&c.Memcap.Slots, 2)
	setInt(&c.Memcap.CPUWeight, DefaultMemcapCPUWeight)
	setStr(&c.Run.Tail, DefaultRunTail)
	setDur(&c.Memcap.HoldBound, 30*time.Minute)
	if !c.Capacity.reviewHeadroomSet {
		c.Capacity.ReviewHeadroom = 2
	}
	if !c.Capacity.ceilingSet && c.Capacity.Ceiling == 0 {
		c.Capacity.Ceiling = DefaultCeiling
	}
	c.Capacity.Usage.defaults(home)
	for _, name := range slices.Sorted(maps.Keys(c.Capacity.Providers)) {
		p := c.Capacity.Providers[name]
		if p.SelfHosted() {
			// A self-hosted provider has no subscription to keep on pace:
			// capacity.ceiling, which a desk lowers to hold one on its week,
			// does not set its target. Without a ceiling of its own it runs
			// DefaultSelfHostedCeiling sessions at once, bounded by the
			// machine guards.
			if !p.ceilingSet {
				p.Ceiling = DefaultSelfHostedCeiling
			}
			for i := range p.ServeModeCommand {
				p.ServeModeCommand[i] = homePath(home, p.ServeModeCommand[i])
			}
		} else if !p.ceilingSet && p.Ceiling == 0 {
			p.Ceiling = c.Capacity.Ceiling
		}
		if name == ClaudeHarness && len(p.Command) == 0 {
			p.Command = slices.Clone(DefaultClaudeUsageCommand)
		}
		if name == AgyHarness && len(p.Command) == 0 && !p.SelfHosted() {
			p.Command = slices.Clone(DefaultAgyUsageCommand)
		}
		if name == CodexHarness && len(p.Command) == 0 {
			p.Command = slices.Clone(DefaultCodexUsageCommand)
		}
		p.defaults(home)
		c.Capacity.Providers[name] = p
	}
	setInt(&c.Capacity.AvailMinMiB, 20<<10)
	setInt(&c.Capacity.SwapGrowthMaxMiB, 256)
	al := &c.Alerts
	if al.Ignore == nil {
		al.Ignore = slices.Clone(DefaultIgnore)
	}
	if al.Quiet == nil {
		al.Quiet = DefaultQuietFor(al.Team)
	}
	if c.Watch.QuietSessions == nil {
		c.Watch.QuietSessions = slices.Clone(DefaultQuietSessions)
	}
	setInt(&al.Collapse, 3)
	setDur(&al.Every, 5*time.Minute)
	setDur(&al.Timeout, time.Minute)
	setStr(&al.Kubectl, "kubectl")
	setInt(&al.Flap.Changes, 4)
	setDur(&al.Flap.Window, time.Hour)
	setStr(&al.PageSeverity, alerts.Page)
	setDur(&al.OwnerGrace, 15*time.Minute)
	setStr(&al.PagerDuty.Server, "pd")
	setDur(&al.PagerDuty.Every, time.Minute)
	setStr(&al.Discover.Label, "application.giantswarm.io/team")
	if al.Discover.Exclude == nil {
		al.Discover.Exclude = []string{"*-*", "*.*"}
	}
	setDur(&c.Upgrades.Every, 5*time.Minute)
	if c.Notify.Kinds == nil {
		c.Notify.Kinds = slices.Clone(notify.Kinds)
	}
	setDur(&c.Notify.Repeat, 30*time.Minute)
	return nil
}

func homePath(home, p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return filepath.Join(home, rest)
	}
	return p
}

// validate keeps the loader's first-finding contract. The checker uses
// validateEach to collect independent rules without repeated edit/check rounds.
func (c *Config) validate() error { return firstFinding(c.validateEach) }

func firstFinding(visit func(func(error))) error {
	var first error
	visit(func(err error) {
		if first == nil {
			first = err
		}
	})
	return first
}

func (c *Config) validateEach(report func(error)) {
	for _, owner := range c.GitHub.PublicOnlyOwners {
		if owner == "" || strings.TrimSpace(owner) != owner || strings.ContainsAny(owner, "/ *?\\\t\r\n") {
			report(fmt.Errorf("github.publicOnlyOwners: invalid owner %q", owner))
		}
	}
	if c.Delivery.StallAfter.Duration < 0 {
		report(fmt.Errorf("delivery.stallAfter must be positive"))
	}
	if c.Delivery.EpicLimit < 0 {
		report(fmt.Errorf("delivery.epicLimit must be nonnegative"))
	}
	if c.Delivery.EpicParallel < 0 {
		report(fmt.Errorf("delivery.epicParallel must be positive"))
	}
	if c.Delivery.WIPLimit < 0 {
		report(fmt.Errorf("delivery.wipLimit must be nonnegative"))
	}
	// These intervals drive recurring loops; zero receives a default, while
	// negative values cannot advance the scheduler.
	for _, interval := range []struct {
		key   string
		value time.Duration
	}{
		{"watch.interval", c.Watch.Interval.Duration},
		{"alerts.every", c.Alerts.Every.Duration},
		{"alerts.pagerduty.every", c.Alerts.PagerDuty.Every.Duration},
		{"supervisor.loopInterval", c.Supervisor.LoopInterval.Duration},
		{"supervisor.unmergedWindow", c.Supervisor.UnmergedWindow.Duration},
	} {
		if interval.value < 0 {
			report(fmt.Errorf("%s: %s is negative", interval.key, interval.value))
		}
	}

	for key, paths := range map[string][]string{"projects.dir": {c.Projects.Dir}, "projects.worktrees": c.Projects.Worktrees, "home.places": c.Home.Places} {
		for _, p := range paths {
			if p != "" && (!filepath.IsAbs(p) || filepath.Clean(p) == "/") {
				report(fmt.Errorf("%s: %q: want an absolute directory below the filesystem root (~/ allowed)", key, p))
			}
		}
	}

	if c.LeaseRound.Duration < 0 {
		report(errors.New("leaseRound: want a positive duration"))
	}
	for key, level := range map[string]string{"guide.effort": c.Guide.Effort, "supervisor.effort": c.Supervisor.Effort} {
		if !ValidEffort(level) {
			report(fmt.Errorf("%s: invalid effort %q (low, medium, high, xhigh, max)", key, level))
		}
	}
	for i, r := range c.Agents.Models {
		if r.Harness != "" && !slices.Contains([]string{ClaudeHarness, CodexHarness, AgyHarness, "omp"}, r.Harness) {
			report(fmt.Errorf("agents.models[%d].harness: unknown harness %q", i, r.Harness))
		}
		if !ValidEffort(strings.TrimSpace(r.Effort)) || r.Harness == AgyHarness && r.Effort != "" {
			report(fmt.Errorf("agents.models[%d].effort: invalid effort %q (low, medium, high, xhigh, max)", i, r.Effort))
		}
		if _, err := regexp.Compile(r.Match); err != nil {
			report(fmt.Errorf("agents.models[%d].match: %w", i, err))
		}
		if strings.TrimSpace(r.Match) == "" {
			report(fmt.Errorf("agents.models[%d]: match is required", i))
		}
	}
	seenKinds := map[string]bool{}
	for i, k := range c.Agents.Kinds {
		if !profileNamePattern.MatchString(k.Name) {
			report(fmt.Errorf("agents.kinds[%d].name: want letters, digits, - or _", i))
		} else if seenKinds[k.Name] {
			report(fmt.Errorf("agents.kinds[%d].name: %s is named twice", i, k.Name))
		}
		seenKinds[k.Name] = true
		if strings.TrimSpace(k.Match) == "" {
			report(fmt.Errorf("agents.kinds[%d]: match is required", i))
		} else if _, err := regexp.Compile(k.Match); err != nil {
			report(fmt.Errorf("agents.kinds[%d].match: %w", i, err))
		}
		if k.MaxBusy < 0 || k.MinInterval.Duration < 0 || k.DailySpend < 0 {
			report(fmt.Errorf("agents.kinds[%d]: maxBusy, minInterval and dailySpend must not be negative", i))
		}
	}
	for kind, p := range c.Capacity.TaskPolicies {
		if strings.TrimSpace(kind) == "" || p.MinClass < 0 || p.MaxCost < 0 {
			report(fmt.Errorf("capacity.taskPolicies.%s: nonempty kind and nonnegative class and cost required", kind))
		}
	}
	if err := c.Run.Remote.Validate(); err != nil {
		report(err)
	}
	if c.Run.Tail != "" && c.Run.Tail != "off" && !runTailRe.MatchString(c.Run.Tail) {
		report(fmt.Errorf("run.tail: want a size with an optional K, M, G or T suffix (256K) or off"))
	}
	if c.Disk.Prune != "" && c.Disk.Prune != DiskPruneAuto && c.Disk.Prune != DiskPruneAsk {
		report(fmt.Errorf("disk.prune: want auto or ask"))
	}
	for _, p := range c.Disk.TrashDirs {
		if !filepath.IsAbs(p) || filepath.Clean(p) == string(filepath.Separator) {
			report(fmt.Errorf("disk.trashDirs: each entry must be an absolute trash directory, not a filesystem root"))
		}
	}
	for _, n := range c.Disk.BuildDirs {
		if n == "" || n == "." || filepath.IsAbs(n) || strings.Contains(n, "..") || strings.Contains(n, ".git") {
			report(fmt.Errorf("disk.buildDirs: invalid directory %q", n))
		}
	}
	if r := c.Disk.Holders.Root; r != "" && (!filepath.IsAbs(r) || filepath.Clean(r) == string(filepath.Separator)) {
		report(fmt.Errorf("disk.holders.root: must be an absolute directory, not a filesystem root"))
	}
	if h := c.Disk.Holders; h.StaleAfter.Duration < 0 || h.AsideRetention.Duration < 0 || h.Every.Duration < 0 {
		report(fmt.Errorf("disk.holders: staleAfter, asideRetention and every take no negative duration"))
	}
	for _, n := range append(slices.Clone(c.Disk.Holders.AsideDirs), c.Disk.Holders.Keep...) {
		if n == "" || n == "." || strings.ContainsRune(n, filepath.Separator) || strings.Contains(n, "..") {
			report(fmt.Errorf("disk.holders: %q is no entry name of the root", n))
		}
	}
	validateDiskCachesEach(c.Disk.Caches, report)
	for _, o := range c.Disk.BuildOutputs {
		if !filepath.IsAbs(o.Path) || !filepath.IsAbs(o.Worktree) || filepath.Clean(o.Path) == string(filepath.Separator) || o.Path == o.Worktree || strings.HasPrefix(o.Worktree, filepath.Clean(o.Path)+string(filepath.Separator)) {
			report(fmt.Errorf("disk.buildOutputs: output and owning worktree must be distinct absolute paths"))
		}
	}
	if err := c.GitHub.WorkerApp.Validate(); err != nil {
		report(err)
	}
	if err := execution.ValidateMCP(c.Agents.RemoteMCP); err != nil {
		report(fmt.Errorf("agents.remoteMCP: %w", err))
	}
	if b := c.Agents.PolicyBundle; b != nil && (!filepath.IsAbs(b.Root) || len(b.Sources) == 0) {
		report(fmt.Errorf("agents.policyBundle: an absolute root and at least one source are required"))
	}
	identities := map[execution.HostID]bool{}
	for selector, host := range c.Agents.Hosts {
		if selector == "local" || !execution.ValidIdentity(selector) || identities[host.Identity] {
			report(errors.New("agents.hosts requires unique identities and selectors; local is reserved"))
		}
		if err := host.ValidateCatalog(); err != nil {
			report(fmt.Errorf("agents.hosts.%s: %w", selector, err))
		}
		identities[host.Identity] = true
	}
	if c.Agents.Execution != nil {
		if err := c.Agents.Execution.Validate(); err != nil {
			report(fmt.Errorf("agents.execution: %w", err))
		}
	}
	for name := range c.Agents.Profiles {
		if !profileNamePattern.MatchString(name) {
			report(fmt.Errorf("agents.profiles: %q is no profile name (letters, digits, - and _)", name))
		}
	}
	for key, name := range map[string]string{"agents.profile": c.Agents.Profile, "guide.profile": c.Guide.Profile} {
		if _, ok := c.Agents.ProfileFor(name); name != "" && !ok {
			report(fmt.Errorf("%s: %q is no profile: name one of agents.profiles or %s", key, name, MinimalProfile))
		}
	}
	if m := c.Agents.Mode; m != "" && m != ModeDesktop && m != ModeBackground {
		report(fmt.Errorf("agents.mode: %q: want %s or %s", m, ModeDesktop, ModeBackground))
	}
	if g := c.Agents.Shell.Globs; g != "" && g != GlobsLiteral && g != GlobsShell {
		report(fmt.Errorf("agents.shell.globs: %q: want %s or %s", g, GlobsLiteral, GlobsShell))
	}
	for _, n := range c.Agents.Shell.Unalias {
		if !commandName.MatchString(n) {
			report(fmt.Errorf("agents.shell.unalias: %q is no command name", n))
		}
	}
	for _, n := range c.Secret.UnlockCommands {
		if !commandName.MatchString(n) {
			report(fmt.Errorf("secret.unlockCommands: %q is no command name", n))
		}
	}
	for _, f := range c.Secret.Files {
		if _, err := filepath.Match(f, ""); err != nil || !filepath.IsAbs(f) {
			report(fmt.Errorf("secret.files: %q is no absolute path or glob (~/ allowed)", f))
		}
	}
	for i, id := range c.Secret.AgeIdentities {
		switch {
		case id.Recipient == "" && id.PathRegex == "":
			report(fmt.Errorf("secret.ageIdentities[%d]: name a recipient or a pathRegex", i))
		case id.Recipient != "" && !strings.HasPrefix(id.Recipient, "age1"):
			report(fmt.Errorf("secret.ageIdentities[%d]: recipient %q is no age recipient (age1…)", i, id.Recipient))
		case !strings.HasPrefix(id.Ref, "op://") && !strings.HasPrefix(id.Ref, "file:///") && !strings.HasPrefix(id.Ref, "store://"):
			report(fmt.Errorf("secret.ageIdentities[%d]: ref %q: want op://<vault>/<item>/<field>, file:///<absolute path> or store://[<entry>]", i, id.Ref))
		}
		if _, err := regexp.Compile(id.PathRegex); err != nil {
			report(fmt.Errorf("secret.ageIdentities[%d]: pathRegex: %w", i, err))
		}
		if rest, ok := strings.CutPrefix(id.Ref, "op://"); ok {
			if vault, _, _ := strings.Cut(rest, "/"); vault != c.Secret.Vault && !slices.Contains(c.Secret.AgeVaults, vault) {
				report(fmt.Errorf("secret.ageIdentities[%d]: ref %q: the vault %q is neither secret.vault nor one of secret.ageVaults", i, id.Ref, vault))
			}
		}
	}
	for _, in := range slices.Sorted(maps.Keys(c.Apps.Secrets)) {
		s := c.Apps.Secrets[in]
		switch {
		case !filepath.IsAbs(s.File):
			report(fmt.Errorf("apps.secrets.%s.file: %q is no absolute path (~/ allowed)", in, s.File))
		case (s.Name == "") != (s.Namespace == ""):
			report(fmt.Errorf("apps.secrets.%s: name and namespace start an absent file's Secret together", in))
		}
	}
	for i, v := range c.Secret.AgeVaults {
		if v == "" || strings.Contains(v, "/") || c.Secret.Vault == "" {
			report(fmt.Errorf("secret.ageVaults[%d]: %q: a vault's name, beside secret.vault", i, v))
		}
	}
	for i, r := range c.Secret.FileRoots {
		if !filepath.IsAbs(r) || r == "/" {
			report(fmt.Errorf("secret.fileRoots[%d]: %q: a directory's absolute path (~/ allowed), not the root", i, r))
		}
	}
	for i, v := range c.Secret.MoveVaults {
		if strings.TrimSpace(v) == "" || strings.ContainsAny(v, "/\x00\r\n") || c.Secret.Vault == "" {
			report(fmt.Errorf("secret.moveVaults[%d]: %q: a vault name requires secret.vault", i, v))
		}
	}
	for i, v := range c.Secret.Vaults {
		if v == "" || strings.Contains(v, "/") || c.Secret.Vault == "" {
			report(fmt.Errorf("secret.vaults[%d]: %q: a vault's name, beside secret.vault", i, v))
		}
	}
	for name, p := range c.Omp.Providers {
		// the message never carries the value someone put there
		if !opRef(p.APIKey) {
			report(fmt.Errorf("omp.providers.%s.apiKey: want op://<vault>/<item>/<field>, the reference of the provider's key: beekeeper hands an agent the key from the vault, never from a file", name))
		}
	}
	for i, r := range c.Outbound.StoreDeny {
		for _, g := range []string{r.Vault, r.Item} {
			if _, err := filepath.Match(g, ""); err != nil {
				report(fmt.Errorf("outbound.storeDeny[%d]: %q: %w", i, g, err))
			}
		}
	}
	for _, g := range c.Hooks.PersonFolders {
		if _, err := filepath.Match(g, ""); err != nil {
			report(fmt.Errorf("hooks.personFolders: %q: %w", g, err))
		}
	}
	for _, g := range c.Outbound.Paths {
		if _, err := filepath.Match(g, ""); err != nil {
			report(fmt.Errorf("outbound.paths: %q: %w", g, err))
		}
	}
	c.Sandbox.validateEach(report)
	if r := c.Reporter; r.Every.Duration != 0 && (r.Every.Duration < time.Minute || r.Brief == "") {
		report(fmt.Errorf("reporter: every %s needs at least a minute and a brief", r.Every.Duration))
	}
	if c.Feedback.Enabled() && !c.Reporter.Enabled() {
		report(fmt.Errorf("feedback.context: the feedback watch reads the reporter's posts: set reporter.every and reporter.brief"))
	}
	if tz := c.Reporter.TZ; tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			report(fmt.Errorf("reporter.tz: %w", err))
		}
	}
	ownerReposEach("reporter.reviews", c.Reporter.Reviews, report)
	ownerReposEach("plans.repositories", c.Plans.Repositories, report)
	for repo, method := range c.Merge.Methods {
		ownerReposEach("merge.methods", []string{repo}, report)
		if method != "squash" && method != "rebase" && method != "merge" {
			report(fmt.Errorf("merge.methods[%s]: expected squash, rebase or merge", repo))
		}
	}
	for repo, timeout := range c.Merge.CITimeouts {
		ownerReposEach("merge.ciTimeouts", []string{repo}, report)
		if timeout.Duration <= 0 {
			report(fmt.Errorf("merge.ciTimeouts.%s: %s is not positive", repo, timeout.Duration))
		}
	}
	ownerReposEach("merge.admin", c.Merge.Admin, report)
	c.Sink.validateEach(report)
	for i, st := range c.Board.Order {
		if st.Name == "" {
			report(fmt.Errorf("board.order[%d]: no name", i))
		}
		if st.Search != "" && (len(st.Status)+len(st.Kind)+len(st.Labels) > 0 || st.SubIssues || st.Unblocked) {
			report(fmt.Errorf("board.order[%d] %q: a search step matches no board fields", i, st.Name))
		}
	}
	if b := c.Teleport.Bot; b.Enabled() {
		for name, value := range map[string]string{"config": b.Config, "certificate": b.Certificate, "kubeconfig": b.Kubeconfig} {
			if !filepath.IsAbs(value) {
				report(fmt.Errorf("teleport.bot.%s: an absolute path is required", name))
			}
		}
	}
	if t := c.Teleport; t.WarnBefore.Duration > t.RenewBefore.Duration {
		report(fmt.Errorf("teleport: warnBefore %s is past renewBefore %s: the watch would warn before the keeper renews", t.WarnBefore.Duration, t.RenewBefore.Duration))
	}
	if t := c.Kube.ContextTemplate; t != "" && !strings.Contains(t, "{installation}") {
		report(fmt.Errorf("kube.contextTemplate: %q has no {installation}", t))
	}
	if c.Supervisor.ReviewRounds < 0 {
		report(fmt.Errorf("supervisor.reviewRounds: %d is fewer than one review round (0 is the default, 1)", c.Supervisor.ReviewRounds))
	}
	if k := c.Supervisor.ClassifierConfidence; k < 0 || k > 1 {
		report(fmt.Errorf("supervisor.classifierConfidence: %v is not between 0 and 1", k))
	}
	c.Supervisor.Ownership.validateEach(func(err error) { report(fmt.Errorf("supervisor.ownership.%w", err)) })
	for _, selector := range c.Supervisor.Hosts {
		if selector == "" || selector == "local" {
			report(fmt.Errorf("supervisor.hosts: %q names no SSH execution host", selector))
		}
		if _, ok := c.Agents.Hosts[selector]; !ok {
			report(fmt.Errorf("supervisor.hosts: %q is no agents.hosts entry", selector))
		}
	}
	if r := c.Guide.Role; r.Skill != "" && r.Instructions != "" {
		report(errors.New("guide: set skill or instructions, not both"))
	}
	if pd := c.Alerts.PagerDuty; pd.Enabled() && len(pd.Services) == 0 {
		report(fmt.Errorf("alerts.pagerduty.services: reading PagerDuty through muster context %s needs the team's service ids", pd.Context))
	}
	if d := c.Alerts.Discover; d.Enabled() && c.Alerts.Team == "" {
		report(errors.New("alerts.discover: discovering the installations that carry the team's HelmReleases needs alerts.team"))
	}
	for _, p := range c.Alerts.Discover.Exclude {
		if _, err := path.Match(p, ""); err != nil {
			report(fmt.Errorf("alerts.discover.exclude: %q: %w", p, err))
		}
	}
	for i, in := range c.Alerts.Installations {
		if in.Name == "" {
			report(fmt.Errorf("alerts.installations[%d]: an installation needs a name", i))
		}
		if in.Floor != "" && !slices.Contains(alerts.Severities, in.Floor) {
			report(fmt.Errorf("alerts.installations[%d]: floor %q is none of %s", i, in.Floor, strings.Join(alerts.Severities, ", ")))
		}
	}
	if n := c.Alerts.Flap.Changes; n < 0 || n == 1 {
		report(fmt.Errorf("alerts.flap.changes: %d; an alert is flapping from its second change on at the earliest", n))
	}
	if p := c.Alerts.PageSeverity; p != "" && !slices.Contains(alerts.Severities, p) {
		report(fmt.Errorf("alerts.pageSeverity: %q is none of %s", p, strings.Join(alerts.Severities, ", ")))
	}
	if g := c.Alerts.OwnerGrace.Duration; g != 0 && g < time.Minute {
		report(fmt.Errorf("alerts.ownerGrace: %s; a page has at least a minute to be owned", g))
	}
	if e := c.Upgrades.Every.Duration; e < 0 {
		report(fmt.Errorf("upgrades.every: %s; an installation is read at most this long apart", e))
	}
	c.Notify.validateEach(report)
	for _, r := range c.Resources {
		if !validResourceName(r) {
			report(fmt.Errorf("resources: %q is not a valid resource name", r))
		}
	}
	if k := c.Capacity; k.Ceiling < 0 {
		report(fmt.Errorf("capacity.ceiling: %d is negative", k.Ceiling))
	}
	if n := c.Supervisor.GraphQLLimit; n < 0 {
		report(fmt.Errorf("supervisor.graphQLLimit: %d is negative", n))
	}
	if c.Capacity.ReviewHeadroom < 0 {
		report(fmt.Errorf("capacity.reviewHeadroom: %d is negative", c.Capacity.ReviewHeadroom))
	}
	if n, ok := c.Capacity.Local.Limit(); ok && n < 0 {
		report(fmt.Errorf("capacity.local.ceiling: %d is negative", n))
	}
	if c.Capacity.SwapPSIMax < 0 {
		report(fmt.Errorf("capacity.swapPSIMax: %g is not a pressure in percent", c.Capacity.SwapPSIMax))
	}
	c.Capacity.Usage.validateNamedEach("capacity.usage", report)
	for _, name := range c.Capacity.ProviderNames() {
		if strings.TrimSpace(name) == "" {
			report(errors.New("capacity.providers: empty provider name"))
		}
		p := c.Capacity.Providers[name]
		if k := strings.TrimSpace(p.Kind); k != "" && k != KindSubscription && k != KindSelfHosted {
			report(fmt.Errorf("capacity.providers.%s.kind: %q is not %s or %s", name, p.Kind, KindSubscription, KindSelfHosted))
		}
		if p.Ceiling < 0 {
			report(fmt.Errorf("capacity.providers.%s.ceiling: %d is negative", name, p.Ceiling))
		}
		if p.SelfHosted() {
			// claude, agy and codex are the subscription harnesses' names: a
			// self-hosted provider under one would be matched by a
			// subscription start's fallback and hand it its room.
			switch strings.ToLower(strings.TrimSpace(name)) {
			case ClaudeHarness, AgyHarness, CodexHarness:
				report(fmt.Errorf("capacity.providers.%s: %s names the %s subscription; a %s provider has another name", name, name, name, KindSelfHosted))
			}
			if p.Enabled() {
				report(fmt.Errorf("capacity.providers.%s: a %s provider reads no subscription usage: remove its command", name, KindSelfHosted))
			}
			if p.hasFields() {
				report(fmt.Errorf("capacity.providers.%s: a %s provider has no usage windows: remove its usage fields", name, KindSelfHosted))
			}
			if p.ServeModeStale.Duration < 0 {
				report(fmt.Errorf("capacity.providers.%s.serveModeStaleAfter: %s is negative", name, p.ServeModeStale.Duration))
			}
			if p.ServeModeSwitchTimeout.Duration < 0 {
				report(fmt.Errorf("capacity.providers.%s.serveModeSwitchTimeout: %s is negative", name, p.ServeModeSwitchTimeout.Duration))
			}
			for _, mode := range slices.Sorted(maps.Keys(p.ServeModeCeilings)) {
				if strings.TrimSpace(mode) == "" {
					report(fmt.Errorf("capacity.providers.%s.serveModeCeilings: empty mode name", name))
				}
				if p.ServeModeCeilings[mode] < 0 {
					report(fmt.Errorf("capacity.providers.%s.serveModeCeilings.%s: %d is negative", name, mode, p.ServeModeCeilings[mode]))
				}
			}
			if len(p.ServeModeCommand) > 0 {
				if strings.TrimSpace(p.ServeModeCommand[0]) == "" {
					report(fmt.Errorf("capacity.providers.%s.serveModeCommand[0]: empty executable", name))
				}
				if len(p.ServeModeCeilings) == 0 && len(p.ServeModeModes) == 0 {
					report(fmt.Errorf("capacity.providers.%s.serveModeCommand: no serveModeCeilings: the command's modes name no ceiling", name))
				}
			} else if len(p.ServeModeCeilings) > 0 || len(p.ServeModeModes) > 0 {
				report(fmt.Errorf("capacity.providers.%s.serveModeCeilings: no serveModeCommand: the ceilings name no mode reader", name))
			}
			if len(p.ServeModeModes) > 0 && len(p.ServeModeCommand) == 0 {
				report(fmt.Errorf("capacity.providers.%s.serveModeModes: no serveModeCommand: the modes name no mode reader", name))
			}
			if len(p.ServeModeSwitch) > 0 && len(p.ServeModeCommand) == 0 {
				report(fmt.Errorf("capacity.providers.%s.serveModeSwitchCommand: no serveModeCommand: the switch has no mode reader", name))
			}
			if len(p.ServeModeSwitch) > 0 && len(p.ServeModeModes) == 0 {
				report(fmt.Errorf("capacity.providers.%s.serveModeSwitchCommand: no serveModeModes: the switch knows no mode to switch into", name))
			}
			if len(p.ServeModeSwitch) > 0 && strings.TrimSpace(p.ServeModeSwitch[0]) == "" {
				report(fmt.Errorf("capacity.providers.%s.serveModeSwitchCommand[0]: empty executable", name))
			}
			if len(p.ServeModeModes) > 0 && len(p.ServeModeSwitch) == 0 {
				report(fmt.Errorf("capacity.providers.%s.serveModeModes: no serveModeSwitchCommand: the modes name no switch", name))
			}
			for _, mode := range slices.Sorted(maps.Keys(p.ServeModeModes)) {
				m := p.ServeModeModes[mode]
				if strings.TrimSpace(mode) == "" {
					report(fmt.Errorf("capacity.providers.%s.serveModeModes: empty mode name", name))
				}
				if m.Sessions < 0 {
					report(fmt.Errorf("capacity.providers.%s.serveModeModes.%s: sessions %d is negative", name, mode, m.Sessions))
				}
				if c, ok := p.ServeModeCeilings[mode]; ok && c != m.Sessions {
					report(fmt.Errorf("capacity.providers.%s.serveModeCeilings.%s: %d contradicts serveModeModes.%s.sessions %d", name, mode, c, mode, m.Sessions))
				}
				for _, n := range m.Nodes {
					if !validResourceName(n) {
						report(fmt.Errorf("capacity.providers.%s.serveModeModes.%s: %q is not a valid resource name", name, mode, n))
					}
				}
			}
			continue
		}
		if len(p.ServeModeCommand) > 0 || len(p.ServeModeCeilings) > 0 || len(p.ServeModeModes) > 0 ||
			len(p.ServeModeSwitch) > 0 || p.ServeModeSwitchTimeout.Duration != 0 {
			report(fmt.Errorf("capacity.providers.%s: serveModeCommand, serveModeCeilings, serveModeModes and serveModeSwitchCommand belong to a %s provider", name, KindSelfHosted))
		}
		p.validateNamedEach("capacity.providers."+name, report)
	}
	for i, ch := range c.Capacity.Choices {
		if !ValidEffort(strings.TrimSpace(ch.Effort)) || ch.Harness == AgyHarness && ch.Effort != "" {
			report(fmt.Errorf("capacity.choices[%d].effort: unsupported effort %q for %s", i, ch.Effort, ch.Harness))
		}
		if ch.Class < 0 || ch.Cost < 0 || ch.ReviewReserve < 0 || ch.ReviewReserve > 100 {
			report(fmt.Errorf("capacity.choices[%d]: class and cost must be nonnegative, reviewReserve in [0, 100]", i))
		}
		switch {
		case strings.TrimSpace(ch.Harness) == "":
			report(fmt.Errorf("capacity.choices[%d]: harness is required", i))
		case strings.TrimSpace(ch.Model) == "":
			report(fmt.Errorf("capacity.choices[%d]: model is required", i))
		case ch.fiveHourMaxSet && !(ch.FiveHourMax > 0 && ch.FiveHourMax <= 100):
			report(fmt.Errorf("capacity.choices[%d]: fiveHourMax %g is not in (0, 100]", i, ch.FiveHourMax))
		case ch.sevenDayMaxSet && !(ch.SevenDayMax > 0 && ch.SevenDayMax <= 100):
			report(fmt.Errorf("capacity.choices[%d]: sevenDayMax %g is not in (0, 100]", i, ch.SevenDayMax))
		case ch.paceMarginSet && (ch.PaceMargin < -100 || ch.PaceMargin > 100):
			report(fmt.Errorf("capacity.choices[%d]: paceMargin %g is not in [-100, 100]", i, ch.PaceMargin))
		}
		prov := ch.EffectiveProvider()
		if p, ok := c.Capacity.Providers[prov]; ok && p.SelfHosted() && !strings.EqualFold(strings.TrimSpace(ch.Harness), OmpHarness) {
			report(fmt.Errorf("capacity.choices[%d]: harness %q is a subscription; %s is a %s provider", i, ch.Harness, prov, KindSelfHosted))
		}
		if _, ok := c.Capacity.Providers[prov]; !ok {
			report(fmt.Errorf("capacity.choices[%d]: provider %q is not in capacity.providers", i, prov))
		}
	}
	if w := c.Watch.SwapWindow.Duration; w != 0 && w < MinSwapSpan {
		report(fmt.Errorf("watch.swapWindow: %s is under the %s a swap growth rate is measured over", w, MinSwapSpan))
	}
	if a := c.Agents; a.LastStepCeiling != 0 && a.LastStepCeiling <= a.RelayAt {
		report(fmt.Errorf("agents.lastStepCeiling: %d is not above relayAt %d", a.LastStepCeiling, a.RelayAt))
	}
	c.validateLabsEach(report)
	if o := c.Ollama; (o.URL != "" || c.Lemonade.URL != "") && (o.BudgetGiB < 1 || o.BudgetGiB > o.MaxBudgetGiB) {
		report(fmt.Errorf("ollama.budgetGiB: %d is not between 1 and maxBudgetGiB %d", o.BudgetGiB, o.MaxBudgetGiB))
	}
	checks := map[string]bool{}
	if len(c.Fatal.Checks) > 16 || len(c.Fatal.Repositories) > 16 {
		report(fmt.Errorf("fatal: at most 16 checks and 16 repositories"))
	}
	for i, check := range c.Fatal.Checks {
		if check.Name == "" || len(check.Probe) == 0 || check.Probe[0] == "" || checks[check.Name] {
			report(fmt.Errorf("fatal.checks[%d]: unique name and probe required", i))
		}
		if check.Harness != "" && !slices.Contains([]string{ClaudeHarness, OmpHarness, AgyHarness, CodexHarness}, check.Harness) {
			report(fmt.Errorf("fatal.checks[%d]: unknown harness", i))
		}
		checks[check.Name] = true
		if check.Repository != "" && !validFatalRepository(check.Repository) {
			report(fmt.Errorf("fatal.checks[%d]: invalid repository", i))
		}
	}
	for _, repo := range c.Fatal.Repositories {
		if !validFatalRepository(repo) {
			report(fmt.Errorf("fatal.repositories: expected owner/repository"))
		}
	}
	for repo, dir := range c.Fatal.Checkouts {
		if !validFatalRepository(repo) || dir == "" {
			report(fmt.Errorf("fatal.checkouts: valid repository and checkout required"))
		}
	}
	faults := map[string]bool{}
	for i, f := range c.Doctor.Faults {
		if f.Name == "" || f.Probe == "" || f.Remedy == "" || faults[f.Name] {
			report(fmt.Errorf("doctor.faults[%d]: a fault needs a name of its own, a probe and a remedy", i))
		}
		faults[f.Name] = true
	}
	names, repos := map[string]bool{}, map[string]string{}
	for i, l := range c.Lanes {
		if l.Name == "" || len(l.Repositories) == 0 || strings.Contains(l.Name, "/") {
			report(fmt.Errorf("lanes[%d]: a lane needs a name without a slash and repositories", i))
		}
		if names[l.Name] {
			report(fmt.Errorf("lanes: %q is named twice", l.Name))
		}
		names[l.Name] = true
		for _, r := range l.Repositories {
			if prev, ok := repos[r]; ok {
				report(fmt.Errorf("lanes: %s is in both %q and %q", r, prev, l.Name))
			}
			repos[r] = l.Name
		}
	}
}

// machineKinds are the kinds that notified before the machine's lines went
// to the supervisor only: a config naming one is told so.
var machineKinds = []string{"oom-line", "oom-kill"}

func notifyKind(field, k string) error {
	if slices.Contains(machineKinds, k) {
		return fmt.Errorf("%s: %q is a machine line, said to the supervisor's watch and never notified: remove it", field, k)
	}
	if !slices.Contains(notify.Kinds, k) {
		return fmt.Errorf("%s: %q is none of %s", field, k, strings.Join(notify.Kinds, ", "))
	}
	return nil
}

func (n Notify) validateEach(report func(error)) {
	for _, k := range n.Kinds {
		if err := notifyKind("notify.kinds", k); err != nil {
			report(err)
		}
	}
	for k, u := range n.Urgency {
		if err := notifyKind("notify.urgency", k); err != nil {
			report(err)
		}
		if !slices.Contains(notify.Urgencies, u) {
			report(fmt.Errorf("notify.urgency.%s: %q is none of %s", k, u, strings.Join(notify.Urgencies, ", ")))
		}
	}
	if _, err := notify.ParseQuietHours(n.QuietHours); err != nil {
		report(fmt.Errorf("notify.quietHours: %w", err))
	}
}

// validResourceName requires one local filename component. Leading dots and
// built-in lease names are reserved; serving-mode nodes share this contract
// because they become lease resources even when resources does not list them.
func validResourceName(name string) bool {
	return strings.TrimSpace(name) != "" && filepath.IsLocal(name) &&
		filepath.Base(name) == name && name[0] != '.' &&
		name != Browser && name != ModelServer
}

// Leasable returns every resource a session can lease: the configured ones,
// the installations under alerts, the nodes a self-hosted serving mode
// occupies, the model server when one is watched, the browser last.
func (c *Config) Leasable() []string {
	out := slices.Clone(c.Resources)
	for _, in := range c.Alerts.Installations {
		if !slices.Contains(out, in.Name) {
			out = append(out, in.Name)
		}
	}
	for _, n := range c.Capacity.serveModeNodeNames() {
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	if c.Ollama.URL != "" || c.Lemonade.URL != "" {
		out = append(out, ModelServer)
	}
	return append(out, Browser)
}

// validateLabsEach checks that every lab lease is a configured resource and
// that no two lab leases map one kind cluster.
func (c *Config) validateLabsEach(report func(error)) {
	by := map[string]string{}
	for _, res := range slices.Sorted(maps.Keys(c.Labs)) {
		cl := c.Labs[res]
		if !slices.Contains(c.Resources, res) {
			report(fmt.Errorf("labs: %q is not one of the resources %s", res, strings.Join(c.Resources, ", ")))
		}
		if cl == "" {
			report(fmt.Errorf("labs: %q names no kind cluster", res))
		}
		if other, ok := by[cl]; ok {
			report(fmt.Errorf("labs: %q and %q both map the kind cluster %q", other, res, cl))
		}
		by[cl] = res
	}
}

// LabCluster is the kind cluster the lab lease res stands for, "" when res
// is no lab lease.
func (c *Config) LabCluster(res string) string { return c.Labs[res] }

// LabLease is the lab lease that stands for the kind cluster cluster, ""
// when none maps it.
func (c *Config) LabLease(cluster string) string {
	for res, cl := range c.Labs {
		if cl == cluster {
			return res
		}
	}
	return ""
}

// IsLeasable reports whether name is one of the resources returned by Leasable.
func (c *Config) IsLeasable(name string) bool {
	return slices.Contains(c.Leasable(), name)
}

// ownerReposEach reports entries of field that are not owner/repo.
func validFatalRepository(repo string) bool {
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(repo) {
		return false
	}
	for _, part := range strings.Split(repo, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

func ownerReposEach(field string, repos []string, report func(error)) {
	for _, r := range repos {
		if o, name, ok := strings.Cut(r, "/"); !ok || o == "" || name == "" || strings.ContainsAny(name, "/ ") {
			report(fmt.Errorf("%s: %q is not owner/repo", field, r))
		}
	}
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
