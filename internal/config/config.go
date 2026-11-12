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
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
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
	"github.com/giantswarm/beekeeper/internal/remotebuild"
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

// Kube configures the kube guard and how an installation's name becomes its
// kube context.
type Kube struct {
	// APIReads are operator-provisioned API identities and exact read RPCs.
	APIReads map[string]APIRead `yaml:"apiReads"`
	// Production is the installation whose clusters agents never write to:
	// a context or cluster name with it as a component. Empty, the kube
	// guard is off unless ProtectedContexts is set.
	Production string `yaml:"production"`
	// ProtectedContexts lists additional production-class contexts (including
	// full test installations) whose clusters agents must not change directly.
	ProtectedContexts []string `yaml:"protectedContexts"`
	// ReadOnlyExecQueries lists exact SELECT queries allowed through psql -X -c.
	ReadOnlyExecQueries []string `yaml:"readOnlyExecQueries"`
	// ProductionNoteDeletes lets an answered note authorise one literal
	// production kubectl delete. Off by default, and unsafe to switch on where
	// agents can record answers: nothing proves a person gave the answer, so an
	// agent that can record one can authorise its own deletion.
	ProductionNoteDeletes bool `yaml:"productionNoteDeletes"`
	// ContextTemplate is the kube context of an installation, with
	// {installation} for its name ("login.example.com-{installation}");
	// empty, an installation's context is its name or the one ending in
	// @<name>.
	ContextTemplate string `yaml:"contextTemplate"`
	// Kubeconfig lists the kubeconfig files beekeeper's own reads resolve
	// a context in, merged as kubectl merges a KUBECONFIG list (~/
	// allowed): the k8s:// source of beekeeper secret, whose Secret read a
	// kind lab's context makes through kind instead. Empty, the files
	// KUBECONFIG lists, else the machine kubeconfig ~/.kube/config.
	Kubeconfig []string `yaml:"kubeconfig"`
}

// APIRead binds a reader identity to one TLS endpoint and token audience.
// The API must enforce read-only authorization for this service account.
type APIRead struct {
	Context        string   `yaml:"context"`
	Namespace      string   `yaml:"namespace"`
	ServiceAccount string   `yaml:"serviceAccount"`
	Audience       string   `yaml:"audience"`
	Endpoint       string   `yaml:"endpoint"`
	Methods        []string `yaml:"methods"`
}

// DiskPruneAuto and DiskPruneAsk select the automatic disk step or the
// existing manual commands and doctor trims.
const (
	DiskPruneAuto = "auto"
	DiskPruneAsk  = "ask"
)

// Disk configures automatic disk reclamation. Ask retains the existing
// on-demand commands and doctor trims, without the automatic disk step.
type Disk struct {
	Prune string   `yaml:"prune"`
	Every Duration `yaml:"every"`
	// SccacheMaxGiB caps sccache's local cache: a scheduled run trims it least
	// recently used first to three quarters of the cap; negative disables the cap.
	SccacheMaxGiB int `yaml:"sccacheMaxGiB"`
	// Checkouts locates main checkouts whose harness worktrees are inspected.
	Checkouts []string `yaml:"checkouts"`
	TrashDirs []string `yaml:"trashDirs"`
	// BuildDirs adds regenerable directory names inside worktrees.
	BuildDirs []string `yaml:"buildDirs"`
	// BuildOutputs associates external build output with its owning worktree.
	BuildOutputs []BuildOutput `yaml:"buildOutputs"`
	// Caches are the disk step's cache policies. Beekeeper's own caches are
	// its default entries (DiskCacheGo, DiskCacheSccache, DiskCacheTmp): an
	// entry of the same name changes them, mode off drops them.
	Caches []DiskCache `yaml:"caches"`
	// Holders is the lifecycle of the state home's holders (beekeeper disk
	// holders): it lists them and what is eligible for removal, removing
	// nothing.
	Holders Holders `yaml:"holders"`
}

// Holders configures the listing of the state home's holders: each entry of
// Root (default the parent of stateDir, ~/.local/state), and each entry of
// its AsideDirs one by one. A holder unused for StaleAfter (default 72h), or
// an aside entry kept past AsideRetention (default 168h), whose task is not
// open and which no process uses, is eligible for removal; the watch says
// STATE HOLDERS once the eligible ones reach WarnGiB (default 10; negative,
// never), looking every Every (default 6h).
type Holders struct {
	Root           string   `yaml:"root"`
	StaleAfter     Duration `yaml:"staleAfter"`
	AsideRetention Duration `yaml:"asideRetention"`
	// AsideDirs are the root's directories holding moved-aside material
	// (default aside, moved-aside).
	AsideDirs []string `yaml:"asideDirs"`
	// Keep names the root's entries never eligible (model weights, costly
	// to fetch again).
	Keep    []string `yaml:"keep"`
	WarnGiB int      `yaml:"warnGiB"`
	Every   Duration `yaml:"every"`
}

// Cache modes of the disk step. Entries removes a root's direct children
// whole, files its single files, command runs the tool's own cleaner; go and sccache are the built-in Go build
// cache and sccache trims; off drops an entry.
const (
	DiskCacheEntries = "entries"
	DiskCacheFiles   = "files"
	DiskCacheCommand = "command"
	DiskCacheModeGo  = "go"
	DiskCacheModeSc  = "sccache"
	DiskCacheOff     = "off"
)

// Names of the built-in cache entries.
const (
	DiskCacheGo          = "go-build"
	DiskCacheSccache     = "sccache"
	DiskCacheTmp         = "beekeeper-tmp"
	DiskCacheTranscripts = "transcripts"
)

// DiskCache is one cache policy of the disk step. Path is absolute (or
// ~/...) and may be a glob: each match is a cache root. In entries mode a
// root's direct children are the units, in files mode its files. A unit
// untouched for OlderThan goes; past MaxGiB the least recently written go
// until the root fits; under disk pressure a Regenerable cache keeps only
// what was written within the hour. Nothing written within the hour, nothing
// a process uses and nothing holding a git checkout is ever removed.
type DiskCache struct {
	// Name names the entry in reports (default its path).
	Name        string   `yaml:"name"`
	Path        string   `yaml:"path"`
	Mode        string   `yaml:"mode"`
	OlderThan   Duration `yaml:"olderThan"`
	MaxGiB      float64  `yaml:"maxGiB"`
	Regenerable bool     `yaml:"regenerable"`
	// Command is mode command's cleaner, the tool's own ("uv cache prune"):
	// split into words and run without a shell in the cache root, once the
	// root holds more than MaxGiB and under disk pressure (no MaxGiB: under
	// pressure only), never while a process references the root.
	Command string `yaml:"command"`
}

// MaxBytes is the size budget in bytes; 0 is none.
func (d DiskCache) MaxBytes() int64 { return int64(d.MaxGiB * (1 << 30)) }

// Builtin reports whether name is one of beekeeper's own cache entries.
func (DiskCache) Builtin(name string) bool {
	return name == DiskCacheGo || name == DiskCacheSccache || name == DiskCacheTmp || name == DiskCacheTranscripts || name == "agy-conversations" || name == "agy-brain"
}

// BuildOutput is explicitly disposable output of the task in Worktree.
// It is reclaimed only once that worktree's task ended and no process uses it.
type BuildOutput struct {
	Path     string `yaml:"path"`
	Worktree string `yaml:"worktree"`
}

// Auto reports whether the disk step runs without a person's answer.
func (d Disk) Auto() bool { return d.Prune == "" || d.Prune == DiskPruneAuto }

// Teleport configures either the supervised Machine ID bot or the periodic
// SSO keeper, which renews under the browser lease.
type Teleport struct {
	// Bot selects headless Machine ID operation instead of SSO renewal.
	Bot TeleportBot `yaml:"bot"`
	// Proxy is tsh login's --proxy. Without a proxy or a bot, no keeper
	// runs and the watch reads no identity.
	Proxy string `yaml:"proxy"`
	// Auth is tsh login's --auth, the SSO connector; empty, the cluster's
	// default.
	Auth string `yaml:"auth"`
	// Tsh is the tsh binary (default tsh).
	Tsh string `yaml:"tsh"`
	// Home is the profile directory the kube contexts' tsh reads (default
	// $TELEPORT_HOME, else ~/.tsh).
	Home string `yaml:"home"`
	// RenewBefore is how long before the expiry the keeper renews (default
	// 90m); WarnBefore is when the watch says the login is about to expire
	// (default 1h), at most RenewBefore.
	RenewBefore Duration `yaml:"renewBefore"`
	WarnBefore  Duration `yaml:"warnBefore"`
	// LoginTimeout bounds one login, the browser's round trip included
	// (default 3m).
	LoginTimeout Duration `yaml:"loginTimeout"`
	// Every is how often the keeper's timer checks the expiry (default 10m).
	Every Duration `yaml:"every"`
}

// TeleportBot points at an administrator-provisioned tbot configuration and outputs.
// The configuration, rather than beekeeper's config or argv, holds the join token.
type TeleportBot struct {
	Config      string `yaml:"config"`
	Binary      string `yaml:"binary"`
	Certificate string `yaml:"certificate"`
	Kubeconfig  string `yaml:"kubeconfig"`
}

// Enabled reports whether Machine ID replaces SSO.
func (b TeleportBot) Enabled() bool { return b.Config != "" }

// Enabled reports whether a keeper is configured.
func (t Teleport) Enabled() bool { return t.Proxy != "" || t.Bot.Enabled() }

func (t *Teleport) defaults(home string) {
	setStr(&t.Bot.Binary, "tbot")
	t.Bot.Config = homePath(home, t.Bot.Config)
	t.Bot.Certificate = homePath(home, t.Bot.Certificate)
	t.Bot.Kubeconfig = homePath(home, t.Bot.Kubeconfig)
	setStr(&t.Tsh, "tsh")
	setStr(&t.Home, os.Getenv("TELEPORT_HOME"))
	setStr(&t.Home, filepath.Join(home, ".tsh"))
	setDur(&t.RenewBefore, 90*time.Minute)
	setDur(&t.WarnBefore, time.Hour)
	setDur(&t.LoginTimeout, 3*time.Minute)
	setDur(&t.Every, 10*time.Minute)
}

// Context is the templated context of installation, "" without a template.
func (k Kube) Context(installation string) string {
	if k.ContextTemplate == "" {
		return ""
	}
	return strings.ReplaceAll(k.ContextTemplate, "{installation}", installation)
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

// Units configures the transient user services beekeeper starts (a gated
// merge's child, a wake's turn, a reopen).
type Units struct {
	// Quiet counts every end of such a unit as success, any exit status
	// and a kill by SIGKILL, SIGTERM or SIGABRT included: the run's own
	// files still record the real exit. It is for an isolated harness
	// state, whose stub tools and deliberate kills would otherwise fail
	// units in the person's user manager; the desk's own configuration
	// leaves it off, so a real tooling fault fails its unit.
	Quiet bool `yaml:"quiet"`
}

// Alerts configures the reading of the installations' Alertmanagers: watch
// reads them every Every and prints NEW and RESOLVED lines, snapshot adds
// the current set as a section.
type Alerts struct {
	// Installations are read always; the discovered ones (Discover) and a
	// held lease whose name resolves to a kube context are read too.
	Installations []Installation `yaml:"installations"`
	// Ignore are alert names that never appear; setting it replaces the
	// default (Watchdog).
	Ignore []string `yaml:"ignore"`
	// Tenant is the Mimir tenant (X-Scope-OrgID) whose Alertmanager is read
	// first; empty, only a plain Alertmanager is read.
	Tenant string `yaml:"tenant"`
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
	// Kubeconfig lists the kubeconfig files (~/ allowed) the installations'
	// contexts are resolved and read in, passed to kubectl as KUBECONFIG:
	// a kubeconfig of the watch's own, logged into with KUBECONFIG=<file>
	// tsh kube login <cluster>, so an installation reads without a context
	// in the machine kubeconfig. The machine kubeconfig is merged first
	// unless the list names it. Empty, kubectl's own default.
	Kubeconfig []string `yaml:"kubeconfig"`
	// Flap is the flap damper.
	Flap Flap `yaml:"flap"`
	// Quiet are the rules whose alerts' changes are no wake-up: the watch
	// logs them (watch.quiet) instead of printing them. A rule never
	// quiets an alert of the team, one on an installation in play (leased,
	// claimed or merged into within the last half hour, or with a merge
	// settling), or a page unless it names a cluster. Setting it replaces
	// the default: once Team is set, every other team's and team-less
	// notify alert ({severity: notify}), else none. An alert back after a
	// reading that missed it, with its old start, is quiet too.
	Quiet []alerts.Quiet `yaml:"quiet"`
	// PageSeverity is the severity that pages the on-call person (default
	// page): a firing alert at it that no session owns (alerts own) for
	// OwnerGrace is a PAGE UNOWNED line, again every OwnerGrace.
	PageSeverity string `yaml:"pageSeverity"`
	// OwnerGrace is how long a page may go unowned (default 15m).
	OwnerGrace Duration `yaml:"ownerGrace"`
	// PagerDuty is the second alert source: the team's PagerDuty services'
	// open incidents.
	PagerDuty PagerDuty `yaml:"pagerduty"`
	// Discover adds every installation that carries the team's
	// HelmReleases.
	Discover Discover `yaml:"discover"`
}

// Discover finds the installations to read beyond alerts.installations:
// every cluster Teleport lists (tsh kube ls, teleport.tsh on
// teleport.home), but the excluded ones, that carries a HelmRelease
// labelled Label=alerts.team. One without a kube context is logged into the
// first file of alerts.kubeconfig that is not the machine kubeconfig
// (KUBECONFIG=<file> tsh kube login <cluster>), so its reads need no
// context in the person's kubeconfig. The watch discovers every Every and
// keeps the result (alerts-discovery.json in the state directory), which
// every reading of the alerts adds to its installations.
type Discover struct {
	// Every is the discovery's interval; unset, nothing is discovered.
	Every Duration `yaml:"every"`
	// Label is the HelmRelease label naming the team (default
	// application.giantswarm.io/team).
	Label string `yaml:"label"`
	// Exclude are the glob patterns of Teleport cluster names never read
	// (default "*-*", a workload cluster's <installation>-<cluster>, and
	// "*.*", the proxy's own).
	Exclude []string `yaml:"exclude"`
}

// Enabled says whether installations are discovered.
func (d Discover) Enabled() bool { return d.Every.Duration > 0 }

// PagerDuty configures the reading of the open incidents of the team's
// PagerDuty services, through muster's read-only PagerDuty tools as the
// person, signed in to the muster context: beekeeper holds no PagerDuty
// token. The watch reads them every Every and prints PAGERDUTY NEW,
// ACKNOWLEDGED and RESOLVED lines; alerts.team marks them.
type PagerDuty struct {
	// Context is the muster context whose PagerDuty server is read; unset,
	// PagerDuty is not read. The binary and the call timeout are central's.
	Context string `yaml:"context"`
	// Server is the name muster registered the PagerDuty MCP server under:
	// its tools are x_<server>_<tool> (default pd).
	Server string `yaml:"server"`
	// Services are the ids of the team's PagerDuty services.
	Services []string `yaml:"services"`
	// Every is the reading interval (default 1m): a page is a watch line
	// within it.
	Every Duration `yaml:"every"`
}

// Enabled says whether PagerDuty is read.
func (p PagerDuty) Enabled() bool { return p.Context != "" }

// Upgrades paces the reading of alerts.installations' Cluster API clusters
// for running upgrades: each installation is read every Every, and every
// watch.interval while an upgrade runs on it or holds it.
type Upgrades struct {
	// Every is how old an installation's last reading may grow before it is
	// read again (default 5m).
	Every Duration `yaml:"every"`
}

// Flap holds back an alert that changes too often: its Changes-th NEW or
// RESOLVED within Window is one FLAPPING line, as is a re-fire within Window
// after it resolved, and its changes print nothing until it has been stable
// for Window.
type Flap struct {
	Changes int      `yaml:"changes"`
	Window  Duration `yaml:"window"`
}

// Installation is an installation and, optionally, the kube context that
// reaches it and the lowest severity printed of its alerts; written as a
// name alone or as {name, context, floor}. Without a context it is
// kube.contextTemplate's, else <name>, else the one ending in @<name>.
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

// OtherTeamsNotify are other teams' and team-less notify alerts: notify
// pages nobody, and the rule never quiets the team's alerts, a page or an
// installation in play.
var OtherTeamsNotify = alerts.Quiet{Severity: "notify"}

// DefaultQuietFor are the default quiet rules of team: OtherTeamsNotify
// once there is a team whose alerts it leaves out, else none.
func DefaultQuietFor(team string) []alerts.Quiet {
	if team == "" {
		return []alerts.Quiet{}
	}
	return []alerts.Quiet{OtherTeamsNotify}
}

// DefaultQuietSessions are the sessions of beekeeper's own tests.
var DefaultQuietSessions = []string{"test: *"}

// DefaultIgnore are the alerts that always fire: Alertmanager's dead man's
// switch.
var DefaultIgnore = []string{"Watchdog"}

// GitHub configures the budget reading.
// WorkerGitHub is coordinator-only user-to-server GitHub App authority. The
// refresh token is a vault or SOPS reference on the coordinator, never a raw
// value or a file copied to an execution host.
type WorkerGitHub struct {
	ClientID     string `yaml:"clientID"`
	RefreshToken string `yaml:"refreshToken"`
}

func (w *WorkerGitHub) Validate() error {
	if w == nil {
		return nil
	}
	if !execution.ValidIdentity(w.ClientID) || !strings.Contains(w.RefreshToken, "://") || strings.ContainsAny(w.RefreshToken, "\r\n") {
		return errors.New("github.workerApp requires clientID and refreshToken reference")
	}
	// The full reference is parsed at use sites. Config cannot import secret
	// (its sandbox depends on config), but must reject read-only schemes here.
	writable := false
	if rest, ok := strings.CutPrefix(w.RefreshToken, "op://"); ok {
		parts := strings.Split(rest, "/")
		writable = len(parts) >= 3 && !slices.Contains(parts, "")
	}
	if rest, ok := strings.CutPrefix(w.RefreshToken, "sops://"); ok {
		file, path, found := strings.Cut(rest, "#")
		writable = found && file != "" && path != ""
	}
	if !writable {
		return errors.New("github.workerApp.refreshToken must name a writable op:// or sops:// secret reference")
	}
	return nil
}

type GitHub struct {
	WorkerApp *WorkerGitHub `yaml:"workerApp,omitempty"`

	// Floor is the remaining core budget under which GitHub work stops.
	Floor int `yaml:"floor"`
	// UrgentBound is the budget an urgent merge keeps for itself under the
	// floor (lanes urgent): it runs while at least this much remains.
	UrgentBound int `yaml:"urgentBound"`
	// ProbeRepo is the repository whose conditional GET reads the budget
	// headers; any repository the token can read (default: beekeeper's own).
	ProbeRepo string `yaml:"probeRepo"`
	// AllowedOrgs are the organizations and accounts whose repositories an
	// agent writes to (default giantswarm); the hook refuses every GitHub
	// write to any other repository. An empty list turns the guard off.
	AllowedOrgs []string `yaml:"allowedOrgs"`
	// PublicOnlyOwners restricts these owners to verified public repositories
	// at task admission and write boundaries, independently of AllowedOrgs.
	PublicOnlyOwners []string `yaml:"publicOnlyOwners"`
	// UpstreamReview is where a change meant for an upstream project is
	// queued for a human's decision (an issue, owner/repo#n), named in the
	// refusal; unset, the refusal names the team's upstream review list.
	UpstreamReview string `yaml:"upstreamReview"`
}

// Overlaps tunes which sessions count as working on the same thing.
type Overlaps struct {
	// Ignore lists repositories whose issues and name form no overlap:
	// issue trackers and notebooks every session writes to.
	Ignore []string `yaml:"ignore"`
	// ActiveWithin leaves out sessions idle for longer.
	ActiveWithin Duration `yaml:"activeWithin"`
}

// Ollama is the host's ollama server, whose loaded models live in the
// iGPU's GTT.
type Ollama struct {
	// URL is its API (http://localhost:11434); unset, the machine runs none
	// and nothing watches or guards one.
	URL string `yaml:"url"`
	// Unit is its systemd unit, whose journal names the client that
	// loaded a model.
	Unit string `yaml:"unit"`
	// BudgetGiB is what a model-server claim may load without --gib;
	// MaxBudgetGiB the most a claim may ask for.
	BudgetGiB    int `yaml:"budgetGiB"`
	MaxBudgetGiB int `yaml:"maxBudgetGiB"`
	// NameOnly keeps the watch from unloading a model no lease covers: it
	// only names it.
	NameOnly bool `yaml:"nameOnly"`
	// LabTests are the agentlab subcommands that run turns on the host's
	// models; the hook refuses them outside the model-server lease.
	LabTests []string `yaml:"labTests"`
}

// Lemonade is the host's Lemonade Server, whose loaded models live in the
// iGPU's memory like ollama's.
type Lemonade struct {
	// URL is its API (http://localhost:13305); unset, the machine runs
	// none and nothing watches or guards one.
	URL string `yaml:"url"`
}

// Watch holds the thresholds of `beekeeper watch` (MiB unless noted). A
// memory or disk threshold left unset is a fraction of what the machine has
// (the Default* fractions): its absolute value is read with the method of
// the same name.
type Watch struct {
	// ThroughputAfter bounds how long an eligible PR may wait for a merge (default 2h).
	ThroughputAfter Duration `yaml:"throughputAfter"`
	// BatchInterval paces routine stdout notices. Negative disables batching;
	// --once always prints immediately. Action lines never wait for a batch.
	BatchInterval Duration `yaml:"batchInterval"`
	Interval      Duration `yaml:"interval"`
	StuckAfter    Duration `yaml:"stuckAfter"`
	// Repeat paces how often a lasting condition is handed to the
	// notifier (which sends it again after notify.repeat); the watch
	// prints it once when it starts and once when it ends.
	Repeat      Duration `yaml:"repeat"`
	BudgetEvery Duration `yaml:"budgetEvery"`
	AvailMinMiB int      `yaml:"availMinMiB"`
	SwapMaxMiB  int      `yaml:"swapMaxMiB"`
	// SwapWindow is how far back swap's growth rate looks (10m, at least
	// 5m): a burst older than the window has left the rate, which the SWAP
	// and OOMD IMMINENT checks and capacity's swap guard read.
	SwapWindow Duration `yaml:"swapWindow"`
	// OOMDHeadroomMinMiB and OOMDWithin decide when systemd-oomd's swap
	// kill is imminent (OOMD IMMINENT), only while oomd watches a cgroup
	// for swap and disk swap grows as MemAvailable falls: less swap growth
	// left before its SwapUsedLimit than OOMDHeadroomMinMiB, or the trigger
	// reached within OOMDWithin at the window's growth rate.
	OOMDHeadroomMinMiB int      `yaml:"oomdHeadroomMinMiB"`
	OOMDWithin         Duration `yaml:"oomdWithin"`
	ScopeAnonMaxMiB    int      `yaml:"scopeAnonMaxMiB"`
	// GTTMaxMiB is the iGPU GTT (system RAM the GPU driver pins, which no
	// cgroup counts) above which the watch prints IGPU GTT and names it
	// with the host ollama's models as the cause of LOW RAM and OOMD
	// IMMINENT.
	GTTMaxMiB int `yaml:"gttMaxMiB"`
	// LoadMax is the 1-minute load average over which the watch says HIGH
	// LOAD; unset (0), it is LoadPerCoreMax × the machine's cores
	// (LoadLimit).
	LoadMax        float64 `yaml:"loadMax"`
	LoadPerCoreMax float64 `yaml:"loadPerCoreMax"`
	PSIMax         float64 `yaml:"psiMax"`
	// CPUPSIMax is the CPU pressure ("some avg10" of /proc/pressure/cpu,
	// in percent) over which the watch says CPU PRESSURE once two samples
	// in a row read it, and over which, like over LoadLimit, the machine is
	// strained: the installation reads (upgrades, alerts, lane settling)
	// then run four times less often and at nice 10 (READS SLOWED).
	CPUPSIMax float64 `yaml:"cpuPSIMax"`
	// ForkRateMax is how far the fork rate (processes started per second,
	// from /proc/stat) must exceed the machine's usual one for the watch to
	// say PROCESS STORM, once two samples in a row read it; StackMax the number of copies of one command line
	// (arguments included), each running over a minute, over which it says
	// STACKED. A negative value turns the line off.
	ForkRateMax float64 `yaml:"forkRateMax"`
	StackMax    int     `yaml:"stackMax"`
	// ToolProcsMax is how many processes of the CLIs named in Tools may run
	// machine-wide before the watch says LOAD with the commands and the
	// sessions that run them: thirty sessions' kubectl, helm and tsh load
	// the CPU while no memory line fires. A negative value turns it off.
	ToolProcsMax int      `yaml:"toolProcsMax"`
	Tools        []string `yaml:"tools"`
	TmpMaxMiB    int      `yaml:"tmpMaxMiB"`
	DiskMinMiB   int      `yaml:"diskMinMiB"`
	// DiskCriticalMiB is the free space on / under which the watch says
	// DISK NEARLY FULL; DiskFillWithin how soon / would run full at the
	// rate its free space fell over the last minutes for the watch to say
	// DISK FILLING with the commands and sessions that wrote.
	DiskCriticalMiB int      `yaml:"diskCriticalMiB"`
	DiskFillWithin  Duration `yaml:"diskFillWithin"`
	// QuietSessions are globs (* matches any run) of the names of
	// short-lived sessions whose start, end and restart are no wake-up:
	// the watch logs them (watch.quiet) instead of printing them. Setting
	// it replaces the default, beekeeper's own tests ("test: *").
	QuietSessions []string `yaml:"quietSessions"`
	// Notify says whether the standby watch beekeeper install writes sends
	// the events that need a person to the desktop (watch --notify
	// --standby); off, the default, it runs as watch --standby.
	Notify bool `yaml:"notify"`
}

// The fractions a memory or disk threshold defaults to: of RAM (available
// memory, the desktop scope, the iGPU GTT), of swap (its use and oomd's
// headroom) and of the filesystem (/tmp's use, /'s free space).
const (
	DefaultAvailMin        = 0.12
	DefaultSwapMax         = 0.6
	DefaultOOMDHeadroomMin = 0.06
	DefaultScopeAnonMax    = 0.32
	DefaultGTTMax          = 0.28
	DefaultTmpMax          = 0.45
	DefaultDiskMin         = 0.05
	DefaultDiskCritical    = 0.01
)

// MinSwapSpan is the shortest span a swap growth rate is measured over, and
// the least Watch.SwapWindow may be: a shorter window would never rate one.
const MinSwapSpan = 5 * time.Minute

// AvailMin is the LOW RAM threshold on a machine of ramMiB.
func (w Watch) AvailMin(ramMiB int) int { return atLeast(w.AvailMinMiB, DefaultAvailMin, ramMiB) }

// SwapMax is the SWAP threshold on a machine of swapMiB.
func (w Watch) SwapMax(swapMiB int) int { return atMost(w.SwapMaxMiB, DefaultSwapMax, swapMiB) }

// OOMDHeadroomMin is the OOMD IMMINENT headroom on a machine of swapMiB.
func (w Watch) OOMDHeadroomMin(swapMiB int) int {
	return atLeast(w.OOMDHeadroomMinMiB, DefaultOOMDHeadroomMin, swapMiB)
}

// ScopeAnonMax is the DESKTOP SCOPE threshold on a machine of ramMiB.
func (w Watch) ScopeAnonMax(ramMiB int) int {
	return atMost(w.ScopeAnonMaxMiB, DefaultScopeAnonMax, ramMiB)
}

// GTTMax is the IGPU GTT threshold on a machine of ramMiB.
func (w Watch) GTTMax(ramMiB int) int { return atMost(w.GTTMaxMiB, DefaultGTTMax, ramMiB) }

// TmpMax is the TMPFS threshold on a /tmp of tmpMiB.
func (w Watch) TmpMax(tmpMiB int) int { return atMost(w.TmpMaxMiB, DefaultTmpMax, tmpMiB) }

// DiskMin is the LOW DISK threshold. Explicit positive values retain at least
// a 2 GiB reserve (or 5% on smaller disks). Negative disables it.
func (w Watch) DiskMin(diskMiB int) int {
	if w.DiskMinMiB < 0 {
		return 0
	}
	return max(min(2048, int(DefaultDiskMin*float64(diskMiB))), atLeast(w.DiskMinMiB, DefaultDiskMin, diskMiB))
}

// DiskCritical is the DISK NEARLY FULL threshold on a / of diskMiB.
func (w Watch) DiskCritical(diskMiB int) int {
	return atLeast(w.DiskCriticalMiB, DefaultDiskCritical, diskMiB)
}

// atLeast is a lower threshold: the configured one, else the fraction of
// total (0 when total is unknown, which never fires).
func atLeast(set int, fraction float64, total int) int {
	if set > 0 {
		return set
	}
	return int(fraction * float64(total))
}

// atMost is an upper threshold: the configured one, else the fraction of
// total (unbounded when total is unknown, which never fires).
func atMost(set int, fraction float64, total int) int {
	if set > 0 {
		return set
	}
	if total <= 0 {
		return math.MaxInt
	}
	return int(fraction * float64(total))
}

// KindClusters is the most kind clusters a machine of ramMiB runs:
// maxKindClusters, else one per 40 GiB, at least one.
func (c *Config) KindClusters(ramMiB int) int {
	if c.MaxKindClusters > 0 {
		return c.MaxKindClusters
	}
	return max(1, ramMiB/(40<<10))
}

// DefaultMemcapMax is the fraction of RAM a capped command may use.
const DefaultMemcapMax = 0.14

// MemcapMax is a capped command's MemoryMax on a machine of ramMiB, a
// systemd size: memcap.max, else DefaultMemcapMax of the RAM.
func (c *Config) MemcapMax(ramMiB int) string {
	if c.Memcap.Max != "" || ramMiB <= 0 {
		return cmp.Or(c.Memcap.Max, "infinity")
	}
	return strconv.Itoa(int(DefaultMemcapMax*float64(ramMiB))) + "M"
}

// DefaultMemcapCPUQuota is the fraction of the cores every capped run
// together may use.
const DefaultMemcapCPUQuota = 0.5

// DefaultMemcapCPUWeight is memcap.slice's CPUWeight: half a desktop
// slice's 100, a third of the cores while the desktop wants them too.
const DefaultMemcapCPUWeight = 50

// MemcapCPUQuota is memcap.slice's CPUQuota on a machine of cores, a
// systemd quota (100% a core): memcap.cpuQuota, else DefaultMemcapCPUQuota
// of the cores; "" with the cores unknown: no quota.
func (c *Config) MemcapCPUQuota(cores int) string {
	if c.Memcap.CPUQuota != "" || cores <= 0 {
		return c.Memcap.CPUQuota
	}
	return strconv.Itoa(int(DefaultMemcapCPUQuota*float64(cores)*100)) + "%"
}

// LoadLimit is the HIGH LOAD threshold on a machine of cores: LoadMax when
// set, LoadPerCoreMax × cores otherwise.
func (w Watch) LoadLimit(cores int) float64 {
	if w.LoadMax > 0 {
		return w.LoadMax
	}
	return w.LoadPerCoreMax * float64(cores)
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
	// LongContextThreshold counts all input tokens, including cache reads
	// and writes. Requests above it multiply every price by LongContextMultiplier.
	LongContextThreshold  int64   `yaml:"longContextThreshold" json:"longContextThreshold,omitempty"`
	LongContextMultiplier float64 `yaml:"longContextMultiplier" json:"longContextMultiplier,omitempty"`
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
	"claude-sonnet-5-5": {Input: 2, Output: 10, CacheWrite5m: 2.5, CacheWrite1h: 4, CacheRead: 0.2, ContextWindow: 1_000_000},
	"claude-sonnet-5":   {Input: 2, Output: 10, CacheWrite5m: 2.5, CacheWrite1h: 4, CacheRead: 0.2, ContextWindow: 1_000_000},
	"claude-sonnet-4-6": {Input: 3, Output: 15, CacheWrite5m: 3.75, CacheWrite1h: 6, CacheRead: 0.3, ContextWindow: 1_000_000},
	"claude-haiku-5-5":  {Input: 0.1, Output: 0.5, CacheWrite5m: 0.125, CacheWrite1h: 0.2, CacheRead: 0.01, ContextWindow: 1_000_000, LongContextThreshold: 100_000, LongContextMultiplier: 5},
	"claude-haiku-4-5":  {Input: 1, Output: 5, CacheWrite5m: 1.25, CacheWrite1h: 2, CacheRead: 0.1, ContextWindow: 200_000},
}

// Model returns the entry of model: its id, its dated snapshot
// ("claude-haiku-4-5-20251001"), or its agy effort selector
// ("claude-opus-5-5-high"). Another model of the same family is not priced
// like it.
func (m Metrics) Model(model string) (Model, bool) {
	if p, ok := m.Models[model]; ok {
		return p, true
	}
	if i := len(model) - len("-20060102"); i > 0 && model[i] == '-' && isDigits(model[i+1:]) {
		p, ok := m.Models[model[:i]]
		return p, ok
	}
	for _, suf := range []string{"-low", "-medium", "-high"} {
		if base, ok := strings.CutSuffix(model, suf); ok {
			if p, ok := m.Models[base]; ok {
				return p, true
			}
		}
	}
	return Model{}, false
}

func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// RunConfig chooses where build commands execute.
type RunConfig struct {
	Remote remotebuild.Config `yaml:"remote"`
	// Tail sizes the tail of a failed run's output beekeeper keeps in its
	// record and prints once, so the failing test's name survives the
	// truncation: a systemd size ("256K", "1M"; default
	// DefaultRunTail, capped at one MiB), or "off" to keep nothing.
	Tail string `yaml:"tail"`
}

// DefaultRunTail is how much of a failed run's output beekeeper keeps
// by default: enough for a test suite's failure block and its panic.
const DefaultRunTail = "256K"

// runTailRe is a run.tail size: a number with an optional systemd suffix.
var runTailRe = regexp.MustCompile(`^(\d+)([KkMmGgTt]?)$`)

// Memcap locates the build slots of the memcap wrapper and caps the
// commands `beekeeper run` runs.
type Memcap struct {
	SlotDir string `yaml:"slotDir"`
	Slots   int    `yaml:"slots"`
	// Max is a command's MemoryMax, a systemd size ("12G"; default: a
	// fraction of RAM, DefaultMemcapMax).
	Max string `yaml:"max"`
	// CPUQuota is memcap.slice's CPUQuota, the cores every capped run
	// together may use, a systemd quota ("1200%" is twelve cores; default:
	// a fraction of the cores, DefaultMemcapCPUQuota). The slots share it
	// by equal weight.
	CPUQuota string `yaml:"cpuQuota"`
	// CPUWeight is memcap.slice's CPUWeight against the desktop's slices
	// (100 each): the runs' share of the cores while the desktop wants
	// them too (default DefaultMemcapCPUWeight).
	CPUWeight int `yaml:"cpuWeight"`
	// HoldBound is how long a run may hold a build slot before watch names
	// it and its holder (default 30m): a build longer than that is a
	// benchmark or a lab proof, which runs with --class and takes no slot.
	HoldBound Duration `yaml:"holdBound"`
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

// GitHubHosts are the hosts the GitHub token goes to.
var GitHubHosts = []string{"github.com", "api.github.com", "uploads.github.com"}

// homePath is p with a leading ~/ resolved against home.
// validateDiskCaches refuses a cache policy that could reach beyond a cache:
// a relative path or filesystem root, a bad glob, a rule-less custom entry.
func validateDiskCachesEach(caches []DiskCache, report func(error)) {
	names := map[string]bool{}
	for _, e := range caches {
		if e.Name == "" || names[e.Name] {
			report(fmt.Errorf("disk.caches: each entry needs a path or a unique name (%q)", e.Name))
		}
		names[e.Name] = true
		builtin := e.Builtin(e.Name)
		switch e.Mode {
		case "", DiskCacheEntries, DiskCacheFiles, DiskCacheOff:
		case DiskCacheCommand:
			if strings.TrimSpace(e.Command) == "" {
				report(fmt.Errorf("disk.caches %s: mode command needs a command", e.Name))
			}
		case DiskCacheModeGo, DiskCacheModeSc:
			if (e.Mode == DiskCacheModeGo) != (e.Name == DiskCacheGo) || (e.Mode == DiskCacheModeSc) != (e.Name == DiskCacheSccache) {
				report(fmt.Errorf("disk.caches %s: mode %s belongs to the built-in entry of that cache alone", e.Name, e.Mode))
			}
		default:
			report(fmt.Errorf("disk.caches %s: mode %q is none of entries, files, command, off", e.Name, e.Mode))
		}
		if e.Command != "" && e.Mode != DiskCacheCommand {
			report(fmt.Errorf("disk.caches %s: command needs mode command", e.Name))
		}
		if e.OlderThan.Duration < 0 || e.MaxGiB < 0 {
			report(fmt.Errorf("disk.caches %s: olderThan and maxGiB take no negative value", e.Name))
		}
		if e.Path == "" {
			if !builtin {
				report(fmt.Errorf("disk.caches %s: path is required", e.Name))
			}
			continue
		}
		if !filepath.IsAbs(e.Path) || filepath.Dir(e.Path) == e.Path {
			report(fmt.Errorf("disk.caches %s: path must be absolute and not a filesystem root", e.Name))
		}
		if _, err := filepath.Match(e.Path, ""); err != nil {
			report(fmt.Errorf("disk.caches %s: path: %w", e.Name, err))
		}
		if !builtin && e.Mode != DiskCacheOff && e.Mode != DiskCacheCommand && e.OlderThan.Duration == 0 && e.MaxGiB == 0 && !e.Regenerable {
			report(fmt.Errorf("disk.caches %s: set olderThan, maxGiB or regenerable", e.Name))
		}
	}
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
