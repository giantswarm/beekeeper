package config

import (
	"regexp"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/execution"
)

// ValidEffort reports whether a level is unset or a supported effort selector.
func ValidEffort(level string) bool {
	return slices.Contains([]string{"", "low", "medium", "high", "xhigh", "max"}, level)
}

// PolicyBundle lists explicitly approved files below Root. Each start reads
// them anew, so an edit reaches the next remote worker; no directory is harvested.
type PolicyBundle struct {
	Root         string                       `yaml:"root"`
	Sources      []execution.BundleSource     `yaml:"sources"`
	Settings     map[string]string            `yaml:"settings,omitempty"`
	Capabilities []execution.BundleCapability `yaml:"capabilities,omitempty"`
}

// Agents configures the hand-over of registered agents.
type Agents struct {
	// RemoteMCP exposes only explicitly named coordinator tools to SSH workers.
	RemoteMCP map[string]execution.MCPServer `yaml:"remoteMCP,omitempty"`
	// PolicyBundle names the coordinator's rules and skills every remote start
	// carries to its host. Unset, a remote worker has the host's policy alone.
	PolicyBundle *PolicyBundle `yaml:"policyBundle,omitempty"`
	// Hosts are explicit SSH execution hosts. No entry or --host local keeps
	// the existing local launch path. Execution configures this machine only
	// when it is itself an execution host; authentication is provisioned there.
	Hosts     map[string]ExecutionHost `yaml:"hosts,omitempty"`
	Execution *execution.HostConfig    `yaml:"execution,omitempty"`
	// RelayAt is an agent's session context, in tokens, at which the watch
	// says its hand-over due (default 400k).
	RelayAt Tokens `yaml:"relayAt"`
	// LastStepGrace is how long the watch holds the hand-over of an agent
	// over RelayAt whose task is in its last step (its sessions serve record
	// waits on the report or says the merge landed, or the gate saw its
	// merge land), counted from that evidence, since its report is expected
	// before a hand-over would pay (30m).
	LastStepGrace Duration `yaml:"lastStepGrace"`
	// LastStepCeiling is the context, in tokens, at which the hand-over is
	// due whatever the agent's last step (default: a quarter above RelayAt).
	LastStepCeiling Tokens `yaml:"lastStepCeiling"`
	// NoteWait bounds how long `agents handover` waits for the agent's
	// note on what is in flight (3m).
	NoteWait Duration `yaml:"noteWait"`
	// StaleAfter bounds inactivity without a running CLI (24h). An idle or
	// unkept task holder is retired after this age; gone task holders free
	// their capacity slots. Negative disables stale retirement.
	StaleAfter Duration `yaml:"staleAfter"`
	// AutoResume has the watch resume a parked agent (agents park) with
	// what settled its wait, once the note it parked on is closed or the
	// pull request merged or closed; off, the watch only says AGENT
	// RESUMABLE and agents resume does it by hand.
	AutoResume bool `yaml:"autoResume"`
	// Shell is the prelude of every agent shell (beekeeper hook
	// sessionstart).
	Shell AgentShell `yaml:"shell"`
	// Dir is the folder every agent beekeeper starts runs in (agents start,
	// agents handover, a role's successor): the desk's checkout, whose
	// project instructions every session loads. Empty: the caller's.
	Dir string `yaml:"dir"`
	// WorktreesDir is the place for worktrees created by agents start.
	// Empty uses <stateDir>/worktrees.
	WorktreesDir string `yaml:"worktreesDir"`
	// Roots are the folders under which an agent may run instead, with
	// --dir: the worktrees of the desk's repositories. Any other folder is
	// refused while Dir is set.
	Roots []string `yaml:"roots"`
	// Profile names the tool profile of the workers agents start creates
	// (Profiles, or the built-in minimal); empty: none, a worker gets the
	// person's whole tool surface and its task runs as a desktop turn.
	Profile string `yaml:"profile"`
	// Profiles are the desk's tool profiles by name; one named minimal
	// replaces the built-in one.
	Profiles map[string]Profile `yaml:"profiles"`
	// Mode is how beekeeper runs the sessions it starts (agents start,
	// agents handover, a role's successor) and wakes: ModeDesktop (the
	// default) imports them into Claude Desktop and wakes them through it;
	// ModeBackground starts each as a Claude Code background session
	// (claude --bg) with Remote Control and none of the caller's CLAUDE*
	// variables, with no desktop import and no claude:// link, and wakes it
	// by its messaging, a stopped one by claude --bg --resume.
	Mode string `yaml:"mode"`
	// Model is the model of a session beekeeper starts when the start names
	// none (--model, a successor's holder's); empty: Claude Code's default.
	Model string `yaml:"model"`
	// Models is ordered: the first matching nonempty model wins.
	Models []AgentModelRule `yaml:"models"`
	// RequireModel refuses omission rather than using a harness default.
	RequireModel bool `yaml:"requireModel"`
	// Kinds are the desk's task kinds by name regexp, each with its own caps
	// on starts; a name may match several (a filer kind and a meta kind
	// over all meta workers), and every matching kind's caps hold.
	Kinds []AgentKind `yaml:"kinds"`
}

// AgentKind is a task kind: the agents whose name Match matches, at most
// MaxBusy of them busy at once, a start at most every MinInterval, and the
// daily spend at which the watch says SPEND <kind>. A zero cap is none.
type AgentKind struct {
	Name        string   `yaml:"name"`
	Match       string   `yaml:"match"`
	MaxBusy     int      `yaml:"maxBusy"`
	MinInterval Duration `yaml:"minInterval"`
	// DailySpend is in USD for a day of the desk's time zone.
	DailySpend float64 `yaml:"dailySpend"`
}

// Matches reports whether name is one of k's agents; an invalid regexp,
// which validation refuses, matches none.
func (k AgentKind) Matches(name string) bool {
	re, err := regexp.Compile(k.Match)
	return err == nil && re.MatchString(name)
}

// KindsOf returns the kinds whose regexp matches name, in their order.
func (a Agents) KindsOf(name string) []AgentKind {
	var out []AgentKind
	for _, k := range a.Kinds {
		if k.Matches(name) {
			out = append(out, k)
		}
	}
	return out
}

// AgentModelRule selects a model for a named task before provider selection.
type AgentModelRule struct {
	// Harness limits effort to this harness; model selection stays independent.
	Harness string `yaml:"harness,omitempty"`
	Match   string `yaml:"match"`
	Kind    string `yaml:"kind"`
	Model   string `yaml:"model"`
	// Effort is the session's effort level (low, medium, high, xhigh, max);
	// empty leaves it to a later rule or the harness default.
	Effort string `yaml:"effort"`
}

// ModelFor returns the first matching nonempty model, then the desk-wide
// fallback. A rule may leave the model unset for independent settings.
func (a Agents) ModelFor(name, kind string) string {
	for _, r := range a.Models {
		if m := strings.TrimSpace(r.Model); m != "" && r.matches(name, kind) {
			return m
		}
	}
	return strings.TrimSpace(a.Model)
}

// EffortFor returns the first matching nonempty effort; empty keeps the
// harness default.
func (a Agents) EffortFor(name, kind string, harness ...string) string {
	for _, r := range a.Models {
		if r.Harness != "" && (len(harness) == 0 || r.Harness != harness[0]) {
			continue
		}
		if e := strings.TrimSpace(r.Effort); e != "" && r.matches(name, kind) {
			return e
		}
	}
	return ""
}

func (r AgentModelRule) matches(name, kind string) bool {
	if r.Kind != "" && r.Kind != kind {
		return false
	}
	re, err := regexp.Compile(r.Match)
	return err == nil && re.MatchString(name)
}

type ExecutionHost = execution.Host

// The values of agents.mode.
const (
	ModeDesktop    = "desktop"
	ModeBackground = "background"
)

// Profile is the tool surface of a session beekeeper starts: the headless
// turns it launches get it on their command line (--tools, and --settings,
// whose flag settings Claude Code ranks over the person's user and project
// settings for that process only), never through the person's settings
// files.
type Profile struct {
	// Tools are the built-in tools kept (--tools); every other one is off.
	// Empty: Claude Code's default set.
	Tools []string `yaml:"tools"`
	// Deny are further permission rules the session is refused
	// (permissions.deny); a bare tool or mcp__<server> name takes it off the
	// tool list.
	Deny []string `yaml:"deny"`
	// Plugins are the plugins kept, by name (beekeeper) or id
	// (beekeeper@beekeeper); every other installed plugin, of any
	// marketplace, is disabled (enabledPlugins), and its skills, agents,
	// MCP servers and hooks with it. Empty: every plugin stays.
	Plugins []string `yaml:"plugins"`
	// MCPServers are the MCP servers kept, by name (allowedMcpServers);
	// every other one, claude.ai connectors included, is off. Empty: every
	// server stays.
	MCPServers []string `yaml:"mcpServers"`
}

// profileNamePattern is a profile's name, which names its settings file too.
var profileNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// MinimalProfile is the name of the profile beekeeper ships.
const MinimalProfile = "minimal"

// minimalProfile keeps what a worker uses: the shell, files, skills, tool
// search, messages to other sessions and the browser connector; beekeeper's
// own plugin. Artifacts, the browser pane, questions to the person,
// workflows, subagents, the desktop-app and claude.ai connectors and every
// other plugin are off.
var minimalProfile = Profile{
	Tools: []string{"Bash", "Read", "Edit", "Write", "Skill", "ToolSearch", "SendMessage", "ListAgents",
		"TaskStop", "WebFetch", "WebSearch"},
	Plugins:    []string{"beekeeper"},
	MCPServers: []string{"claude-in-chrome"},
}

// ProfileFor resolves a profile name: the desk's profile of that name, else
// the built-in minimal. ok is false for an empty name (no profile) or one
// neither defines.
func (a Agents) ProfileFor(name string) (Profile, bool) {
	if name == "" {
		return Profile{}, false
	}
	if p, ok := a.Profiles[name]; ok {
		return p, true
	}
	if name == MinimalProfile {
		return minimalProfile, true
	}
	return Profile{}, false
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
	// Path are directories the prelude puts first on PATH, in their order
	// (a leading ~/ is the home directory): the agent's own programs, such
	// as a gh link to devctl that acts with the GitHub App's short-lived
	// token, in place of the person's (default: none).
	Path []string `yaml:"path"`
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
