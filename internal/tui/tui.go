// Package tui is beekeeper's screen for the person: one window over the
// live state of the machine beekeeper keeps — the sessions and what each
// is on, the machine's memory and build slots, the leases, holds and merge
// lanes, the supervisor and guide, the agents, notes and timers, the
// GitHub budget, the installations' alerts and the event log.
//
// Besides the person's messages to a session it reads only: it takes no
// lease and lifts no hold — those stay with the commands, whose exit codes
// the sessions gate on. The data comes through
// a Source, so the model is testable without a machine; cmd supplies one
// built from the same view code the commands print.
package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Source supplies the screen's data. Implementations must be safe to call
// repeatedly and never block longer than the caller's context allows;
// a failed refresh keeps the model showing the last good Data.
type Source interface {
	// Data reads everything the screen shows in one go.
	Data(ctx context.Context) (*Data, error)
	// Tail reads the last turns of one session's transcript: what it
	// said and what it was told. session is a name, id or PID as in
	// `beekeeper tail`.
	Tail(ctx context.Context, session string, turns int) ([]Turn, error)
	// Send delivers the person's message to one session and says where
	// it went; a session that takes no message is an error.
	Send(ctx context.Context, session, text string) (string, error)
	// TakeOver brings the approvals of the session with id to this
	// screen; Release hands them back to its own window.
	TakeOver(ctx context.Context, id string) error
	Release(ctx context.Context, id string) error
	// Answer allows or denies the held request of the session with id.
	Answer(ctx context.Context, id, request string, allow bool) error
}

// Options tunes the run.
type Options struct {
	// Interval is how often Data is refreshed (default 2s).
	Interval time.Duration
}

// Run opens the screen and blocks until the person quits.
func Run(src Source, opts Options) error {
	if opts.Interval <= 0 {
		opts.Interval = 2 * time.Second
	}
	p := tea.NewProgram(newModel(src, opts))
	final, err := p.Run()
	if m, ok := final.(*model); ok {
		m.releaseAll() // a closed screen answers nothing
	}
	return err
}

// Data is one refresh: everything the screen shows. All times are wall
// clock; the screen renders ages against its own clock.
type Data struct {
	At       time.Time
	Status   Status
	Machine  Machine
	Budget   Budget
	Sessions []Session
	Totals   Totals
	Overlaps []Overlap
	Leases   Leases
	Holds    []Hold
	Lanes    []Lane
	Roles    []Role
	Agents   []Agent
	Notes    []Note
	Timers   []Timer
	Records  []Record
	Alerts   []AlertsFor
	Upgrades []string
	Events   []Event
	// Errors are what went wrong this refresh that left the rest of the
	// data usable (a failed budget probe, an unreadable source).
	Errors []string
}

// Status is the one-line summary: who supervises and what is pending.
type Status struct {
	// Supervisor is the recorded supervisor's name, "" when none is.
	Supervisor string
	// SupervisorLive is whether its session runs.
	SupervisorLive bool
	// SupervisorGone says the supervisor is recorded but its session is
	// gone: claims wait for a successor.
	SupervisorGone bool
	// LeaseCount, HoldCount count what is held; Due counts the notes and
	// timers whose time has come.
	LeaseCount int
	HoldCount  int
	Due        int
}

// Machine is the guarded side: what the memory looks like and what runs
// under a cap.
type Machine struct {
	Load      [3]float64
	Mem       Mem
	PSIFull60 float64
	Scope     *Scope
	Tmp       Disk
	Root      Disk
	Slots     []Slot
	Clusters  []Cluster
	// ClustersErr is set when docker cannot be asked.
	ClustersErr string
	CLIMemMiB   int
	// Waits are the long-running commands sessions sit on.
	Waits []Wait
	OOM   []OOMKill
	Oomd  []string
}

type Mem struct {
	TotalMiB     int
	AvailableMiB int
	SwapTotalMiB int
	SwapUsedMiB  int
	ShmemMiB     int
}

// Scope is the Claude Desktop cgroup systemd-oomd kills whole.
type Scope struct {
	CurrentMiB int
	AnonMiB    int
	SwapMiB    int
	High       string
	Max        string
	SwapMax    string
	OOMKills   int64
	HighEvents int64
}

type Disk struct {
	Path    string
	UsedMiB int
	FreeMiB int
}

// Slot is one memcap build slot.
type Slot struct {
	N      int
	Free   bool
	Holder string
}

type Cluster struct {
	Name       string
	Nodes      int
	MemMiB     int
	RunningFor string
}

// Wait is a command a session sits on.
type Wait struct {
	PID     int
	Session string
	Args    string
	Elapsed time.Duration
}

// OOMKill is a kernel OOM kill with whose limit it hit.
type OOMKill struct {
	At         time.Time
	PID        int
	Task       string
	AnonMiB    int
	Constraint string
	Memcg      string
	Owner      string
}

// Budget is the GitHub REST budget every session draws from.
type Budget struct {
	// At is when the reading was taken; zero when there is none yet.
	At        time.Time
	Limit     int
	Remaining int
	Reset     time.Time
	Floor     int
	// Held says a "github" hold is set, HoldReason why.
	Held       bool
	HoldReason string
	// GraphQL says the GraphQL limit, GraphQLRefused that GitHub refuses
	// GraphQL calls.
	GraphQL        string
	GraphQLRefused bool
	Pollers        []Poller
	// Err is set when the probe failed; the figures above are then the
	// last reading from the state, if any.
	Err string
}

// Poller is a gh or devctl process drawing on the budget.
type Poller struct {
	PID     int
	Session string
	Args    string
	Elapsed time.Duration
}

// Session is one running agent session: Claude Code, or omp.
type Session struct {
	PID int
	// ID is the session id its permission requests carry.
	ID   string
	Name string
	Role string
	// Harness is "" for Claude Code, "omp" for an omp session.
	Harness string
	// State is an omp session's own: busy, idle or ended; "" for Claude
	// Code, whose state the screen reads off its activity (stateOf).
	State  string
	Cwd    string
	Repo   string
	Branch string
	Model  string
	// Permission is the permission mode.
	Permission string
	Started    time.Time
	LastActive time.Time
	MemMiB     int
	// Waiting is what the session waits on its person for, "" when it
	// waits on nobody.
	Waiting string
	// Work is what its latest turns are about: repositories and
	// owner/repo#n refs.
	Work   []string
	Serves string
	Waits  string
	// Leases names the resources it holds.
	Leases []string
	// Context is its context in tokens, ContextWindow the model's window,
	// ContextFill the share of it in use (0 when unknown).
	Context       int64
	ContextWindow int
	ContextFill   float64
	// LastHour and Total are its activity; Idle is the time since its
	// transcript last changed.
	LastHour Counter
	Total    Counter
	Idle     time.Duration
	// Commands are what it runs right now.
	Commands []Command
	// Scopes are the capped runs (beekeeper run) it holds now.
	Scopes []RunScope
	// GitHubProcesses are its gh and devctl processes now.
	GitHubProcesses int
	Merges          Merges
	// TakenOver says a screen holds the session's approvals; Approvals
	// are the requests held for it, oldest first.
	TakenOver bool
	Approvals []Approval
}

// Approval is a permission request a taken-over session waits on.
type Approval struct {
	ID string
	At time.Time
	// Gist is the call in one line ("Bash: Run the tests"), Detail its
	// input, a line per field.
	Gist   string
	Detail []string
}

type Counter struct {
	Busy        time.Duration
	Turns       int
	ToolCalls   int
	ToolErrors  int
	GitHubCalls int
	// Cost is nil when its model has no price.
	Cost *float64
}

type Command struct {
	PID     int
	Args    string
	Elapsed time.Duration
	// Remaining is set for a bounded sleep: when it ends.
	Remaining time.Duration
}

type RunScope struct {
	Unit    string
	Command string
	MemMiB  int
}

// Merges counts a session's gated merges: queued, merged, refused, failed.
type Merges struct {
	Queued  int
	Merged  int
	Refused int
	Failed  int
}

// Totals are the figures over all sessions.
type Totals struct {
	Total           Counter
	LastHour        Counter
	GitHubProcesses int
	Merges          Merges
	// Top are the sessions that spent the most in the last hour.
	Top []TopSession
}

type TopSession struct {
	Name        string
	LastHour    Counter
	ContextFill float64
}

// Overlap names what more than one session is on.
type Overlap struct {
	// Kind is "ref" or "repo".
	Kind     string
	Key      string
	Sessions []string
}

// Leases is the sharing view: what is held, what is free and the grant
// queues waiting on each held resource.
type Leases struct {
	Held   []Lease
	Free   []string
	Queues map[string][]Grant
}

// Lease is one held resource. State is "live", "gone" (its holder's
// session is gone: stale) or "person".
type Lease struct {
	Resource       string
	Holder         string
	Name           string
	Purpose        string
	State          string
	Since          time.Time
	UpgradeUnblock string
}

// Grant is the supervisor's word that a session may claim a resource.
type Grant struct {
	Resource string
	To       string
	By       string
	At       time.Time
}

// Hold stops work on a target until lifted or until Until passes.
type Hold struct {
	Target string
	Reason string
	By     string
	At     time.Time
	Until  time.Time
	Except string
	// Tool names the release window this hold is.
	Tool        string
	ToolFrom    string
	ToolRelease string
}

// Lane is one merge lane's queue.
type Lane struct {
	Name         string
	Installation string
	Running      *Merge
	Settling     []*Merge
	Waiting      []Merge
	// Hold is the lane's or the repository's hold reason, "" when none.
	Hold string
	// Stall names the waiting merge and the places it waits behind when
	// the lane has stalled; "" when it has not.
	Stall string
}

// Merge is one gated devctl pr merge.
type Merge struct {
	Key      string
	By       string
	Phase    string
	Joined   time.Time
	Started  time.Time
	Finished time.Time
	Exit     int
	Seeded   bool
	Outside  bool
	// Retrying is a failed attempt that keeps its place.
	Retrying bool
	Release  string
	Roll     []string
}

// Role is the supervisor's or the guide's record as the screen reads it.
type Role struct {
	// Name is "supervisor" or "guide".
	Name   string
	Holder string
	Live   bool
	Since  time.Time
	// Gone is when the CLI was first seen gone, zero while it runs;
	// RestartUntil is the end of the grace a restart has.
	Gone         time.Time
	RestartUntil time.Time
	// Context is the holder's context in tokens, RelayAt the point the
	// watch reports the relay due.
	Context int64
	RelayAt int64
	// Relay describes an open relay ("to X until HH:MM"), "" when none.
	Relay string
}

// Agent is a session registered as spare capacity. Reachable is "live",
// "waiting …" or "not running".
type Agent struct {
	Name       string
	Task       string
	AssignedAt time.Time
	IdleSince  time.Time
	Reachable  string
}

// Note is an open item: a decision waiting on a person, a deadline.
type Note struct {
	ID      int
	For     string
	Text    string
	Default string
	Due     time.Time
	By      string
}

// Timer is a point in time to look at something.
type Timer struct {
	ID    int
	Due   time.Time
	What  string
	By    string
	Fired bool
}

// Record says which session serves which issue.
type Record struct {
	Session string
	Issue   string
	Waits   string
}

// AlertsFor is one installation's baseline: its last set and whether it
// answered.
type AlertsFor struct {
	Installation string
	Reachable    bool
	Alerts       []Alert
}

// Alert is one firing alert.
type Alert struct {
	Severity  string
	Team      string
	Alertname string
	Cluster   string
	Where     string
	Since     string
}

// Event is one line of the event log.
type Event struct {
	At     time.Time
	Verb   string
	By     string
	Detail string
}

// Turn is one turn of a session's transcript.
type Turn struct {
	At   time.Time
	Role string
	Text string
}
