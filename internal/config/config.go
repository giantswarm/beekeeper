// Package config loads beekeeper's machine configuration: where its state
// lives, which shared resources sessions lease, the GitHub budget floor, the
// thresholds of the machine watch and which installations' alerts it reads.
//
// The file is $XDG_CONFIG_HOME/beekeeper/config.yaml (or --config, or
// $BEEKEEPER_CONFIG). Every field is optional; a missing file is the
// defaults. The defaults are neutral: memory thresholds are fractions of the
// machine's RAM, swap or filesystem, and every organisation or desk choice
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
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/notify"
)

// Browser is the resource every machine has: the one Chrome the Claude in
// Chrome extension drives.
const Browser = "browser"

// ModelServer is the resource of the host's model server: a claim carries
// the GiB its models may hold.
const ModelServer = "model-server"

// Config is the parsed configuration with the defaults applied.
type Config struct {
	// StateDir holds state.json, events.jsonl and the last snapshot.
	StateDir string `yaml:"stateDir"`
	// LeaseDir holds one directory per held lease (mkdir is the lock).
	LeaseDir string `yaml:"leaseDir"`
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

	GitHub   GitHub   `yaml:"github"`
	Watch    Watch    `yaml:"watch"`
	Overlaps Overlaps `yaml:"overlaps"`
	Claude   Claude   `yaml:"claude"`
	Omp      Omp      `yaml:"omp"`
	Desktop  Desktop  `yaml:"desktop"`
	Memcap   Memcap   `yaml:"memcap"`
	Lanes    []Lane   `yaml:"lanes"`
	Merge    Merge    `yaml:"merge"`
	Alerts   Alerts   `yaml:"alerts"`
	Notify   Notify   `yaml:"notify"`
	Metrics  Metrics  `yaml:"metrics"`
	Ollama   Ollama   `yaml:"ollama"`
	// Lemonade is the host's Lemonade Server, a second model server under
	// the same model-server lease and ollama's budgets.
	Lemonade Lemonade `yaml:"lemonade"`
	// Supervisor is what `handover --prompt` tells a successor supervisor,
	// how long a relay to it stays open and at what context it is due.
	Supervisor Supervisor `yaml:"supervisor"`
	// Guide is the role that walks the person through the decisions
	// waiting on them, never in the supervisor's session.
	Guide Guide `yaml:"guide"`
	// Agents configures how a registered agent near its context limit is
	// handed over to a fresh session.
	Agents Agents `yaml:"agents"`
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
	// Board is the project board `beekeeper board` picks work from.
	Board Board `yaml:"board"`
	// Plans are the repositories whose pull requests a note for a person
	// names only once their stage check is green.
	Plans Plans `yaml:"plans"`
}

// Plans configures the plans repositories: a note for the guide's person
// that links one of their open pull requests is refused while the pull
// request's stage check is red, pending or missing.
type Plans struct {
	// Repositories are owner/repo; empty, no note is checked.
	Repositories []string `yaml:"repositories"`
	// Check is the name of the stage check (default plan-stages).
	Check string `yaml:"check"`
}

// DefaultPlansCheck is the stage check's name unless plans.check says
// otherwise.
const DefaultPlansCheck = "plan-stages"

// Covers reports whether repo (owner/repo, any case) is a plans repository.
func (p Plans) Covers(repo string) bool {
	return slices.ContainsFunc(p.Repositories, func(r string) bool { return strings.EqualFold(r, repo) })
}

// Board is a GitHub project board and the order its work is picked in.
type Board struct {
	// Owner is the organisation and Project the number of its board;
	// unset, the board commands refuse.
	Owner   string `yaml:"owner"`
	Project int    `yaml:"project"`
	// Team is the value of the board's Team field whose items are the
	// desk's; empty, every item is.
	Team string `yaml:"team"`
	// People are the logins whose assigned items are free; an item
	// assigned to anybody else is skipped.
	People []string `yaml:"people"`
	// StaleAfter skips an item without activity for longer (default a
	// year).
	StaleAfter Duration `yaml:"staleAfter"`
	// Order is the picking order: the first free item of the first step
	// that offers one is picked.
	Order []BoardStep `yaml:"order"`
}

// BoardStep is one step of the picking order: the open board items whose
// status, kind and labels match (each list matches any of its values, an
// empty one everything), or the open issues of a GitHub search.
type BoardStep struct {
	// Name says why an item of the step is picked.
	Name string `yaml:"name"`
	// Status and Kind are the board's Status and Kind values; an
	// unambiguous part of one ("up next") stands for it.
	Status []string `yaml:"status"`
	Kind   []string `yaml:"kind"`
	Labels []string `yaml:"labels"`
	// SubIssues offers the open sub-issues of a matching item that has
	// any (an epic's remainder) instead of the item, in their order.
	SubIssues bool `yaml:"subIssues"`
	// Unblocked offers only items with recorded blockers, all of them
	// closed: a blocked item whose blocker cleared.
	Unblocked bool `yaml:"unblocked"`
	// CreatedWithin offers only items created within the duration.
	CreatedWithin Duration `yaml:"createdWithin"`
	// Search is a GitHub issue search whose open issues the step offers
	// (oldest first) instead of board items.
	Search string `yaml:"search"`
}

// Hooks configures where beekeeper's Claude Code hooks act.
type Hooks struct {
	// Scope is the desk's part of the machine; out of it the hooks are
	// inert.
	Scope HookScope `yaml:"scope"`
}

// HookScope is where the hooks act: in a session beekeeper started or
// knows (an agents start, a role holder, an agent on the roster), and in a
// session whose working directory or project lies under one of Dirs.
type HookScope struct {
	// Dirs are the desk's directories (~/ allowed); empty, every session
	// is in scope.
	Dirs []string `yaml:"dirs"`
}

// Outbound configures the outbound secret guard. The token patterns are
// built in; these add what is specific to the machine.
type Outbound struct {
	// Phrases never leave the machine: outbound text that contains one
	// (case-insensitive) is refused, naming its number, never the phrase.
	Phrases []string `yaml:"phrases"`
	// Paths are globs (~/ allowed) of files whose Write and Edit count as
	// outbound (plans, posts); a directory covers every file under it.
	Paths []string `yaml:"paths"`
	// StoreDeny are the secret-store writes the hook refuses.
	StoreDeny []StoreRule `yaml:"storeDeny"`
	// SweepRoots are the directories (~/ allowed; default: home) the watch
	// sweeps, SweepDepth levels deep (default 5), for world-readable key
	// files and git remote URLs that carry a credential.
	SweepRoots []string `yaml:"sweepRoots"`
	SweepDepth int      `yaml:"sweepDepth"`
}

// Secret configures beekeeper secret, the credential operations beekeeper
// runs so that no agent reads a value.
type Secret struct {
	// Vault is the team's shared 1Password vault, the only one an op://
	// reference may name; unset, beekeeper secret reads SOPS files only.
	Vault string `yaml:"vault"`
	// TokenFile (~/ allowed) holds the vault's service account token,
	// which beekeeper gives only to its own op calls. Agents reach it
	// nowhere: it lies outside every agent container's mounts.
	TokenFile string `yaml:"tokenFile"`
}

// Scan configures the transcript value scanner: beekeeper scan index
// fingerprints the values of these sources into the index under the state
// directory (scan/), which the PostToolUse hook and beekeeper scan sweep
// match against.
type Scan struct {
	// SOPS are globs (~/ allowed) of SOPS files whose values are indexed,
	// decrypted with the sops binary in beekeeper's own process.
	SOPS []string `yaml:"sops"`
	// Vaults are 1Password vaults whose concealed fields are indexed, read
	// with the op binary.
	Vaults []string `yaml:"vaults"`
	// MinLength is the shortest value indexed (default 12): shorter ones
	// would match ordinary output.
	MinLength int `yaml:"minLength"`
}

// StoreRule refuses a secret-store write (op item or document create and
// edit, vault kv put and patch) whose vault and item match its globs
// (case-insensitive); an empty glob matches any, as does a write that names
// no vault.
type StoreRule struct {
	Vault string `yaml:"vault"`
	Item  string `yaml:"item"`
}

// Kube configures the kube guard and how an installation's name becomes its
// kube context.
type Kube struct {
	// Production is the installation whose clusters agents never write to:
	// a context or cluster name with it as a component. Empty, the kube
	// guard is off.
	Production string `yaml:"production"`
	// ContextTemplate is the kube context of an installation, with
	// {installation} for its name ("login.example.com-{installation}");
	// empty, an installation's context is its name or the one ending in
	// @<name>.
	ContextTemplate string `yaml:"contextTemplate"`
}

// Teleport configures the Teleport login's keeper: a periodic user unit
// that renews the SSO login before it expires, under the browser lease.
type Teleport struct {
	// Proxy is tsh login's --proxy (login.example.com:443); empty, no
	// keeper runs and the watch reads no login.
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

// Enabled reports whether a keeper is configured.
func (t Teleport) Enabled() bool { return t.Proxy != "" }

func (t *Teleport) defaults(home string) {
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

// Reporter configures the scheduled status reporter: once per Every the
// standby watch starts one headless session with Brief, which posts one
// report to Person and ends; beekeeper then takes it off the roster.
type Reporter struct {
	// Every is the interval; the reports start on its multiples (on the
	// hour for 1h). Unset, no reporter runs.
	Every Duration `yaml:"every"`
	// Brief is the reporter's brief file (~/ allowed): what to report and
	// where to post it.
	Brief string `yaml:"brief"`
	// Model is the session's model (default: Claude Code's).
	Model string `yaml:"model"`
	// Person is who the report is for (default: guide.person).
	Person string `yaml:"person"`
	// Dir is the session's working directory (~/ allowed; default: home).
	Dir string `yaml:"dir"`
	// Timeout is how long a reporter may run without posting before it is
	// stopped (20m).
	Timeout Duration `yaml:"timeout"`
	// TZ is the report's time zone, an IANA name (default: the machine's).
	TZ string `yaml:"tz"`
	// Reviews are the repositories (owner/repo) whose open draft pull
	// requests wait for the person's review.
	Reviews []string `yaml:"reviews"`
}

// Enabled reports whether a reporter is scheduled.
func (r Reporter) Enabled() bool { return r.Every.Duration > 0 && r.Brief != "" }

// Agents configures the hand-over of registered agents.
type Agents struct {
	// RelayAt is an agent's session context, in tokens, at which the watch
	// says its hand-over due (default: supervisor.relayAt).
	RelayAt Tokens `yaml:"relayAt"`
	// NoteWait bounds how long `agents handover` waits for the agent's
	// note on what is in flight (3m).
	NoteWait Duration `yaml:"noteWait"`
	// StaleAfter is how long an idle agent whose CLI no longer runs stays on
	// the roster before the doctor takes it off (24h).
	StaleAfter Duration `yaml:"staleAfter"`
	// Shell is the prelude of every agent shell (beekeeper hook
	// sessionstart).
	Shell AgentShell `yaml:"shell"`
}

// AgentShell configures the prelude Claude Code runs before each Bash
// command of a session: the person's interactive shell setup (aliases, the
// harness's own tool shadows, zsh's nomatch) breaks the commands agents
// write for the plain tools.
type AgentShell struct {
	// Unalias are the commands whose alias or shell function the prelude
	// removes, so the name runs the tool on PATH (default: grep, find, ls,
	// cp, mv, rm; an empty list removes none).
	Unalias []string `yaml:"unalias"`
	// Globs is what an unmatched glob does: GlobsLiteral (the default)
	// passes it on as written, GlobsShell leaves the shell's own behaviour
	// (zsh: "no matches found", the command does not run).
	Globs string `yaml:"globs"`
}

// The values of agents.shell.globs.
const (
	GlobsLiteral = "literal"
	GlobsShell   = "shell"
)

// DefaultUnalias are the commands an agent shell runs unshadowed by default.
var DefaultUnalias = []string{"grep", "find", "ls", "cp", "mv", "rm"}

// commandName is what agents.shell.unalias takes: a name, nothing a shell
// would read as more.
var commandName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+-]*$`)

// Doctor configures the known faults the doctor probes and remedies.
type Doctor struct {
	Faults []Fault `yaml:"faults"`
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

// Guide configures the guide: its role and the person it guides.
type Guide struct {
	Role `yaml:",inline"`
	// Person is the name the guide's notes are filed for (note add --for,
	// or an older note's "[for <person>]" prefix), matched without regard
	// to case. Its queue and feed show only that person's notes; empty,
	// they show every note filed --for anyone.
	Person string `yaml:"person"`
	// WaitingTTL is how long a stopped session that waits on the person
	// stays in the queue and the feed; older ones fold into one line.
	WaitingTTL Duration `yaml:"waitingTTL"`
}

// Supervisor configures the supervisor: its role and the scope it
// supervises.
type Supervisor struct {
	Role `yaml:",inline"`
	// Scope says what the supervisor watches, in a sentence or two; empty,
	// the prompt names the resources, lanes and installations configured.
	Scope string `yaml:"scope"`
}

// Role configures a relayed role, the supervisor's or the guide's: the
// successor's session prompt (the instructions it follows, a skill or a
// file, not both) and the relay.
type Role struct {
	// Skill is the name of the skill the successor runs (supervise, guide).
	Skill string `yaml:"skill"`
	// Instructions is a file (~/ allowed) whose content opens the prompt
	// instead.
	Instructions string `yaml:"instructions"`
	// RelayAt is the role's session context, in tokens, at which the watch
	// reports the relay due ("400k"; the guide's default is 150k, the
	// supervisor's 400k).
	RelayAt Tokens `yaml:"relayAt"`
	// RelayTTL is how long a relay stays open for the successor's start.
	RelayTTL Duration `yaml:"relayTTL"`
	// RestartGrace is how long beekeeper waits after it first saw the
	// role's CLI gone: a CLI back under the same session within it is a
	// restart and keeps the role; past it, the watch says the supervisor
	// is gone and notifies. The grant rule holds throughout.
	RestartGrace Duration `yaml:"restartGrace"`
	// Dir is the folder a successor starts in (~/ allowed; default: the
	// folder its predecessor's desktop session started from, not the
	// worktree the desktop made for it, whose branch the desktop cannot
	// check out a second time).
	Dir string `yaml:"dir"`
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
	// DevctlOwners are the owners whose repositories devctl pr merge serves
	// (its GitHub App login reaches its own organisation only); a
	// repository of any other owner, and every repository while it is
	// empty, takes the plain squash merge as the gh login instead.
	DevctlOwners []string `yaml:"devctlOwners"`
}

// DevctlServes says whether devctl pr merge serves repo's owner.
func (m Merge) DevctlServes(repo string) bool {
	owner, _, _ := strings.Cut(repo, "/")
	return slices.ContainsFunc(m.DevctlOwners, func(o string) bool { return strings.EqualFold(o, owner) })
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

// Alerts configures the reading of the installations' Alertmanagers: watch
// reads them every Every and prints NEW and RESOLVED lines, snapshot adds
// the current set as a section.
type Alerts struct {
	// Installations are read always; a held lease whose name resolves to a
	// kube context is read too.
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
type GitHub struct {
	// Floor is the remaining core budget under which GitHub work stops.
	Floor int `yaml:"floor"`
	// ProbeRepo is the repository whose conditional GET reads the budget
	// headers; any repository the token can read (default: beekeeper's own).
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
	Interval Duration `yaml:"interval"`
	// Repeat paces how often a lasting condition is handed to the
	// notifier (which sends it again after notify.repeat); the watch
	// prints it once when it starts and once when it ends.
	Repeat      Duration `yaml:"repeat"`
	BudgetEvery Duration `yaml:"budgetEvery"`
	AvailMinMiB int      `yaml:"availMinMiB"`
	SwapMaxMiB  int      `yaml:"swapMaxMiB"`
	// OOMDHeadroomMinMiB and OOMDWithin decide when systemd-oomd's swap
	// kill is imminent (OOMD IMMINENT): less swap growth left before its
	// SwapUsedLimit than OOMDHeadroomMinMiB, or the trigger reached within
	// OOMDWithin at the last hour's growth rate.
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
	// QuietSessions are globs (* matches any run) of the names of
	// short-lived sessions whose start, end and restart are no wake-up:
	// the watch logs them (watch.quiet) instead of printing them. Setting
	// it replaces the default, beekeeper's own tests ("test: *").
	QuietSessions []string `yaml:"quietSessions"`
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
)

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

// DiskMin is the LOW DISK threshold on a / of diskMiB.
func (w Watch) DiskMin(diskMiB int) int { return atLeast(w.DiskMinMiB, DefaultDiskMin, diskMiB) }

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

// DefaultDesktopApp is claude.desktopApp's default, the desktop app's
// executable on Linux.
const DefaultDesktopApp = "claude-desktop"

// Claude locates what Claude Code and the desktop app keep on disk.
type Claude struct {
	ProjectsDir string `yaml:"projectsDir"`
	// DesktopApp is the desktop app's executable, which starts the app or
	// hands a claude:// link to the running one.
	DesktopApp string `yaml:"desktopApp"`
	// SessionsDir holds the record every running CLI keeps of the session
	// it runs (<pid>.json), what `claude agents` lists.
	SessionsDir string `yaml:"sessionsDir"`
	DesktopDir  string `yaml:"desktopDir"`
	// DesktopLog is the desktop app's main log: its latest focus change
	// says which session the main window shows.
	DesktopLog string `yaml:"desktopLog"`
}

// Omp locates what omp (oh-my-pi), the second local harness, keeps on disk.
type Omp struct {
	// SessionsDir holds omp's session files, one folder per working
	// directory.
	SessionsDir string `yaml:"sessionsDir"`
	// Model is the model `agents start --harness omp` starts an agent on
	// without --model, an exact selector omp lists ("ollama/qwen3.5:9b");
	// empty: such a start is refused.
	Model string `yaml:"model"`
}

// Desktop is how beekeeper shares the person's desktop.
type Desktop struct {
	// TypingQuiet is how long the person's keyboards and pointers stay
	// idle before a claude:// link switches the desktop's window (the
	// import of agents start, a reopen): a stray keystroke goes into the
	// window the link opens. A negative value opens links without
	// watching the input.
	TypingQuiet Duration `yaml:"typingQuiet"`
}

// Memcap locates the build slots of the memcap wrapper and caps the
// commands `beekeeper run` runs.
type Memcap struct {
	SlotDir string `yaml:"slotDir"`
	Slots   int    `yaml:"slots"`
	// Max is a command's MemoryMax, a systemd size ("12G"; default: a
	// fraction of RAM, DefaultMemcapMax).
	Max string `yaml:"max"`
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
	// The desktop app never restarts a crashed CLI by itself: the grace only
	// debounces a supervisor someone woke.
	setDur(&c.Supervisor.RestartGrace, 30*time.Second)
	c.Supervisor.defaults(home)
	// The guide's context is the person's conversation: it relays early.
	if c.Guide.RelayAt == 0 {
		c.Guide.RelayAt = 150_000
	}
	c.Guide.defaults(home)
	setDur(&c.Guide.WaitingTTL, 2*time.Hour)
	if c.Agents.RelayAt == 0 {
		c.Agents.RelayAt = c.Supervisor.RelayAt
	}
	setDur(&c.Agents.NoteWait, 3*time.Minute)
	setDur(&c.Agents.StaleAfter, 24*time.Hour)
	if c.Agents.Shell.Unalias == nil {
		c.Agents.Shell.Unalias = DefaultUnalias
	}
	setStr(&c.Agents.Shell.Globs, GlobsLiteral)
	c.Reporter.defaults(home, c.Guide.Person)
	c.Outbound.defaults(home)
	for i, d := range c.Hooks.Scope.Dirs {
		c.Hooks.Scope.Dirs[i] = filepath.Clean(homePath(home, d))
	}
	setInt(&c.Scan.MinLength, 12)
	c.Secret.TokenFile = homePath(home, c.Secret.TokenFile)
	for i := range c.Scan.SOPS {
		c.Scan.SOPS[i] = homePath(home, c.Scan.SOPS[i])
	}
	setStr(&c.Plans.Check, DefaultPlansCheck)
	c.Teleport.defaults(home)
	setStr(&c.Shell, os.Getenv("SHELL"))
	setStr(&c.Shell, "sh")

	setDur(&c.Overlaps.ActiveWithin, time.Hour)
	setDur(&c.Board.StaleAfter, 365*24*time.Hour)

	setInt(&c.GitHub.Floor, 2500)
	setStr(&c.GitHub.ProbeRepo, "giantswarm/beekeeper")

	w := &c.Watch
	setDur(&w.Interval, 30*time.Second)
	setDur(&w.Repeat, 10*time.Minute)
	setDur(&w.BudgetEvery, 5*time.Minute)
	setDur(&w.OOMDWithin, 30*time.Minute)
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
	if c.Notify.Kinds == nil {
		c.Notify.Kinds = slices.Clone(notify.Kinds)
	}
	setDur(&c.Notify.Repeat, 30*time.Minute)
	return nil
}

func (r *Role) defaults(home string) {
	setDur(&r.RelayTTL, 15*time.Minute)
	setDur(&r.RestartGrace, time.Minute)
	if r.RelayAt == 0 {
		r.RelayAt = 400_000
	}
	r.Instructions = homePath(home, r.Instructions)
	r.Dir = homePath(home, r.Dir)
}

// homePath is p with a leading ~/ resolved against home.
func homePath(home, p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return filepath.Join(home, rest)
	}
	return p
}

func (o *Outbound) defaults(home string) {
	if len(o.SweepRoots) == 0 {
		o.SweepRoots = []string{home}
	}
	setInt(&o.SweepDepth, 5)
	for _, ps := range [][]string{o.Paths, o.SweepRoots} {
		for i := range ps {
			ps[i] = homePath(home, ps[i])
		}
	}
}

func (r *Reporter) defaults(home, person string) {
	setDur(&r.Timeout, 20*time.Minute)
	setStr(&r.Person, person)
	setStr(&r.Dir, home)
	r.Brief, r.Dir = homePath(home, r.Brief), homePath(home, r.Dir)
}

func (c *Config) validate() error {
	if g := c.Agents.Shell.Globs; g != "" && g != GlobsLiteral && g != GlobsShell {
		return fmt.Errorf("agents.shell.globs: %q: want %s or %s", g, GlobsLiteral, GlobsShell)
	}
	for _, n := range c.Agents.Shell.Unalias {
		if !commandName.MatchString(n) {
			return fmt.Errorf("agents.shell.unalias: %q is no command name", n)
		}
	}
	for i, r := range c.Outbound.StoreDeny {
		for _, g := range []string{r.Vault, r.Item} {
			if _, err := filepath.Match(g, ""); err != nil {
				return fmt.Errorf("outbound.storeDeny[%d]: %q: %w", i, g, err)
			}
		}
	}
	for _, g := range c.Outbound.Paths {
		if _, err := filepath.Match(g, ""); err != nil {
			return fmt.Errorf("outbound.paths: %q: %w", g, err)
		}
	}
	if r := c.Reporter; r.Every.Duration != 0 && (r.Every.Duration < time.Minute || r.Brief == "") {
		return fmt.Errorf("reporter: every %s needs at least a minute and a brief", r.Every.Duration)
	}
	if tz := c.Reporter.TZ; tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return fmt.Errorf("reporter.tz: %w", err)
		}
	}
	if err := ownerRepos("reporter.reviews", c.Reporter.Reviews); err != nil {
		return err
	}
	if err := ownerRepos("plans.repositories", c.Plans.Repositories); err != nil {
		return err
	}
	for i, st := range c.Board.Order {
		if st.Name == "" {
			return fmt.Errorf("board.order[%d]: no name", i)
		}
		if st.Search != "" && (len(st.Status)+len(st.Kind)+len(st.Labels) > 0 || st.SubIssues || st.Unblocked) {
			return fmt.Errorf("board.order[%d] %q: a search step matches no board fields", i, st.Name)
		}
	}
	if t := c.Teleport; t.WarnBefore.Duration > t.RenewBefore.Duration {
		return fmt.Errorf("teleport: warnBefore %s is past renewBefore %s: the watch would warn before the keeper renews", t.WarnBefore.Duration, t.RenewBefore.Duration)
	}
	if t := c.Kube.ContextTemplate; t != "" && !strings.Contains(t, "{installation}") {
		return fmt.Errorf("kube.contextTemplate: %q has no {installation}", t)
	}
	for name, r := range map[string]Role{"supervisor": c.Supervisor.Role, "guide": c.Guide.Role} {
		if r.Skill != "" && r.Instructions != "" {
			return fmt.Errorf("%s: set skill or instructions, not both", name)
		}
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
	if p := c.Alerts.PageSeverity; p != "" && !slices.Contains(alerts.Severities, p) {
		return fmt.Errorf("alerts.pageSeverity: %q is none of %s", p, strings.Join(alerts.Severities, ", "))
	}
	if g := c.Alerts.OwnerGrace.Duration; g != 0 && g < time.Minute {
		return fmt.Errorf("alerts.ownerGrace: %s; a page has at least a minute to be owned", g)
	}
	if err := c.Notify.validate(); err != nil {
		return err
	}
	for _, r := range c.Resources {
		if r == "" || r == Browser || r == ModelServer || filepath.Base(r) != r || r[0] == '.' {
			return fmt.Errorf("resources: %q is not a valid resource name", r)
		}
	}
	if err := c.validateLabs(); err != nil {
		return err
	}
	if o := c.Ollama; (o.URL != "" || c.Lemonade.URL != "") && (o.BudgetGiB < 1 || o.BudgetGiB > o.MaxBudgetGiB) {
		return fmt.Errorf("ollama.budgetGiB: %d is not between 1 and maxBudgetGiB %d", o.BudgetGiB, o.MaxBudgetGiB)
	}
	faults := map[string]bool{}
	for i, f := range c.Doctor.Faults {
		if f.Name == "" || f.Probe == "" || f.Remedy == "" || faults[f.Name] {
			return fmt.Errorf("doctor.faults[%d]: a fault needs a name of its own, a probe and a remedy", i)
		}
		faults[f.Name] = true
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

func (n Notify) validate() error {
	for _, k := range n.Kinds {
		if err := notifyKind("notify.kinds", k); err != nil {
			return err
		}
	}
	for k, u := range n.Urgency {
		if err := notifyKind("notify.urgency", k); err != nil {
			return err
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

// Leasable returns every resource a session can lease: the configured ones,
// the model server when one is watched, the browser last.
func (c *Config) Leasable() []string {
	out := slices.Clone(c.Resources)
	if c.Ollama.URL != "" || c.Lemonade.URL != "" {
		out = append(out, ModelServer)
	}
	return append(out, Browser)
}

// validateLabs checks that every lab lease is a configured resource and
// that no two lab leases map one kind cluster.
func (c *Config) validateLabs() error {
	by := map[string]string{}
	for _, res := range slices.Sorted(maps.Keys(c.Labs)) {
		cl := c.Labs[res]
		if !slices.Contains(c.Resources, res) {
			return fmt.Errorf("labs: %q is not one of the resources %s", res, strings.Join(c.Resources, ", "))
		}
		if cl == "" {
			return fmt.Errorf("labs: %q names no kind cluster", res)
		}
		if other, ok := by[cl]; ok {
			return fmt.Errorf("labs: %q and %q both map the kind cluster %q", other, res, cl)
		}
		by[cl] = res
	}
	return nil
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

// IsLeasable reports whether name is a configured resource or the browser.
func (c *Config) IsLeasable(name string) bool {
	return slices.Contains(c.Leasable(), name)
}

// ownerRepos refuses an entry of field that is not owner/repo.
func ownerRepos(field string, repos []string) error {
	for _, r := range repos {
		if o, name, ok := strings.Cut(r, "/"); !ok || o == "" || name == "" || strings.ContainsAny(name, "/ ") {
			return fmt.Errorf("%s: %q is not owner/repo", field, r)
		}
	}
	return nil
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
