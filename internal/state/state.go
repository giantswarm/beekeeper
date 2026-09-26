// Package state is beekeeper's shared memory on the machine: the supervisor
// and its relay, grants, holds, registered agents, notes, timers and session records, one
// JSON document read and rewritten under an exclusive file lock, plus an
// append-only event log of every change and of every build run. It outlives any session: a restarted supervisor or its
// successor reads what the previous one knew instead of rebuilding it from
// prose.
package state

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// Party names a session (or a person) in the state.
type Party struct {
	// Session is the Claude Code session id (CLAUDE_CODE_SESSION_ID).
	Session string `json:"session,omitempty"`
	// HostSession is the desktop app's id for it (local_…); it survives a
	// CLI restart that changes Session.
	HostSession string `json:"hostSession,omitempty"`
	Name        string `json:"name"`
}

// Is reports whether p and o name the same session or person.
func (p Party) Is(o Party) bool {
	switch {
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
}

// Of reports whether the record is about sup's current term.
func (c *CLI) Of(sup *Supervisor) bool {
	return c != nil && sup != nil && c.Supervisor.Is(sup.Party) && c.Since.Equal(sup.Since)
}

// RelayDue is the watch's memory that it reported the relay due to the
// supervisor's term Supervisor and Since, once: a relay cancelled or
// expired removes it, so the next quiet moment reports it again.
type RelayDue struct {
	Supervisor Party     `json:"supervisor"`
	Since      time.Time `json:"since"`
	Reported   time.Time `json:"reported"`
	// Context is the supervisor's context in tokens when it was reported.
	Context int64 `json:"contextTokens"`
}

// Of reports whether the record is about sup's current term.
func (r *RelayDue) Of(sup *Supervisor) bool {
	return r != nil && sup != nil && r.Supervisor.Is(sup.Party) && r.Since.Equal(sup.Since)
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
}

// SupervisorRole is the supervisor's record.
func (st *State) SupervisorRole() Role {
	return Role{Holder: st.Supervisor, Relay: st.Relay, Relieved: st.Relieved, CLI: st.SupervisorCLI, RelayDue: st.RelayDue}
}

// SetSupervisorRole stores the supervisor's record.
func (st *State) SetSupervisorRole(r Role) {
	st.Supervisor, st.Relay, st.Relieved, st.SupervisorCLI, st.RelayDue = r.Holder, r.Relay, r.Relieved, r.CLI, r.RelayDue
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
	if r.Holder == nil && r.Relay == nil && len(r.Relieved) == 0 && len(r.Fed) == 0 {
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
	// Tool is the binary whose release window this hold is, ToolFrom the
	// version it reported when the window opened and ToolRelease the release
	// the merge produced. The window closes once the tool reports another
	// version than ToolFrom and no merge of its repository runs.
	Tool        string `json:"tool,omitempty"`
	ToolFrom    string `json:"toolFrom,omitempty"`
	ToolRelease string `json:"toolRelease,omitempty"`
	// UpgradeTo is the target release of the upgrade an automatic upgrade
	// hold stands for.
	UpgradeTo string `json:"upgradeTo,omitempty"`
	// LiftedBy and LiftedAt record the lift of an upgrade hold: the watch
	// keeps a lifted hold, inactive, for as long as its upgrade runs.
	LiftedBy *Party    `json:"liftedBy,omitempty"`
	LiftedAt time.Time `json:"liftedAt,omitzero"`
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

// Agent is an empty session registered as spare capacity.
type Agent struct {
	Party
	Registered time.Time `json:"registered"`
	Task       string    `json:"task,omitempty"`
	AssignedAt time.Time `json:"assignedAt,omitzero"`
	IdleSince  time.Time `json:"idleSince,omitzero"`
	LastTask   string    `json:"lastTask,omitempty"`
}

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
}

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
}

// ModeBypass is Claude Code's bypassPermissions mode.
const ModeBypass = "bypassPermissions"

// Start is a session `beekeeper agents start` started: the id beekeeper
// chose and the permission mode it passed, recorded before the session
// existed. A session cannot add itself: its id is only ever written here by
// the start that created it.
type Start struct {
	Party
	Mode string    `json:"mode"`
	Dir  string    `json:"dir"`
	By   Party     `json:"by"`
	At   time.Time `json:"at"`
}

// BypassStart returns the start of session when beekeeper started it in
// bypassPermissions.
func (st *State) BypassStart(session string) (Start, bool) {
	for _, s := range st.Starts {
		if session != "" && s.Session == session && s.Mode == ModeBypass {
			return s, true
		}
	}
	return Start{}, false
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
	// Spare is the session the supervisor keeps ready to take over: the
	// standby watch keeps it awake and, after a crash, hands it the role.
	Spare *Party `json:"spare,omitempty"`
	// RelayDue is the relay due the watch reported to the supervisor.
	RelayDue *RelayDue `json:"relayDue,omitempty"`
	// Guide is the guide's role, the supervisor's counterpart for the
	// person's decisions.
	Guide *Role `json:"guide,omitempty"`
	// Grants are queued per resource in the order given.
	Grants []Grant `json:"grants,omitempty"`
	// Released is when each resource was last released; a grant's TTL runs
	// from the later of it and the grant.
	Released  map[string]time.Time `json:"released,omitempty"`
	Holds     []Hold               `json:"holds,omitempty"`
	Agents    []Agent              `json:"agents,omitempty"`
	Notes     []Note               `json:"notes,omitempty"`
	NextNote  int                  `json:"nextNote,omitempty"`
	Timers    []Timer              `json:"timers,omitempty"`
	NextTimer int                  `json:"nextTimer,omitempty"`
	// Records say which session serves which issue.
	Records []Record `json:"records,omitempty"`
	// Starts are the sessions beekeeper started, what the permission hook
	// answers for.
	Starts []Start `json:"starts,omitempty"`
	// BudgetETag makes the budget probe a conditional request (a 304
	// costs no budget).
	BudgetETag string `json:"budgetETag,omitempty"`
	// Budget is the last reading of the GitHub core budget.
	Budget *Budget `json:"budget,omitempty"`
	// Merges are the wrapped devctl pr merge runs, per lane: waiting in join
	// order, running, and settling until the lane's installation rolled them.
	Merges []Merge `json:"merges,omitempty"`

	// unknown are the fields a newer beekeeper wrote: an older binary still
	// running (a watch, a gated merge) writes them back unchanged instead of
	// dropping the newer one's state.
	unknown map[string]json.RawMessage
}

// plainState is State without its JSON methods.
type plainState State

// knownKeys are the JSON names of State's fields.
var knownKeys = func() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeFor[plainState]()
	for i := range t.NumField() {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}()

// UnmarshalJSON decodes the state and keeps the fields it does not know.
func (s *State) UnmarshalJSON(raw []byte) error {
	if err := json.Unmarshal(raw, (*plainState)(s)); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return err
	}
	for k := range all {
		if knownKeys[k] {
			delete(all, k)
		}
	}
	s.unknown = nil
	if len(all) > 0 {
		s.unknown = all
	}
	return nil
}

// MarshalJSON encodes the state with the fields it did not know.
func (s State) MarshalJSON() ([]byte, error) {
	raw, err := json.Marshal(plainState(s))
	if err != nil || len(s.unknown) == 0 {
		return raw, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, err
	}
	for k, v := range s.unknown {
		all[k] = v
	}
	return json.Marshal(all)
}

// Budget is one reading of the GitHub core budget.
type Budget struct {
	Remaining int       `json:"remaining"`
	Limit     int       `json:"limit"`
	Reset     time.Time `json:"reset"`
	At        time.Time `json:"at"`
}

// The phases of a Merge.
const (
	Waiting  = "waiting"
	Running  = "running"
	Settling = "settling"
)

// Merge is one devctl pr merge the gate holds in its lane's queue.
type Merge struct {
	Repo  string `json:"repo"`
	PR    int    `json:"pr"`
	Lane  string `json:"lane"`
	By    Party  `json:"by"`
	PID   int    `json:"pid"`
	Phase string `json:"phase"`
	// Joined orders the queue; a rerun within the queue TTL keeps it.
	Joined  time.Time `json:"joined"`
	Seen    time.Time `json:"seen"`
	Started time.Time `json:"started,omitzero"`
	// Finished and Exit are when and how the merge's run ended; on a
	// waiting merge, the failed attempt whose place it keeps (Retrying).
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
	// Release is the tag the merge released, empty when unknown.
	Release string `json:"release,omitempty"`
	// Roll names the HelmReleases (namespace/name) that must reach Release
	// before the lane frees.
	Roll []string `json:"roll,omitempty"`
}

// Key is the merge's repository and number, owner/repo#n.
func (m Merge) Key() string { return fmt.Sprintf("%s#%d", m.Repo, m.PR) }

// Retrying says whether a waiting merge is a failed attempt that keeps its
// place for its session's retry of the same pull request.
func (m Merge) Retrying() bool { return m.Phase == Waiting && !m.Finished.IsZero() }

// Event is one line of events.jsonl.
type Event struct {
	At     time.Time `json:"at"`
	By     Party     `json:"by"`
	Verb   string    `json:"verb"`
	Detail string    `json:"detail"`
}

// Store is the state directory.
type Store struct{ dir string }

// Open returns the store in dir, creating the directory.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Dir is the store's directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) path(name string) string { return filepath.Join(s.dir, name) }

// Read returns the current state under a shared lock.
func (s *Store) Read() (*State, error) {
	l := flock.New(s.path("state.lock"))
	if err := l.RLock(); err != nil {
		return nil, err
	}
	defer func() { _ = l.Unlock() }()
	return s.load()
}

// Peek returns the current state without taking the lock, for a caller
// that must never wait on it (a permission hook): a write replaces the file
// in one rename, so the document read is always a whole one.
func (s *Store) Peek() (*State, error) { return s.load() }

// Update runs fn on the state under the exclusive lock and writes the result
// back atomically together with the events fn returns. When fn fails nothing
// is written.
func (s *Store) Update(fn func(*State) ([]Event, error)) error {
	l := flock.New(s.path("state.lock"))
	if err := l.Lock(); err != nil {
		return err
	}
	defer func() { _ = l.Unlock() }()
	st, err := s.load()
	if err != nil {
		return err
	}
	events, err := fn(st)
	if err != nil {
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
func (s *Store) Log(events ...Event) error {
	ctx, cancel := context.WithTimeout(context.Background(), logWait)
	defer cancel()
	l := flock.New(s.path("state.lock"))
	ok, err := l.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("the state lock is busy")
	}
	defer func() { _ = l.Unlock() }()
	return s.append(events)
}

const logWait = time.Second

func (s *Store) load() (*State, error) {
	st := &State{}
	raw, err := os.ReadFile(s.path("state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path("state.json"), err)
	}
	return st, nil
}

func (s *Store) append(events []Event) error {
	if len(events) == 0 {
		return nil
	}
	f, err := os.OpenFile(s.path("events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			_ = f.Close()
			return err
		}
	}
	return f.Close()
}

// Events returns the last n events keep accepts, oldest first (all when
// n <= 0, every event when keep is nil).
func (s *Store) Events(n int, keep func(Event) bool) ([]Event, error) {
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
		if json.Unmarshal(sc.Bytes(), &e) == nil && (keep == nil || keep(e)) {
			out = append(out, e)
		}
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out, sc.Err()
}

// ReadFile decodes a JSON side file of the store (the last snapshot);
// found is false when it does not exist yet.
func (s *Store) ReadFile(name string, v any) (found bool, err error) {
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
func (s *Store) WriteFile(name string, v any) error {
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
