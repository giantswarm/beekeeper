// Package state is beekeeper's shared memory on the machine: the supervisor
// and its relay, grants, holds, registered agents, notes, timers and session records, one
// JSON document read and rewritten under an exclusive file lock, plus an
// append-only event log of every change and of every build run. It outlives any session: a restarted supervisor or its
// successor reads what the previous one knew instead of rebuilding it from
// prose.
package state

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// Party names a session (or a person) in the state.
type Party struct {
	// Session is the Claude Code session id (CLAUDE_CODE_SESSION_ID).
	Session string `json:"session,omitempty"`
	// HostSession is the desktop app's id for it (local_…); it survives a
	// CLI restart that changes Session.
	HostSession string `json:"hostSession,omitempty"`
	Name        string `json:"name"`
	// Person is whose agent the party is: a verified email, stable across
	// identity providers.
	Person string `json:"person,omitempty"`
	// Team is the person's team.
	Team string `json:"team,omitempty"`
	// Host is the machine or installation the party runs on.
	Host string `json:"host,omitempty"`
}

// Owner names whose agent the party is, of which team, on which host
// ("timo@example.com, team bumblebee, on lab"); empty when none is known.
func (p Party) Owner() string {
	var parts []string
	if p.Person != "" {
		parts = append(parts, p.Person)
	}
	if p.Team != "" {
		parts = append(parts, "team "+p.Team)
	}
	if p.Host != "" {
		parts = append(parts, "on "+p.Host)
	}
	return strings.Join(parts, ", ")
}

// Is reports whether p and o name the same session or person. Parties of
// two people, or on two hosts, are never the same, whatever their names.
func (p Party) Is(o Party) bool {
	switch {
	case p.Person != "" && o.Person != "" && !strings.EqualFold(p.Person, o.Person):
		return false
	case p.Host != "" && o.Host != "" && p.Host != o.Host:
		return false
	case p.Session != "" && p.Session == o.Session:
		return true
	case p.HostSession != "" && p.HostSession == o.HostSession:
		return true
	case p.Session == "" && o.Session == "" && p.Name != "" && p.Name == o.Name:
		return true
	}
	return false
}

// Supervisor is the session holding the watch.
type Supervisor struct {
	Party
	Since time.Time `json:"since"`

	rest rest
}

// Relay is the supervisor naming its successor: the successor's
// `supervisor start` takes the role and the grants until Expires; the
// outgoing supervisor keeps both meanwhile.
type Relay struct {
	From    Party     `json:"from"`
	To      Party     `json:"to"`
	At      time.Time `json:"at"`
	Expires time.Time `json:"expires"`
	// Taken is when the successor took the role; the record stays so the
	// outgoing supervisor learns it has been relieved.
	Taken time.Time `json:"taken,omitzero"`
	// Reported is when a watch said the relay was taken or expired; it says
	// it once.
	Reported time.Time `json:"reported,omitzero"`

	rest rest
}

// Open reports whether the relay can still be taken at now.
func (r *Relay) Open(now time.Time) bool {
	return r != nil && r.Taken.IsZero() && now.Before(r.Expires)
}

// Relief records a supervisor a relay relieved, so that its `supervisor
// status` keeps saying so after the successor relays onward, cancels a relay
// or is relieved in turn.
type Relief struct {
	Party Party `json:"party"`
	// By is the successor whose start took the relay, At when the relay
	// was opened and Taken when it was taken.
	By    Party     `json:"by"`
	At    time.Time `json:"at"`
	Taken time.Time `json:"taken"`

	rest rest
}

// CLI is what beekeeper saw of the supervisor's CLI process in the term
// Supervisor and Since name: the PID it ran as and since when it has been
// gone. A CLI back under the same session within supervisor.restartGrace of
// Gone is a restart; the grant rule holds whether it comes back or not.
type CLI struct {
	Supervisor Party     `json:"supervisor"`
	Since      time.Time `json:"since"`
	PID        int       `json:"pid,omitempty"`
	Gone       time.Time `json:"gone,omitzero"`

	rest rest
}

// Of reports whether the record is about sup's current term.
func (c *CLI) Of(sup *Supervisor) bool {
	return c != nil && sup != nil && c.Supervisor.Is(sup.Party) && c.Since.Equal(sup.Since)
}

// RelayDue is the watch's memory that it reported the relay due to the
// supervisor's term Supervisor and Since at RelayAt: a relay cancelled or
// expired removes it, so the next quiet moment reports it again, and a
// changed relayAt reports it anew.
type RelayDue struct {
	Supervisor Party     `json:"supervisor"`
	Since      time.Time `json:"since"`
	Reported   time.Time `json:"reported"`
	// Context is the supervisor's context in tokens when it was reported.
	Context int64 `json:"contextTokens"`
	// RelayAt is the relayAt in tokens it was reported at.
	RelayAt int64 `json:"relayAt,omitempty"`

	rest rest
}

// Of reports whether the record is about sup's current term.
func (r *RelayDue) Of(sup *Supervisor) bool {
	return r != nil && sup != nil && r.Supervisor.Is(sup.Party) && r.Since.Equal(sup.Since)
}

// At reports whether the record is about sup's current term at relayAt.
func (r *RelayDue) At(sup *Supervisor, relayAt int64) bool {
	return r.Of(sup) && r.RelayAt == relayAt
}

// Role is a relayed role's record: its holder and term, its last relay,
// the holders relays relieved, what beekeeper saw of its CLI and the relay
// due the watch reported. The supervisor's lives in State's own fields
// (SupervisorRole), the guide's in Guide.
type Role struct {
	Holder   *Supervisor `json:"holder,omitempty"`
	Relay    *Relay      `json:"relay,omitempty"`
	Relieved []Relief    `json:"relieved,omitempty"`
	CLI      *CLI        `json:"cli,omitempty"`
	RelayDue *RelayDue   `json:"relayDue,omitempty"`
	// Fed are the keys of what the guide's feed reported: the open notes
	// for a person and the sessions waiting on one, each said once.
	Fed []string `json:"fed,omitempty"`
	// Run is the highest run of the role numbered: "<Role> run <Run>".
	Run int `json:"run,omitempty"`
	// Asking is the note the guide asks its person now (beekeeper guide
	// next): served again until it is answered or done. AskingFrom is the
	// muster context of the central instance that holds it, empty for the
	// machine's own.
	Asking     int    `json:"asking,omitempty"`
	AskingFrom string `json:"askingFrom,omitempty"`

	rest rest
}

// SupervisorRole is the supervisor's record.
func (st *State) SupervisorRole() Role {
	return Role{Holder: st.Supervisor, Relay: st.Relay, Relieved: st.Relieved, CLI: st.SupervisorCLI, RelayDue: st.RelayDue, Run: st.SupervisorRun}
}

// SetSupervisorRole stores the supervisor's record.
func (st *State) SetSupervisorRole(r Role) {
	st.Supervisor, st.Relay, st.Relieved, st.SupervisorCLI, st.RelayDue, st.SupervisorRun = r.Holder, r.Relay, r.Relieved, r.CLI, r.RelayDue, r.Run
}

// LocalAsking is the machine's own note the guide asks now, 0 when it
// asks none or a central instance's.
func (r Role) LocalAsking() int {
	if r.AskingFrom != "" {
		return 0
	}
	return r.Asking
}

// GuideRole is the guide's record.
func (st *State) GuideRole() Role {
	if st.Guide == nil {
		return Role{}
	}
	return *st.Guide
}

// SetGuideRole stores the guide's record, none when it is empty.
func (st *State) SetGuideRole(r Role) {
	if r.Holder == nil && r.Relay == nil && len(r.Relieved) == 0 && len(r.Fed) == 0 && r.Run == 0 && r.Asking == 0 && r.AskingFrom == "" {
		st.Guide = nil
		return
	}
	st.Guide = &r
}

// Grant is the supervisor's word that a session may claim a resource.
type Grant struct {
	Resource string    `json:"resource"`
	To       Party     `json:"to"`
	By       Party     `json:"by"`
	At       time.Time `json:"at"`
	// UpgradeUnblock is why the supervisor granted a claim that the
	// resource's upgrade hold admits: the work that unblocks the upgrade.
	UpgradeUnblock string `json:"upgradeUnblock,omitempty"`

	rest rest
}

// Hold stops work on a target (a repository's merges, "github" for every
// GitHub call) until it is lifted or Until passes.
type Hold struct {
	Target string    `json:"target"`
	Reason string    `json:"reason"`
	By     Party     `json:"by"`
	At     time.Time `json:"at"`
	Until  time.Time `json:"until,omitzero"`
	// Except is what a merge hold lets through: a repository (the tool's own
	// during a tool-release window) or one pull request, owner/repo#n.
	Except string `json:"except,omitempty"`
	// ExceptSession is the session (by name) a merge hold lets through: its
	// merges and promotes run, a line's promoter promoting while the hold
	// keeps every other session's merges off the line.
	ExceptSession string `json:"exceptSession,omitempty"`
	// Tool is the binary whose release window this hold is, ToolFrom the
	// version it reported when the window opened, ToolPR the pull request
	// that opened it (0 for a promotion), ToolMerged whether it merged and
	// ToolRelease the release it produced. The window closes once its pull
	// request merged (or its promotion runs) and the tool reports another
	// version than ToolFrom (ToolRelease once known), also while the merge's
	// devctl still waits for the release workflow, or once its merge ended
	// with nothing merged, no release warranted or only a release candidate.
	// ToolUpdated is when beekeeper last ran the tool's update for the
	// window, ToolFailed that update's failure ("" when it ran clean: the
	// release's binary did not verify, or no newer one is published yet),
	// ToolChecked when a gate or the watch last claimed the read of the
	// window's state (the tool's version, its pull request), which they share.
	Tool        string    `json:"tool,omitempty"`
	ToolFrom    string    `json:"toolFrom,omitempty"`
	ToolPR      int       `json:"toolPR,omitempty"`
	ToolMerged  bool      `json:"toolMerged,omitempty"`
	ToolRelease string    `json:"toolRelease,omitempty"`
	ToolUpdated time.Time `json:"toolUpdated,omitzero"`
	ToolFailed  string    `json:"toolFailed,omitempty"`
	ToolChecked time.Time `json:"toolChecked,omitzero"`
	// LiftWhen is a shell command on this machine; the watch lifts the hold
	// once it exits 0.
	LiftWhen string `json:"liftWhen,omitempty"`
	// UpgradeTo is the target release of the upgrade an automatic upgrade
	// hold stands for.
	UpgradeTo string `json:"upgradeTo,omitempty"`
	// LiftedBy and LiftedAt record the lift of an upgrade hold: the watch
	// keeps a lifted hold, inactive, for as long as its upgrade runs.
	LiftedBy *Party    `json:"liftedBy,omitempty"`
	LiftedAt time.Time `json:"liftedAt,omitzero"`

	rest rest
}

// Active reports whether the hold still applies at now.
func (h Hold) Active(now time.Time) bool {
	return h.LiftedBy == nil && !h.Expired(now)
}

// Expired reports whether the hold's time has passed at now.
func (h Hold) Expired(now time.Time) bool {
	return !h.Until.IsZero() && !now.Before(h.Until)
}

// Excepts reports whether the hold lets a merge of repo#pr through.
func (h Hold) Excepts(repo string, pr int) bool {
	return h.Except != "" && (strings.EqualFold(h.Except, repo) || strings.EqualFold(h.Except, fmt.Sprintf("%s#%d", repo, pr)))
}

// Admits reports whether the hold lets the session named session through.
func (h Hold) Admits(session string) bool {
	return h.ExceptSession != "" && strings.EqualFold(strings.TrimSpace(h.ExceptSession), strings.TrimSpace(session))
}

// AwaitsOwnRelease reports whether the hold is a repository's whose lift
// probe waits on that repository's release or tag: only the repository's
// own promote cuts it, so the hold lets that promote through.
func (h Hold) AwaitsOwnRelease() bool {
	probe := strings.ToLower(h.LiftWhen)
	if !strings.Contains(h.Target, "/") || !strings.Contains(probe, strings.ToLower(h.Target)) {
		return false
	}
	words := strings.FieldsFunc(probe, func(r rune) bool { return r < 'a' || r > 'z' })
	return slices.ContainsFunc(words, func(w string) bool { return slices.Contains([]string{"release", "releases", "tag", "tags"}, w) })
}

// Keep is a roster entry's keep marker: who set it, when, why, and until
// when (zero: until lifted).
type Keep struct {
	By     Party     `json:"by"`
	At     time.Time `json:"at"`
	Until  time.Time `json:"until,omitzero"`
	Reason string    `json:"reason,omitempty"`

	rest rest
}

// Holds reports whether the marker still keeps its entry at now.
func (k *Keep) Holds(now time.Time) bool {
	return k != nil && (k.Until.IsZero() || now.Before(k.Until))
}

// Park is an agent's park: what it waits for, on which note (#<id>) or
// issue or pull request (owner/repo#n), and once that settled, since when
// it is resumable and the words its resume carries.
type Park struct {
	By    Party     `json:"by"`
	At    time.Time `json:"at"`
	On    string    `json:"on,omitempty"`
	Waits string    `json:"waits"`
	// Resumable is when the watch saw On settle; Answer is what settled it,
	// a person's answer word for word or the pull request's state.
	Resumable time.Time `json:"resumable,omitzero"`
	Answer    string    `json:"answer,omitempty"`
	// Told is when the supervisor's watch said the park waits on a person;
	// zero: not yet, or it waits on no person.
	Told time.Time `json:"told,omitzero"`

	rest rest
}

// Agent is an empty session registered as spare capacity.
type Agent struct {
	Party
	// Execution references a coordinating roster entry's remote process.
	// Its reachability and lifecycle are stored on the matching Start.
	Execution  *execution.Ref `json:"execution,omitempty"`
	Registered time.Time      `json:"registered"`
	Task       string         `json:"task,omitempty"`
	AssignedAt time.Time      `json:"assignedAt,omitzero"`
	IdleSince  time.Time      `json:"idleSince,omitzero"`
	LastTask   string         `json:"lastTask,omitempty"`
	// Keep marks an idle entry kept on purpose (agents keep): the doctor
	// never removes it for staleness nor archives its desktop session.
	Keep *Keep `json:"keep,omitempty"`
	// Park marks an agent that keeps its task but waits on a note, a pull
	// request or a person without holding a busy slot (agents park).
	Park *Park `json:"park,omitempty"`
	// Done says the agent reported its work finished (agents idle --done):
	// the doctor takes it off the roster and archives its desktop session
	// once its CLI is idle.
	Done bool `json:"done,omitempty"`
	// DesktopTurn is when the agent asked for a desktop turn (agents
	// desktop, agents start --desktop): the import or reopen after its
	// headless turn shows it in the desktop without waiting for the
	// desktop's window to lose the focus. Zero: none asked, or the desktop
	// was given it.
	DesktopTurn time.Time `json:"desktopTurn,omitzero"`
	// Import is the wait of the reopen that shows the agent in the desktop,
	// while it waits; nil: none waits.
	Import *ImportWait `json:"import,omitempty"`
	// ResumedWait is when the reopen after a headless turn that ended on a
	// background wait, its task open, resumed the agent headless (once per
	// task), and ResumedOn the wait it named; zero: never.
	ResumedWait time.Time `json:"resumedWait,omitzero"`
	ResumedOn   string    `json:"resumedOn,omitempty"`
	// Undelivered is why agents start delivered no turn of the agent's
	// task, which waits until agents wake delivers one; empty: delivered.
	Undelivered string `json:"undelivered,omitempty"`
	// Conversation is klaus-gateway's conversation the agent holds with its
	// person, a Slack thread; empty: none is open.
	Conversation string `json:"conversation,omitempty"`
	// GH is the gh the agent's shell resolves outside the sandbox, read at
	// its registration and each session start; GHAt when. Empty: not read.
	GH   string    `json:"gh,omitempty"`
	GHAt time.Time `json:"ghAt,omitzero"`
	// Harness is the agent harness the session runs on ("" or "claude",
	// "omp", "agy"); Model is the model it runs on.
	Harness string `json:"harness,omitempty"`
	Model   string `json:"model,omitempty"`

	rest rest
}

// ImportWait is a reopen waiting to show a session in the desktop: what
// holds it, since when, and its bound.
type ImportWait struct {
	On    string    `json:"on"`
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`

	rest rest
}

// Pending reports whether the wait still runs at now: a reopen that died
// leaves its record behind, past its bound.
func (w *ImportWait) Pending(now time.Time) bool { return w != nil && now.Before(w.Until) }

// The kinds of a note.
const (
	NoteDecision = "decision"
	NoteLogin    = "login"
)

// Note is an open item: a question for a person, a deadline.
type Note struct {
	ID   int       `json:"id"`
	For  string    `json:"for,omitempty"`
	Text string    `json:"text"`
	Due  time.Time `json:"due,omitzero"`
	// Default is what happens when nobody answers by Due.
	Default string    `json:"default,omitempty"`
	By      Party     `json:"by"`
	At      time.Time `json:"at"`
	// Fired is when a watch reported the note due; it reports it once.
	Fired time.Time `json:"fired,omitzero"`
	// Kind is NoteDecision for a question that waits on a person or
	// NoteLogin for a sign-in the watch closes once Until, a shell command,
	// exits 0.
	Kind  string `json:"kind,omitempty"`
	Until string `json:"until,omitempty"`
	// Refs are the issues and pull requests (owner/repo#n) the note asks
	// about: once every one is closed or merged, the watch closes the note
	// as overtaken, unless a closing keyword closed one and the note's
	// worker has not ended its task (Kept). Without refs the note is linked
	// to its filing session, and closes once that session is archived.
	Refs []string `json:"refs,omitempty"`
	// TaskEnded is when the worker that filed the note reported its task
	// over (agents idle): from then on its settled refs overtake the note,
	// a closing keyword's close included. Zero for a note no worker filed
	// or whose worker still runs.
	TaskEnded time.Time `json:"taskEnded,omitzero"`
	// Kept is why the watch keeps the note open although every ref is
	// closed or merged, said once per reason (NOTE KEPT).
	Kept string `json:"kept,omitempty"`
	// Question, StatusQuo, Options ("<label>: <consequence>" each) and
	// Recommend (the recommended option, 1-based) are a decision's parts as
	// its message renders them; Text joins them for the terminal.
	Question  string   `json:"question,omitempty"`
	StatusQuo string   `json:"statusQuo,omitempty"`
	Options   []string `json:"options,omitempty"`
	Recommend int      `json:"recommend,omitempty"`
	// Posted is the gateway's handle on the decision's message, once posted.
	Posted string `json:"posted,omitempty"`
	// Held is the board item the decision holds (note add --holds), its
	// first --ref, and AlsoHeld those of its further --ref: each one's
	// Status set to Blocked / Waiting until the decision closes, then set
	// back. nil and empty for a note that holds nothing.
	Held     *ItemHold  `json:"held,omitempty"`
	AlsoHeld []ItemHold `json:"alsoHeld,omitempty"`
	// Central is the muster context of the central instance that holds
	// the note, as a machine lists it beside its own; empty for the
	// machine's own note, and never stored.
	Central string `json:"central,omitempty"`

	rest rest
}

// Holds are the board items n holds: Held, then AlsoHeld.
func (n *Note) Holds() []ItemHold {
	if n.Held == nil {
		return nil
	}
	return append([]ItemHold{*n.Held}, n.AlsoHeld...)
}

// HoldOf is n's hold of the board item ref; nil when n holds no such item.
func (n *Note) HoldOf(ref string) *ItemHold {
	if n.Held != nil && strings.EqualFold(n.Held.Ref, ref) {
		return n.Held
	}
	for i := range n.AlsoHeld {
		if strings.EqualFold(n.AlsoHeld[i].Ref, ref) {
			return &n.AlsoHeld[i]
		}
	}
	return nil
}

// ItemHold is a board item a decision holds on its person: the issue, the
// Status the hold set and the Status it had before, which its release sets
// back.
type ItemHold struct {
	Ref    string `json:"ref"`
	Status string `json:"status"`
	Was    string `json:"was"`
}

// Option is a decision's option i (1-based) split into its label and its
// consequence; ok is false for none.
func (n *Note) Option(i int) (label, consequence string, ok bool) {
	if i < 1 || i > len(n.Options) {
		return "", "", false
	}
	label, consequence, _ = strings.Cut(n.Options[i-1], ":")
	return strings.TrimSpace(label), strings.TrimSpace(consequence), true
}

// Timer is a point in time the supervisor has to look at something ("check
// the rollout after 22:55").
type Timer struct {
	ID   int       `json:"id"`
	Due  time.Time `json:"due"`
	What string    `json:"what"`
	By   Party     `json:"by"`
	At   time.Time `json:"at"`
	// Fired is when a watch reported the timer; it reports it once.
	Fired time.Time `json:"fired,omitzero"`
	// When is a typed condition (pr-merged owner/repo#n, …) and Probe a
	// shell command that exits 0 once its condition holds: from Due, the
	// watch checks it every Every and fires the timer once it holds.
	When  string        `json:"when,omitempty"`
	Probe string        `json:"probe,omitempty"`
	Every time.Duration `json:"every,omitempty"`
	// Checked is when a watch last found the condition not holding, Reason
	// why: what the reference says, or why it could not be read
	// (Unreadable).
	Checked    time.Time `json:"checked,omitzero"`
	Reason     string    `json:"reason,omitempty"`
	Unreadable bool      `json:"unreadable,omitempty"`
	// Since is the first check that found the condition not holding; Stuck
	// is set once the watch said it has not held for a day.
	Since time.Time `json:"since,omitzero"`
	Stuck bool      `json:"stuck,omitempty"`
	// Until is the end of the wait: past it the timer fires as timed out,
	// or closes unfired when Expire is set.
	Until  time.Time `json:"until,omitzero"`
	Expire bool      `json:"expire,omitempty"`
	// Wake is the agent the firing wakes with What; Run a shell command it
	// runs, its exit code logged. Without either the watch's line says it.
	Wake string `json:"wake,omitempty"`
	Run  string `json:"run,omitempty"`
	// Repeat re-arms a timer that wakes or runs at the same local time:
	// daily, weekdays (Monday to Friday) or weekly. Such a timer stays open
	// until marked done.
	Repeat string `json:"repeat,omitempty"`

	rest rest
}

// The values of Timer.Repeat.
const (
	RepeatDaily    = "daily"
	RepeatWeekdays = "weekdays"
	RepeatWeekly   = "weekly"
)

// Repeats are the values of Timer.Repeat.
var Repeats = []string{RepeatDaily, RepeatWeekdays, RepeatWeekly}

// Next is the first time after now the repeating timer is due again, at its
// local time of day: a day or a week on, a weekday timer skipping the weekend.
func (t Timer) Next(now time.Time) time.Time {
	next := t.Due.Local()
	for !next.After(now) || t.Repeat == RepeatWeekdays && Weekend(next) {
		if t.Repeat == RepeatWeekly {
			next = next.AddDate(0, 0, 7)
		} else {
			next = next.AddDate(0, 0, 1)
		}
	}
	return next.UTC()
}

// Weekend reports whether t falls on a Saturday or a Sunday.
func Weekend(t time.Time) bool {
	d := t.Weekday()
	return d == time.Saturday || d == time.Sunday
}

// Auto reports whether the watch acts on the timer itself and closes it
// when it fires: a condition to wait on, an agent to wake or a command to
// run. A plain timer stays open until marked done.
func (t Timer) Auto() bool {
	return t.Conditional() || t.Wake != "" || t.Run != ""
}

// Conditional reports whether the timer waits on a condition.
func (t Timer) Conditional() bool { return t.When != "" || t.Probe != "" }

// Looked reports whether someone has to look at the timer by now: a plain
// timer whose time has come. An auto timer is the watch's to act on.
func (t Timer) Looked(now time.Time) bool { return !t.Auto() && !t.Due.After(now) }

// Due reports whether a watch has yet to report something due by now.
func Due(due, fired, now time.Time) bool {
	return !due.IsZero() && !due.After(now) && fired.IsZero()
}

// Record is what a session serves: the issue or epic (owner/repo#n) and
// what it waits on. Any session can have one, registered agent or not.
type Record struct {
	Session Party     `json:"session"`
	Issue   string    `json:"issue"`
	Waits   string    `json:"waits,omitempty"`
	By      Party     `json:"by"`
	At      time.Time `json:"at"`
	// Ended is when a watch saw the session's CLI gone; it reports it once.
	Ended time.Time `json:"ended,omitzero"`

	rest rest
}

// AlertOwner is the session that owns a firing alert (alerts own), until the
// session ends or the alert resolves. A party without a session id is a
// person, who owns it until it resolves.
type AlertOwner struct {
	// Alert is the alert's key in the alert baseline (fingerprint@start).
	Alert string    `json:"alert"`
	Name  string    `json:"name"`
	By    Party     `json:"by"`
	At    time.Time `json:"at"`
	// Ended is when a watch saw the owner's session gone.
	Ended time.Time `json:"ended,omitzero"`

	rest rest
}

// Person reports whether the owner is a person rather than a session.
func (o AlertOwner) Person() bool { return o.By.Session == "" && o.By.HostSession == "" }

// ModeBypass is Claude Code's bypassPermissions mode.
const ModeBypass = "bypassPermissions"

// App is an App the person owns and asked a consent, or one change of its
// permissions, for, on their word: the hook answers the consent click on
// its page, and the navigation to its permissions page and the clicks of
// that change, itself (guard.Click, guard.Navigation).
type App struct {
	// Name is the App's name as its consent page titles it ("Authorize
	// <name>" on GitHub).
	Name string `json:"name"`
	// ClientID is the OAuth client id the consent URL carries, "" for a
	// record without a consent.
	ClientID string `json:"clientID"`
	// Callbacks are the hosts the consent page's redirect may go to: the
	// callback URLs' hosts of the App's declaration; none for a record
	// without a consent.
	Callbacks []string `json:"callbacks"`
	// Org is the organisation whose settings pages the record covers: the
	// owner of the declaring repository.
	Org string `json:"org,omitempty"`
	// Permission is the one change of the App's permissions the person
	// asked for, "<scope>: <level>" ("Members: Read-only"); "" for a record
	// without one.
	Permission string `json:"permission,omitempty"`
	// CallbackURL is the one callback URL the person asked to add to the
	// App's settings, on a callback host its manifest declares; "" for a
	// record without one.
	CallbackURL string `json:"callbackURL,omitempty"`
	// Delete says the person asked for the App's deletion: the hook answers
	// its Advanced page's Delete GitHub App and the confirmation.
	Delete bool `json:"delete,omitempty"`
	// Google says the App is a Google OAuth client of the person's: the
	// hook answers its consent on Google's sign-in, by the callback hosts
	// the person named.
	Google bool `json:"google,omitempty"`
	// Account is the Google account a Google OAuth client's sign-in
	// chooses: the hook answers no other account on Google's chooser; of a
	// third party's OAuth client, the account the consent is given as.
	Account string `json:"account,omitempty"`
	// GitLab is the host of the self-hosted GitLab whose OAuth application
	// of the person's this is: the hook answers its authorize page there,
	// by the callback hosts the person named; "" for any other record.
	GitLab string `json:"gitlab,omitempty"`
	// OAuth is the sign-in host of a third party's OAuth client the
	// person consents to as their Account there: the hook answers that
	// client's consent page on that host; "" for any other record.
	OAuth string `json:"oauth,omitempty"`
	// Slack is the workspace host (<workspace>.slack.com) of a Slack app of
	// the person's whose install consent the hook answers there, SlackApp
	// its app id and WebhookChannel the channel its webhook posts to, which
	// the install keeps (lower case, without "#"; "" for an app without
	// one); Slack is "" for any other record.
	Slack          string `json:"slack,omitempty"`
	SlackApp       string `json:"slackApp,omitempty"`
	WebhookChannel string `json:"webhookChannel,omitempty"`
	// Declared is where the organisation declares the App:
	// <repo>@<commit>:<manifest path>, the local manifest of an App on the
	// person's own account or of a deletion, or the person's own
	// declaration of a GitLab application's client id.
	Declared string `json:"declared"`
	// Word is the person's words that asked for the grant.
	Word string    `json:"word"`
	By   Party     `json:"by"`
	At   time.Time `json:"at"`

	rest rest
}

// AppRun is a running `beekeeper app create`: the App it creates, the
// loopback listener its manifest page and GitHub's redirect go to, and its
// process. The hook answers GitHub's Create click on the manifest page and
// the App's Install click while the listener answers.
type AppRun struct {
	Name string `json:"name"`
	// Org is the account the App is created for: an organisation, or a
	// personal account (app create --manifest --user).
	Org string `json:"org"`
	// Declared is where the App is declared: its manifest in the
	// organisation's repository, or the local manifest file.
	Declared string `json:"declared"`
	// Listener is the run's loopback address, host:port.
	Listener string    `json:"listener"`
	PID      int       `json:"pid"`
	By       Party     `json:"by"`
	At       time.Time `json:"at"`

	rest rest
}

// AppCreated is an App `beekeeper app create` created: beekeeper's ledger of
// its own Apps. The hook answers its Install click, and the deletion of one
// whose credentials were stored nowhere.
type AppCreated struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
	ID   int64  `json:"id"`
	// Org is the organisation the App was created for.
	Org      string `json:"org"`
	Declared string `json:"declared"`
	// Stored is where its credentials went (a vault item, a SOPS file), ""
	// for nowhere.
	Stored string    `json:"stored,omitempty"`
	By     Party     `json:"by"`
	At     time.Time `json:"at"`

	rest rest
}

// Start is a session `beekeeper agents start` started: the id beekeeper
// chose and the permission mode it passed, recorded before the session
// existed. A session cannot add itself: its id is only ever written here by
// the start that created it.
type Start struct {
	Party
	Execution *ExecutionRecord `json:"execution,omitempty"`
	Mode      string           `json:"mode"`
	Dir       string           `json:"dir"`
	By        Party            `json:"by"`
	At        time.Time        `json:"at"`
	// Harness is the agent harness started: "" for Claude Code, "omp".
	Harness string `json:"harness,omitempty"`
	// Model is the model the session was started on (--model, as its first
	// reply names it), which its turns keep; empty: none was chosen.
	Model string `json:"model,omitempty"`
	// Background says the session runs as a Claude Code background session
	// (agents.mode background): it has no desktop session, and its wakes go
	// by its messaging, a stopped one's by claude --bg --resume.
	Background bool `json:"background,omitempty"`

	rest rest
}

// ExecutionRecord is the coordinator's durable admission/activation journal.
// Request includes the private component callback credential and must never
// be returned by an operator status or logs command.
type ExecutionRecord struct {
	Completed       bool                               `json:"completed,omitempty"`
	Messages        []ExecutionMessage                 `json:"messages,omitempty"`
	Handle          execution.Handle                   `json:"handle"`
	Request         execution.StartRequest             `json:"request"`
	RequestDigest   string                             `json:"requestDigest"`
	Selector        string                             `json:"selector"`
	Phase           string                             `json:"phase"`
	Provider        string                             `json:"provider"`
	Bucket          string                             `json:"bucket,omitempty"`
	ReservedAt      time.Time                          `json:"reservedAt"`
	Observation     execution.Observation              `json:"observation"`
	StopRequest     *execution.StopRequest             `json:"stopRequest,omitempty"`
	ControlSequence uint64                             `json:"controlSequence,omitempty"`
	ControlResults  map[string]execution.ControlResult `json:"controlResults,omitempty"`
	rest            rest
}

type ExecutionMessage struct {
	TextDigest string             `json:"textDigest,omitempty"`
	Resolution string             `json:"resolution,omitempty"`
	Envelope   execution.Envelope `json:"envelope"`
	Key        string             `json:"key"`
	Stage      string             `json:"stage"`
	Resume     *Park              `json:"resume,omitempty"`
	rest       rest
}

func (r *ExecutionRecord) Terminal() bool {
	if r.StopRequest != nil && !r.Observation.StopAcknowledged {
		return false
	}
	return r.Observation.StopAcknowledged || r.Observation.ProcessState == execution.Exited || r.Observation.ProcessState == execution.Failed
}

// Archive is the desktop session of an agent that left the roster while
// its archive could not be done (its CLI ran a turn, no steward recorded
// it): the doctor asks for it again until the desktop records it.
type Archive struct {
	// Party is the agent that left the roster.
	Party
	// Host is its desktop session.
	Host  string    `json:"host"`
	Since time.Time `json:"since"`
	// Why says why it stayed the last time.
	Why string `json:"why"`
	// Tries counts the stewards' turns asked for it, Tried the last.
	Tries int       `json:"tries,omitempty"`
	Tried time.Time `json:"tried,omitzero"`
	// Looked is the last doctor's run that found no steward to ask.
	Looked time.Time `json:"looked,omitzero"`
	// Warmed is when the desktop was last made to warm the session's own
	// CLI to archive itself: the watch leaves that CLI running a while.
	Warmed time.Time `json:"warmed,omitzero"`

	rest rest
}

// Decline is a steward's answer to an archive request that archived
// nothing: its desktop session, when, and the first line of its reply.
type Decline struct {
	Host string    `json:"host"`
	At   time.Time `json:"at"`
	Why  string    `json:"why"`

	rest rest
}

// Report is one run of the scheduled status reporter: the session the
// standby watch started for an interval's slot, until it posted, timed out
// or failed to start.
type Report struct {
	Party
	// Unit is the transient user unit its turn runs in.
	Unit string `json:"unit"`
	// Slot is the start of the interval it reports on.
	Slot    time.Time `json:"slot"`
	Started time.Time `json:"started"`
	// Ended is when beekeeper took it off the roster; zero while it runs.
	Ended time.Time `json:"ended,omitzero"`
	// Outcome is posted, timeout or failed: <why>; empty while it runs.
	Outcome string `json:"outcome,omitempty"`
	// Skipped is the latest slot skipped while it ran.
	Skipped time.Time `json:"skipped,omitzero"`

	rest rest
}

// ReportThread is the Slack thread of a posted status report, whose
// replies by the report's author the feedback watch delivers.
type ReportThread struct {
	// Report is the reporter's name.
	Report  string    `json:"report"`
	Channel string    `json:"channel"`
	TS      string    `json:"ts"`
	Posted  time.Time `json:"posted"`
	// Seen is the ts of the last reply delivered.
	Seen string `json:"seen,omitempty"`

	rest rest
}

// ReportPause pauses the scheduled reporter until it is resumed: from Since,
// or, while Final is set, from the start of one final report due then.
type ReportPause struct {
	Final time.Time `json:"final,omitzero"`
	Since time.Time `json:"since,omitzero"`
	By    Party     `json:"by"`

	rest rest
}

// Paused reports whether no scheduled report starts.
func (p *ReportPause) Paused() bool { return p != nil && !p.Since.IsZero() }

// FinalDue reports whether the final report is due at now.
func (p *ReportPause) FinalDue(now time.Time) bool {
	return p != nil && !p.Final.IsZero() && !now.Before(p.Final)
}

// Running reports whether the reporter has yet to end.
func (r *Report) Running() bool { return r != nil && r.Ended.IsZero() }

// BypassStart returns the start of session when beekeeper started it in
// bypassPermissions.
func (st *State) BypassStart(session string) (Start, bool) {
	if session == "" {
		return Start{}, false
	}
	return st.BypassStartOf(Party{Session: session})
}

// BypassStartOf returns the start of p when beekeeper started it in
// bypassPermissions, matched by its CLI session id or its desktop host id:
// the desktop gives a session's restarted CLI a new session id and keeps
// its host id.
func (st *State) BypassStartOf(p Party) (Start, bool) {
	for _, s := range st.Starts {
		if s.Mode == ModeBypass && s.of(p) {
			return s, true
		}
	}
	return Start{}, false
}

// StartIndex is the index in Starts of p's start in any mode, matched as
// BypassStartOf matches; -1 when beekeeper did not start it.
func (st *State) StartIndex(p Party) int {
	return slices.IndexFunc(st.Starts, func(s Start) bool { return s.of(p) })
}

// of reports whether s is the start of p, by its CLI session id or its
// desktop host id; a name alone matches no start.
func (s Start) of(p Party) bool {
	return (p.Session != "" || p.HostSession != "") && s.Is(Party{Session: p.Session, HostSession: p.HostSession})
}

// Knows reports whether p is a session beekeeper started (any mode), holds
// the supervisor's or the guide's role, or is an agent on the roster: the
// sessions the hooks act in wherever they run.
func (st *State) Knows(p Party) bool {
	if p.Session == "" && p.HostSession == "" {
		return false
	}
	for _, s := range st.Starts {
		if s.Is(p) {
			return true
		}
	}
	for _, h := range []*Supervisor{st.Supervisor, st.GuideRole().Holder} {
		if h != nil && h.Is(p) {
			return true
		}
	}
	for _, a := range st.Agents {
		if a.Is(p) {
			return true
		}
	}
	return false
}

// State is the whole document.
type State struct {
	Supervisor *Supervisor `json:"supervisor,omitempty"`
	// Relay is the supervisor's last relay: open, taken or expired.
	Relay *Relay `json:"relay,omitempty"`
	// Relieved are the supervisors relays relieved, one per party.
	Relieved []Relief `json:"relieved,omitempty"`
	// SupervisorCLI is what beekeeper saw of the supervisor's CLI.
	SupervisorCLI *CLI `json:"supervisorCLI,omitempty"`
	// SupervisorRun is the highest supervisor run numbered.
	SupervisorRun int `json:"supervisorRun,omitempty"`
	// RelayDue is the relay due the watch reported to the supervisor.
	RelayDue *RelayDue `json:"relayDue,omitempty"`
	// Guide is the guide's role, the supervisor's counterpart for the
	// person's decisions.
	Guide *Role `json:"guide,omitempty"`
	// Grants are queued per resource in the order given.
	Grants []Grant `json:"grants,omitempty"`
	// Released is when each resource was last released; a grant's TTL runs
	// from the later of it and the grant.
	Released map[string]time.Time `json:"released,omitempty"`
	Holds    []Hold               `json:"holds,omitempty"`
	Agents   []Agent              `json:"agents,omitempty"`
	// Parks are the parks of sessions off the roster (agents park by a
	// session no roster entry names), one per session, its By: the watch
	// settles and resumes them as it does a roster agent's.
	Parks []Park `json:"parks,omitempty"`
	Notes []Note `json:"notes,omitempty"`
	// Defaulted are the decisions closed by their default within the last
	// day: an answer that arrives meanwhile replaces the default.
	Defaulted []Note  `json:"defaulted,omitempty"`
	NextNote  int     `json:"nextNote,omitempty"`
	Timers    []Timer `json:"timers,omitempty"`
	NextTimer int     `json:"nextTimer,omitempty"`
	// Records say which session serves which issue.
	Records []Record `json:"records,omitempty"`
	// Starts are the sessions beekeeper started, what the permission hook
	// answers for.
	Starts []Start `json:"starts,omitempty"`
	// Apps are the Apps the person owns and asked a consent for, what the
	// hook answers a consent click for.
	Apps []App `json:"apps,omitempty"`
	// AppRuns are the running `beekeeper app create` commands.
	AppRuns []AppRun `json:"appRuns,omitempty"`
	// AppsCreated are the Apps `beekeeper app create` created.
	AppsCreated []AppCreated `json:"appsCreated,omitempty"`
	// Archives are the desktop sessions of agents off the roster the
	// doctor still owes an archive.
	Archives []Archive `json:"archives,omitempty"`
	// ArchivesSeeded says the doctor owed the archives of the finished
	// workers whose desktop CLI ran on before it kept Archives.
	ArchivesSeeded bool `json:"archivesSeeded,omitempty"`
	// FinishedSeeded says the doctor owed the archives of all finished
	// workers whose desktop record stayed unarchived, a CLI running or not.
	FinishedSeeded bool `json:"finishedSeeded,omitempty"`
	// DoneSeeded says the doctor owed those archives again, of the agents
	// that reported done and the relieved role runs no person typed in, once
	// a finished session's own warmed CLI archived it.
	DoneSeeded bool `json:"doneSeeded,omitempty"`
	// Declines are the stewards that declined an archive request, which
	// the doctor asks for none for a while.
	Declines []Decline `json:"declines,omitempty"`
	// BudgetETag makes the budget probe a conditional request (a 304
	// costs no budget).
	BudgetETag string `json:"budgetETag,omitempty"`
	// Budget is the last reading of the GitHub core budget.
	Budget *Budget `json:"budget,omitempty"`
	// Merges are the wrapped devctl pr merge runs, per lane: waiting in join
	// order, running, and settling until the lane's installation rolled them.
	Merges []Merge `json:"merges,omitempty"`
	// Failed are the gated merges whose last run merged nothing, one per
	// pull request, shown on their lane until its next attempt joins it.
	Failed []Failed `json:"failed,omitempty"`
	// Owed are the outcomes of detached runs their owners have not heard
	// yet, until the watch hands each over.
	Owed []Owed `json:"owed,omitempty"`
	// Report is the scheduled status reporter's current or last run.
	Report *Report `json:"report,omitempty"`
	// ReportThreads are the posted reports' Slack threads the feedback
	// watch reads, while feedback.window lasts.
	ReportThreads []ReportThread `json:"reportThreads,omitempty"`
	// AlertOwners are the owners of the firing alerts, one per alert.
	AlertOwners []AlertOwner `json:"alertOwners,omitempty"`
	// ReportPause pauses the scheduled reporter, after one final report.
	ReportPause *ReportPause `json:"reportPause,omitempty"`
	// Writer is the newest beekeeper that saved the state.
	Writer *Writer `json:"writer,omitempty"`
	// WorkerReports are the reports workers finished with (agents idle
	// --done) that the supervisor's watch has not printed yet.
	WorkerReports []WorkerReport `json:"workerReports,omitempty"`
	// Urgent are the merges marked urgent (lanes urgent): waiting for their
	// gate, or run under the GitHub budget floor in a reset window that has
	// not ended.
	Urgent []Urgent `json:"urgent,omitempty"`
	// Pairs are the merges declared to land together (the gate's --pair):
	// neither starts unless the other is next in its lane.
	Pairs []Pair `json:"pairs,omitempty"`
	// RoleLines are lines for a role's holder that found none to tell: the
	// role's next start says them.
	RoleLines []RoleLine `json:"roleLines,omitempty"`
	// Stop is the stop of every start, wake, revive, import and merge
	// (beekeeper stop) while it stands; beekeeper resume lifts it.
	Stop *Stop `json:"stop,omitempty"`

	rest rest
}

// Stop is a standing beekeeper stop: who stopped the machine's sessions,
// when and why, and what the stop changed, which beekeeper resume puts
// back.
type Stop struct {
	Reason string    `json:"reason"`
	By     Party     `json:"by"`
	At     time.Time `json:"at"`
	// Holds are the targets of the holds the stop set (merges, github); a
	// full hold that stood before the stop is not among them and stays.
	Holds []string `json:"holds,omitempty"`
	// Replaced are the holds on those targets the stop's replaced: one that
	// let a merge through, or that would have ended by itself while the
	// stop stands (a time, a probe). Resume puts each back as it was.
	Replaced []Hold `json:"replaced,omitempty"`
	// Units are the installed units the stop stopped (the standby service,
	// stop.units), which resume starts again.
	Units []string `json:"units,omitempty"`
	// Sessions are the names of the sessions the stop ended.
	Sessions []string `json:"sessions,omitempty"`

	rest rest
}

// RoleLine is a line for the holder of a role (supervisor, guide) that no
// session held, or whose holder could not be told, when it was said: the
// answer to a note a run of the role filed.
type RoleLine struct {
	Role string    `json:"role"`
	Line string    `json:"line"`
	At   time.Time `json:"at"`

	rest rest
}

// Urgent is a merge marked urgent, a privacy or security fix's: under the
// GitHub budget floor it runs, once per reset window, while the budget keeps
// github.urgentBound for it.
type Urgent struct {
	Repo string `json:"repo"`
	PR   int    `json:"pr"`
	// By is who asked for it, Reason why.
	By     Party     `json:"by"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
	// Window is the reset of the budget window the merge ran under the
	// floor in; zero while it waits for its gate.
	Window time.Time `json:"window,omitzero"`
	// Remaining is the budget when it ran, Spent what its run drew from
	// it: nil while it runs, -1 unread.
	Remaining int  `json:"remaining,omitempty"`
	Spent     *int `json:"spent,omitempty"`

	rest rest
}

// Key is the urgent merge's owner/repo#n.
func (u Urgent) Key() string { return Merge{Repo: u.Repo, PR: u.PR}.Key() }

// Ran says whether the merge ran under the floor in the window that resets
// at Window.
func (u Urgent) Ran() bool { return !u.Window.IsZero() }

// Pair is two merges that must land together, a change split over two
// repositories: the gate holds the first at its turn until the other is
// next in its lane too, then starts both, and a refusal of either refuses
// both with one reason.
type Pair struct {
	// Keys are the two merges, owner/repo#n each.
	Keys []string  `json:"keys"`
	By   Party     `json:"by"`
	At   time.Time `json:"at"`
	// Ready is when each merge's gate last found only the pair holding it
	// back, by key.
	Ready map[string]time.Time `json:"ready,omitempty"`
	// Started is the merge that started first; its sibling then starts
	// outside merge.cap and merge.sessionCap, the slot taken for both.
	Started string `json:"started,omitempty"`
	// Refused is why one of the two was refused, RefusedKey which: the
	// other's gate refuses it with the same reason.
	Refused    string `json:"refused,omitempty"`
	RefusedKey string `json:"refusedKey,omitempty"`

	rest rest
}

// Sibling is the pair's other merge than key, "" when key is not in it.
func (p Pair) Sibling(key string) string {
	switch {
	case len(p.Keys) != 2:
		return ""
	case p.Keys[0] == key:
		return p.Keys[1]
	case p.Keys[1] == key:
		return p.Keys[0]
	}
	return ""
}

// PairOf is the index of the pair key is in, -1 when none.
func (s *State) PairOf(key string) int {
	return slices.IndexFunc(s.Pairs, func(p Pair) bool { return p.Sibling(key) != "" })
}

// WorkerReport is the report a worker finished its task with: what it
// delivered and the problems it found (broken functions, ways around them,
// follow-ups). The supervisor's watch prints it once and drops it; the event
// log keeps it (agents.report, agents.problem).
type WorkerReport struct {
	By       Party     `json:"by"`
	At       time.Time `json:"at"`
	Task     string    `json:"task,omitempty"`
	Text     string    `json:"text"`
	Problems []string  `json:"problems,omitempty"`

	rest rest
}

// Writer is the version of the newest beekeeper that saved the state.
type Writer struct {
	Version string `json:"version"`

	rest rest
}

// StaleWriterError is a save refused because a newer release wrote the
// state, or a state a newer release wrote that this binary does not load
// (Load): this binary would save it by rules that release changed (a watch,
// a start's reopen or a gate call left running by an install). The state
// stays as it is; the process ends or is restarted, and the installed
// beekeeper runs its command.
type StaleWriterError struct {
	PID     int
	Command string
	// Version is this binary's, Newer the one that wrote the state.
	Version, Newer string
	// Load is why the state does not decode, nil for a refused save.
	Load error
}

func (e *StaleWriterError) Error() string {
	if e.Load != nil {
		return fmt.Sprintf("pid %d (%s) runs beekeeper %s, older than the %s that wrote the state: it does not load it (%v); restart it on the installed beekeeper", e.PID, e.Command, e.Version, e.Newer, e.Load)
	}
	return fmt.Sprintf("pid %d (%s) runs beekeeper %s, older than the %s that wrote the state: its save is refused; restart it on the installed beekeeper", e.PID, e.Command, e.Version, e.Newer)
}

func (e *StaleWriterError) Unwrap() error { return e.Load }

// Budget is one reading of the GitHub core budget.
type Budget struct {
	Remaining int       `json:"remaining"`
	Limit     int       `json:"limit"`
	Reset     time.Time `json:"reset"`
	At        time.Time `json:"at"`
	// GraphQL is the last reading of the GraphQL limit; nil before the
	// first.
	GraphQL *GraphQL `json:"graphql,omitempty"`

	rest rest
}

// GraphQL is one reading of the GitHub GraphQL limit.
type GraphQL struct {
	Remaining int `json:"remaining"`
	Limit     int `json:"limit"`
	// Reset is when a refusal ends; zero when GitHub named no time.
	Reset time.Time `json:"reset,omitzero"`
	// Refused is GitHub's words while it refused GraphQL calls.
	Refused   string    `json:"refused,omitempty"`
	Secondary bool      `json:"secondary,omitempty"`
	At        time.Time `json:"at"`

	rest rest
}

// Blocks says the reading's refusal still holds at now: one without a
// reset holds until a reading says otherwise.
func (g *GraphQL) Blocks(now time.Time) bool {
	return g != nil && g.Refused != "" && (g.Reset.IsZero() || now.Before(g.Reset))
}

// The phases of a Merge.
const (
	Waiting  = "waiting"
	Running  = "running"
	Settling = "settling"
)

// Merge is one devctl pr merge the gate holds in its lane's queue.
type Merge struct {
	Repo string `json:"repo"`
	PR   int    `json:"pr"`
	Lane string `json:"lane"`
	By   Party  `json:"by"`
	PID  int    `json:"pid"`
	// Child is a running merge's devctl, in a session of its own: it merges
	// on when the gate's caller, or the gate, is gone.
	Child int `json:"child,omitempty"`
	// Run is the files' base of a queued merge's own run (the gate under
	// --queued and its merge-child), whose merge-child owes the outcome to
	// the owner: a run cut before that merge-child wrote its exit code
	// leaves the outcome to the watch. Cleared once the cut is owed.
	Run string `json:"run,omitempty"`
	// Output is the file the gate's stdout writes to, a background task's
	// output file: a TaskStop naming that task stops the merge.
	Output string `json:"output,omitempty"`
	Phase  string `json:"phase"`
	// Joined orders the queue; a rerun within the queue TTL keeps it.
	Joined  time.Time `json:"joined"`
	Seen    time.Time `json:"seen"`
	Started time.Time `json:"started,omitzero"`
	// Finished and Exit are when and how the merge's run ended.
	Finished time.Time `json:"finished,omitzero"`
	Exit     int       `json:"exit,omitempty"`
	// Seeded marks a place queued on a session's behalf (lanes queue): it
	// survives refusals and keeps the seed TTL until the merge runs.
	Seeded bool `json:"seeded,omitempty"`
	// Outside marks a merge run outside the gate (lanes settle): waiting, it
	// heads its lane until its own devctl pr merge runs or GitHub reports it
	// merged; merged, it settles its lane like a gated merge.
	Outside bool `json:"outside,omitempty"`
	// Checked is when GitHub last reported an outside merge not merged yet.
	Checked time.Time `json:"checked,omitzero"`
	// Candidate is the release candidate a promotion (PR 0) was queued for,
	// the newest one at queue time, empty when there was none: its turn
	// refuses when the newest candidate is another by then.
	Candidate string `json:"candidate,omitempty"`
	// Release is the tag the merge released, empty when unknown.
	Release string `json:"release,omitempty"`
	// ReleaseFailed marks a merge whose release failed (devctl exit 6): its
	// auto-release run or tag pipeline failed, nothing rolls, and the lane
	// settles at once.
	ReleaseFailed bool `json:"releaseFailed,omitempty"`
	// HandCut is the merge's base branch when no Auto-release run tags a
	// merge into it (its tags are cut by hand): devctl awaits no release and
	// the merge leaves its lane once merged.
	HandCut string `json:"handCut,omitempty"`
	// Roll names the HelmReleases (namespace/name) that must reach Release
	// before the lane frees.
	Roll []string `json:"roll,omitempty"`
	// Reset is the GitHub budget reset a merge queued under the floor waits
	// for: for merge.resetFirst from then it goes before the waits, and the
	// older such merges before it. Its start clears it.
	Reset time.Time `json:"reset,omitzero"`

	rest rest
}

// Owed is a detached run's outcome its owner has not heard: recorded by the
// run's merge-child before it hands the outcome over, so a merge-child stopped
// or killed on the way (a desktop restart, its user manager stopping) leaves
// it to the watch. Gate and Child are the pids of the run's gate and
// merge-child, which hand it over while either runs; Heard is the gate's
// heard marker, which says its caller heard it. Line is the outcome; Ask
// marks a merge stopped before devctl reported its end, whose outcome is
// its pull request's state on GitHub (Repo, PR), Command and Stop naming
// the command and its stop. Tried is when a hand-over was last claimed, Why
// why the last one failed.
type Owed struct {
	Owner   Party     `json:"owner"`
	Line    string    `json:"line"`
	Ask     bool      `json:"ask,omitempty"`
	Repo    string    `json:"repo,omitempty"`
	PR      int       `json:"pr,omitempty"`
	Command string    `json:"command,omitempty"`
	Stop    string    `json:"stop,omitempty"`
	Gate    int       `json:"gate,omitempty"`
	Child   int       `json:"child"`
	Heard   string    `json:"heard,omitempty"`
	Since   time.Time `json:"since"`
	Tried   time.Time `json:"tried,omitzero"`
	Why     string    `json:"why,omitempty"`

	rest rest
}

// Key tells owed outcomes apart: the merge-child that owes it and when.
func (o Owed) Key() string {
	return fmt.Sprintf("%d@%s", o.Child, o.Since.UTC().Format(time.RFC3339Nano))
}

// Failed is a gated merge's run that merged nothing: when, devctl's exit
// code and its reason (the verdict and the failed checks), for whoever it ran
// for.
type Failed struct {
	Repo   string    `json:"repo"`
	PR     int       `json:"pr"`
	Lane   string    `json:"lane"`
	By     Party     `json:"by"`
	At     time.Time `json:"at"`
	Exit   int       `json:"exit"`
	Reason string    `json:"reason,omitempty"`

	rest rest
}

// Key is the failed merge's owner/repo#n, or owner/repo promote.
func (f Failed) Key() string { return Merge{Repo: f.Repo, PR: f.PR}.Key() }

// Key is the merge's repository and number, owner/repo#n, or owner/repo
// promote for a promotion (PR 0).
func (m Merge) Key() string {
	if m.PR == 0 {
		return m.Repo + " promote"
	}
	return fmt.Sprintf("%s#%d", m.Repo, m.PR)
}

// Event is one line of events.jsonl. At is written in UTC (RFC 3339 with a
// trailing Z), so a reader can compare it as a string.
type Event struct {
	At     time.Time `json:"at"`
	By     Party     `json:"by"`
	Verb   string    `json:"verb"`
	Detail string    `json:"detail"`
	// Subject is the issue or pull request the event concerns, owner/repo#n,
	// on the events the sink may dispatch (config.SinkVerbs); empty on the
	// rest.
	Subject string `json:"subject,omitempty"`
}

// Store is the seam every command reads and changes the state through.
type Store interface {
	// Dir is the machine-local directory beside the state: the merge
	// gate's and the timers' side files.
	Dir() string
	// Read returns the current state.
	Read() (*State, error)
	// Peek returns the current state without waiting on a writer.
	Peek() (*State, error)
	// Update runs fn on the state and saves the result together with the
	// events fn returns; when fn fails nothing is saved.
	Update(fn func(*State) ([]Event, error)) error
	// UpdateWithin is Update for a caller that must not wait long, a
	// session's start for one: a lock busy past wait fails it.
	UpdateWithin(wait time.Duration, fn func(*State) ([]Event, error)) error
	// Exclusive runs fn while no other process holds the state, writing
	// nothing.
	Exclusive(fn func() error) error
	// Log appends events that change no state, giving up on a busy lock.
	Log(events ...Event) error
	// Record appends events that change no state and must not be lost,
	// waiting for the lock as an update does.
	Record(events ...Event) error
	// RecordWithin is Record for a caller that must not wait long, an
	// agent's hand-over note for one: a lock busy past wait fails it with
	// a LockBusyError, the events not appended.
	RecordWithin(wait time.Duration, events ...Event) error
	// Events returns the last n events keep accepts, oldest first.
	Events(n int, keep func(Event) bool) ([]Event, error)
	// Follow returns every event keep accepts, oldest first, as Events(0,
	// keep); a reader that calls it again under the same name, with the
	// same keep, may be handed what it read before and only what was logged
	// since read.
	Follow(name string, keep func(Event) bool) ([]Event, error)
	// ReadFile decodes a JSON side file; found is false when it is missing.
	ReadFile(name string, v any) (found bool, err error)
	// WriteFile replaces a JSON side file.
	WriteFile(name string, v any) error
}

// FileStore is the Store in a directory of the machine: state.json under a
// flock, events.jsonl beside it.
type FileStore struct {
	dir string
	// version is the binary's, which every save stamps or judges.
	version string
	// follows are the Follow readers' places in the log, by name; refused
	// says a save was refused and logged, once per process.
	mu      sync.Mutex
	follows map[string]*followed
	refused bool
}

// followed is how far a Follow reader has read the log, and what it kept.
type followed struct {
	file   os.FileInfo
	offset int64
	kept   []Event
}

var _ Store = (*FileStore)(nil)

// Open returns the file store in dir, creating the directory, saving as
// this binary's version.
func Open(dir string) (*FileStore, error) { return OpenVersion(dir, project.Version()) }

// OpenVersion is Open saving as version: a release stamps the state and is
// refused by a newer one's, a build without a release version does neither.
func OpenVersion(dir, version string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &FileStore{dir: dir, version: version}, nil
}

// Dir is the store's directory.
func (s *FileStore) Dir() string { return s.dir }

func (s *FileStore) path(name string) string { return filepath.Join(s.dir, name) }

// Read returns the current state under a shared lock.
func (s *FileStore) Read() (*State, error) {
	unlock, err := s.lock(true, lockWait)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.load()
}

// lockWait bounds every wait for the state lock but Log's: a holder that
// stops (a frozen or stopped process) fails the commands behind it with its
// PID and age (LockBusyError) instead of starving them.
const lockWait = 2 * time.Minute

// LockBusyError is a wait for the state lock that ended before its holder
// released it: who holds it, since when, as the holder recorded it; PID 0
// is a holder without a record (a reader, or an older beekeeper).
type LockBusyError struct {
	PID     int
	Command string
	Since   time.Time
	Waited  time.Duration
}

func (e *LockBusyError) Error() string {
	if e.PID == 0 {
		return fmt.Sprintf("the state lock is busy: still held after %s by a process without a holder record (a reader, or a beekeeper before the record)", e.Waited.Round(time.Millisecond))
	}
	return fmt.Sprintf("the state lock is busy: held by pid %d (%s) for %s, waited %s; a stopped or frozen holder releases it when it resumes or ends",
		e.PID, e.Command, time.Since(e.Since).Round(time.Second), e.Waited.Round(time.Millisecond))
}

// lockHolder is the exclusive holder's record beside the lock, removed
// before the lock is released.
type lockHolder struct {
	PID     int       `json:"pid"`
	Command string    `json:"command"`
	At      time.Time `json:"at"`
}

// lock takes the state lock, shared or exclusive, waiting at most wait;
// an exclusive holder records itself for the waiters' LockBusyError.
func (s *FileStore) lock(shared bool, wait time.Duration) (unlock func(), err error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	l := flock.New(s.path("state.lock"))
	take := l.TryLockContext
	if shared {
		take = l.TryRLockContext
	}
	ok, err := take(ctx, 10*time.Millisecond)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	if !ok {
		return nil, s.busy(wait)
	}
	if shared {
		return func() { _ = l.Unlock() }, nil
	}
	holder := s.path(holderFile)
	_ = writeJSON(holder, lockHolder{PID: os.Getpid(), Command: command(os.Args), At: time.Now()})
	return func() {
		_ = os.Remove(holder)
		_ = l.Unlock()
	}, nil
}

const holderFile = "state.lock.holder"

// busy is the LockBusyError of a wait that ended after waited: a record
// of a process that ended is a holder without one.
func (s *FileStore) busy(waited time.Duration) error {
	e := &LockBusyError{Waited: waited}
	var h lockHolder
	if raw, err := os.ReadFile(s.path(holderFile)); err == nil && json.Unmarshal(raw, &h) == nil && proc.Alive(h.PID) {
		e.PID, e.Command, e.Since = h.PID, h.Command, h.At
	}
	return e
}

// Peek returns the current state without taking the lock, for a caller
// that must never wait on it (a permission hook): a write replaces the file
// in one rename, so the document read is always a whole one.
func (s *FileStore) Peek() (*State, error) { return s.load() }

// Update runs fn on the state under the exclusive lock and writes the result
// back atomically together with the events fn returns. When fn fails nothing
// is written, nor when a newer release wrote the state (StaleWriterError).
func (s *FileStore) Update(fn func(*State) ([]Event, error)) error {
	return s.UpdateWithin(lockWait, fn)
}

// UpdateWithin is Update waiting at most wait for the lock.
func (s *FileStore) UpdateWithin(wait time.Duration, fn func(*State) ([]Event, error)) error {
	unlock, err := s.lock(false, wait)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	events, err := fn(st)
	if err != nil {
		return err
	}
	if err := s.stamp(st, time.Now()); err != nil {
		return err
	}
	if err := writeJSON(s.path("state.json"), st); err != nil {
		return err
	}
	return s.append(events)
}

// Log appends events that change no state, a build's run for one, under
// the state lock. It waits at most logWait for the lock, so that a caller on
// every build's path never queues behind a slow update.
func (s *FileStore) Log(events ...Event) error {
	unlock, err := s.lock(false, logWait)
	if err != nil {
		return err
	}
	defer unlock()
	return s.append(events)
}

const logWait = time.Second

// Record appends events that change no state and must not be lost, a secret
// call's audit entry for one, under the state lock, waiting for it as an
// update does: a busy lock delays the entry, and a lock busy past lockWait
// fails the call instead of dropping it.
func (s *FileStore) Record(events ...Event) error {
	return s.RecordWithin(lockWait, events...)
}

// RecordWithin is Record waiting at most wait for the lock.
func (s *FileStore) RecordWithin(wait time.Duration, events ...Event) error {
	unlock, err := s.lock(false, wait)
	if err != nil {
		return err
	}
	defer unlock()
	return s.append(events)
}

// Exclusive runs fn under the exclusive state lock, writing nothing: what
// must not happen while a process holds the lock, freezing a session's
// processes for one.
func (s *FileStore) Exclusive(fn func() error) error {
	unlock, err := s.lock(false, lockWait)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// VerbStaleWriter is the event of an older release's first refused save
// after a newer one wrote the state.
const VerbStaleWriter = "state.stale-writer"

// stamp records the binary as the state's writer. A newer release wrote it:
// the save is refused (StaleWriterError), logged once per process, since
// this binary would save by rules that release changed. A build without a
// release version (dev, a release candidate, a +dirty branch build) neither
// stamps nor is judged, and a stamp of one is overwritten by the next
// release that saves.
func (s *FileStore) stamp(st *State, now time.Time) error {
	own, ok := release(s.version)
	if !ok {
		return nil
	}
	if st.Writer != nil {
		if newer, ok := release(st.Writer.Version); ok && own.LessThan(newer) {
			return s.refuse(st.Writer.Version, now)
		}
	}
	st.Writer = &Writer{Version: s.version, rest: writerRest(st.Writer)}
	// Releases before the refusal recorded their stale saves here instead.
	delete(st.rest, "staleWriters")
	return nil
}

// refuse is the refused save's error, logged the first time in this process.
func (s *FileStore) refuse(newer string, now time.Time) error {
	err := &StaleWriterError{PID: os.Getpid(), Command: command(os.Args), Version: s.version, Newer: newer}
	s.mu.Lock()
	logged := s.refused
	s.refused = true
	s.mu.Unlock()
	if !logged {
		_ = s.append([]Event{{At: now, By: Party{Name: err.Command}, Verb: VerbStaleWriter, Detail: err.Error()}})
	}
	return err
}

// release is v as a release version: no prerelease, no build metadata.
func release(v string) (*semver.Version, bool) {
	r, err := semver.NewVersion(v)
	if err != nil || r.Prerelease() != "" || r.Metadata() != "" {
		return nil, false
	}
	return r, true
}

func writerRest(w *Writer) rest {
	if w == nil {
		return nil
	}
	return w.rest
}

// command is a process's beekeeper command, its subcommand words only: the
// arguments behind them are a timer's text, a note, a start's whole brief.
// "beekeeper agents start".
func command(args []string) string {
	words := []string{filepath.Base(args[0])}
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "-") || len(words) == 3 {
			break
		}
		words = append(words, a)
	}
	return strings.Join(words, " ")
}

func (s *FileStore) load() (*State, error) {
	st := &State{}
	raw, err := os.ReadFile(s.path("state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, st); err != nil {
		err = fmt.Errorf("%s: %w", s.path("state.json"), err)
		if newer := s.newerWriter(raw); newer != "" {
			return nil, &StaleWriterError{PID: os.Getpid(), Command: command(os.Args), Version: s.version, Newer: newer, Load: err}
		}
		return nil, err
	}
	return st, nil
}

// newerWriter is the release that wrote raw, a state this binary does not
// decode, when it is newer than this one; "" otherwise. A build without a
// release version judges none, as stamp does.
func (s *FileStore) newerWriter(raw []byte) string {
	own, ok := release(s.version)
	if !ok {
		return ""
	}
	var head struct {
		Writer struct {
			Version string `json:"version"`
		} `json:"writer"`
	}
	if json.Unmarshal(raw, &head) != nil {
		return ""
	}
	if newer, ok := release(head.Writer.Version); ok && own.LessThan(newer) {
		return head.Writer.Version
	}
	return ""
}

func (s *FileStore) append(events []Event) error {
	if len(events) == 0 {
		return nil
	}
	f, err := os.OpenFile(s.path("events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := endLine(f); err != nil {
		_ = f.Close()
		return err
	}
	enc := json.NewEncoder(f)
	for _, e := range events {
		e.At = e.At.UTC()
		if err := enc.Encode(e); err != nil {
			_ = f.Close()
			return err
		}
	}
	return f.Close()
}

// endLine writes a newline when the file does not end in one: an unclean
// shutdown can leave an append's range as NUL bytes or a torn line, and the
// next event must not share that line.
func endLine(f *os.File) error {
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return err
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, fi.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	_, err = f.Write([]byte{'\n'})
	return err
}

// Events returns the last n events keep accepts, oldest first (all when
// n <= 0, every event when keep is nil).
func (s *FileStore) Events(n int, keep func(Event) bool) ([]Event, error) {
	f, err := os.Open(s.path("events.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e Event
		// A write lost to an unclean shutdown leaves NUL bytes, and the next
		// append lands right behind them on the same line.
		line := bytes.TrimLeft(sc.Bytes(), "\x00")
		if json.Unmarshal(line, &e) == nil && (keep == nil || keep(e)) {
			out = append(out, e)
		}
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out, sc.Err()
}

// Follow returns every event keep accepts, oldest first. It remembers under
// name how far it read and what it kept, and reads only the lines appended
// since: a watch that follows the log every poll parses each line once. A
// log that was replaced or shrank is read again from its start.
func (s *FileStore) Follow(name string, keep func(Event) bool) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(s.path("events.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if s.follows == nil {
		s.follows = map[string]*followed{}
	}
	at := s.follows[name]
	if at == nil || !os.SameFile(at.file, fi) || fi.Size() < at.offset {
		at = &followed{}
		s.follows[name] = at
	}
	at.file = fi
	if _, err := f.Seek(at.offset, io.SeekStart); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	sc.Split(wholeLines)
	for sc.Scan() {
		at.offset += int64(len(sc.Bytes())) + 1
		var e Event
		line := bytes.TrimLeft(sc.Bytes(), "\x00")
		if json.Unmarshal(line, &e) == nil && (keep == nil || keep(e)) {
			at.kept = append(at.kept, e)
		}
	}
	return slices.Clone(at.kept), sc.Err()
}

// wholeLines splits the log into its complete lines: a line still being
// appended, without its newline yet, is left for the next read.
func wholeLines(data []byte, _ bool) (advance int, token []byte, err error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil
	}
	return 0, nil, nil
}

// ReadFile decodes a JSON side file of the store (the last snapshot);
// found is false when it does not exist yet.
func (s *FileStore) ReadFile(name string, v any) (found bool, err error) {
	raw, err := os.ReadFile(s.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, v)
}

// WriteFile atomically replaces a JSON side file of the store.
func (s *FileStore) WriteFile(name string, v any) error {
	return writeJSON(s.path(name), v)
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
