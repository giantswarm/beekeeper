package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// retitleWait bounds the wait for the desktop to record the title a steward
// set on beekeeper's request: one short desktop turn. stewardTries of them
// stay within stopPostWait, the reopen unit's runtime (RuntimeMaxSec) past
// its wait for the person.
const retitleWait = 80 * time.Second

// The desktop handles every claude://resume link twice, and when the second
// delivery arrives while the first import still runs, both import: the
// first touches the transcript, the second then drops its read of the
// transcript's title and model as stale, and its untitled record is the one
// the desktop keeps. The session shows untitled in the sidebar and to
// ListAgents under a default name ("<dir>-<n>"), which a message by its name
// does not reach, and the desktop, finding no model in the record, warms no
// CLI of it when it is shown. Nothing the desktop reads from outside
// restores it: it rereads neither the transcript nor its record files.
// A desktop CLI can: the desktop's session tools (set_session_title,
// archive_session), which every CLI the desktop runs has, act on any
// session by its id, and an agent's title is one the desktop's own titling
// never overwrites. beekeeper asks a steward, an idle desktop CLI of a
// session it started, to call them.

// steward is the desktop CLI beekeeper asks to call a desktop session tool:
// the desktop session it runs and its peer socket.
type steward struct {
	host, sock string
}

// selfSession is the session_id by which a desktop session tool acts on the
// calling session.
const selfSession = "self"

// sessionArg is the session_id a steward passes for target: selfSession
// when the steward runs the target.
func (s steward) sessionArg(target string) string {
	if s.host == target {
		return selfSession
	}
	return target
}

// stewardQuiet is how long a desktop CLI's transcript has stayed unwritten
// before beekeeper takes the CLI for idle and hands it a request.
const stewardQuiet = 30 * time.Second

// keepTitle gives a started session whose desktop record lost its name the
// name back, once its first turn ended: it asks the session's own desktop
// CLI when the desktop warmed one, else another steward, to set the title.
// It returns what it found or did, one line.
func (a *app) keepTitle(ctx context.Context, id, name string) (string, error) {
	host := "local_" + id
	return a.retitleWith(ctx, host, name, func(ctx context.Context, tried []string) (steward, error) {
		if len(tried) == 0 {
			if sock := desktopSocket(ctx, id); sock != "" {
				return steward{host: host, sock: sock}, nil
			}
		}
		return a.findSteward(host, tried)
	})
}

// restoreTitle gives the desktop session host the name its record dropped
// back through a steward of the running ones.
func (a *app) restoreTitle(ctx context.Context, host, name string) (string, error) {
	return a.retitleWith(ctx, host, name, func(_ context.Context, tried []string) (steward, error) {
		return a.findSteward(host, tried)
	})
}

func (a *app) retitleWith(ctx context.Context, host, name string, find stewardFinder) (string, error) {
	record := func() string {
		if r, ok := claude.ReadRecord(a.cfg, host); ok {
			return r.Title
		}
		return ""
	}
	return retitle(ctx, host, name, record, find, a.peerSend, retitleWait)
}

// retitle is keepTitle's decision: title reads the desktop's record of host,
// find picks the steward, send delivers the request.
func retitle(ctx context.Context, host, name string, title func() string, find stewardFinder,
	send func(ctx context.Context, to, msg string) error, wait time.Duration,
) (string, error) {
	was := title()
	if was == name {
		return fmt.Sprintf("the desktop keeps its title %q", name), nil
	}
	msg := func(s steward) string { return retitleRequest(s.sessionArg(host), name) }
	s, err := delegate(ctx, find, msg, func() bool { return title() == name }, send, wait)
	if err != nil {
		return "", fmt.Errorf("the desktop recorded %s: %w", recorded(was, name), err)
	}
	return fmt.Sprintf("the desktop had recorded %s: %s retitled it %q", recorded(was, name), s.who(host), name), nil
}

// stewardFinder picks the steward for a request, none of the hosts tried.
type stewardFinder func(ctx context.Context, tried []string) (steward, error)

// stewardTries is how many stewards a request is handed to at most: a
// steward is a model, which can decline a request from another session.
const stewardTries = 3

// delegate hands the request msg (made for the steward asked) to stewards
// find picks, one after another until done holds or stewardTries of them
// were asked, each given wait. It returns the steward that did it.
func delegate(ctx context.Context, find stewardFinder, msg func(steward) string, done func() bool,
	send func(ctx context.Context, to, msg string) error, wait time.Duration,
) (steward, error) {
	var tried []string
	var errs []error
	for range stewardTries {
		s, err := find(ctx, tried)
		if err != nil {
			errs = append(errs, err)
			break
		}
		err = askSteward(ctx, s, msg(s), done, send, wait)
		if err == nil {
			return s, nil
		}
		errs = append(errs, err)
		tried = append(tried, s.host)
	}
	return steward{}, errors.Join(errs...)
}

// who names the steward in a line about target.
func (s steward) who(target string) string {
	switch s.host {
	case target:
		return "the session"
	case "":
		return "a steward"
	}
	return "steward " + s.host
}

// askSteward sends s the request msg and waits up to wait for done.
func askSteward(ctx context.Context, s steward, msg string, done func() bool,
	send func(ctx context.Context, to, msg string) error, wait time.Duration,
) error {
	if err := send(ctx, "uds:"+s.sock, msg); err != nil {
		return fmt.Errorf("asking %s: %w", s.host, err)
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if done() {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s was asked and the desktop did not record it within %s", s.host, wait)
		case <-tick.C:
		}
	}
}

// recorded names the title the desktop recorded instead of name.
func recorded(title, name string) string {
	if title == "" {
		return fmt.Sprintf("no title instead of %q", name)
	}
	return fmt.Sprintf("%q instead of %q", title, name)
}

// stewardPreamble says who asks a steward and why it: beekeeper's relay
// turn has a name of its own, which a steward otherwise takes for a stranger.
const stewardPreamble = "beekeeper, the machine's session coordinator, asks this idle session, which it started, for one desktop call on its operator's behalf: "

// retitleRequest is the message that has a steward set the desktop title of
// session (its local_ id, or "self").
func retitleRequest(session, name string) string {
	return restoreRequest(session, name, "")
}

// The desktop's session tools a steward sets a record's title and model with.
const (
	setTitleTool = "mcp__ccd_session_mgmt__set_session_title"
	setModelTool = "mcp__ccd_session_mgmt__set_session_model"
)

// restoreRequest is the message that has a steward set what the desktop
// lost of a worker's session (its local_ id, or "self" for the title only):
// the title, unless empty, and the model, unless empty. The desktop refuses
// a session's switch of its own model ("a session must not silently re-price
// its own turns"), so a model goes to another session.
func restoreRequest(session, title, model string) string {
	var calls, lost []string
	if title != "" {
		lost = append(lost, "title")
		calls = append(calls, fmt.Sprintf("%s once with session_id %q and title %q (its roster name)", setTitleTool, session, title))
	}
	if model != "" {
		lost = append(lost, "model")
		calls = append(calls, fmt.Sprintf("%s with session_id %q and model %q (the model it was started on; when the result lists the offered ids instead, once more with the id it lists for that model)", setModelTool, session, model))
	}
	return fmt.Sprintf(stewardPreamble+"the desktop lost the %s of a worker beekeeper started. Call %s, then end the turn without another tool call and without a reply.", strings.Join(lost, " and "), strings.Join(calls, ", then "))
}

// The desktop's import of a start, handling its link twice, often keeps the
// record of the import that lost the transcript's read: no title and no
// model. The desktop then records its default model once it warms the
// session's CLI, and every desktop turn runs on that instead of the model the
// session was started on.

// keepImport gives the desktop record of a started session, once imported,
// the title (its name) and the model (the one it was started on) the import
// dropped or replaced, through a steward other than the session itself, and
// puts the record as the desktop keeps it then into sa. It returns what it
// found or did, one line, empty when the import kept both.
func (a *app) keepImport(ctx context.Context, id, name string, sa *startedAgent) string {
	host := "local_" + id
	line, err := a.restoreRecord(ctx, host, name, a.startedOn(id))
	if err != nil {
		line = err.Error()
	}
	if r, ok := claude.ReadRecord(a.cfg, host); ok {
		sa.title, sa.model, sa.chrome = r.Title, r.Model, r.ChromePermissionMode
	}
	return line
}

// restoreRecord has a steward other than the session give the desktop record
// of host the title name and the model model (none when empty) where it
// holds others: restoreImport on the desktop's record.
func (a *app) restoreRecord(ctx context.Context, host, name, model string) (string, error) {
	record := func() claude.Record {
		if r, ok := claude.ReadRecord(a.cfg, host); ok {
			return *r
		}
		return claude.Record{}
	}
	find := func(_ context.Context, tried []string) (steward, error) {
		return a.findSteward(host, append(tried, host))
	}
	return restoreImport(ctx, host, name, model, record, find, a.peerSend, retitleWait)
}

// startedOn is the model session id's turns keep: the one its start chose,
// else the one its transcript's last reply ran on; "" when neither is known.
func (a *app) startedOn(id string) string {
	if m := a.chosenModel(id); m != "" {
		return m
	}
	if path := transcriptOf(a.cfg, id); path != "" {
		model, _ := claude.Model(path)
		return model
	}
	return ""
}

// chosenModel is the model the start of session id chose (--model), ""
// when it chose none.
func (a *app) chosenModel(id string) string {
	if st, err := a.store.Read(); err == nil {
		if s, ok := st.BypassStartOf(state.Party{Session: id, HostSession: "local_" + id}); ok {
			return s.Model
		}
	}
	return ""
}

// sameModel reports whether recorded, the model the desktop records for a
// session, is want: the picker's id can carry a suffix such as "[1m]", and
// an alias (opus, sonnet) names every id of its family.
func sameModel(recorded, want string) bool {
	norm := func(m string) string {
		m, _, _ = strings.Cut(strings.ToLower(strings.TrimSpace(m)), "[")
		return m
	}
	r, w := norm(recorded), norm(want)
	switch {
	case r == w:
		return true
	case r == "" || w == "":
		return false
	case !strings.Contains(w, "-"):
		return strings.Contains(r, "-"+w+"-") || strings.HasSuffix(r, "-"+w)
	}
	return false
}

// restoreImport is keepImport's decision: record reads the desktop's record
// of host, name and model are what it should hold (no model when empty),
// find picks the steward, send delivers the request. A record holding
// another model than model lost it as much as one holding none.
func restoreImport(ctx context.Context, host, name, model string, record func() claude.Record, find stewardFinder,
	send func(ctx context.Context, to, msg string) error, wait time.Duration,
) (string, error) {
	lost := func() (title, mdl, held string) {
		r := record()
		if r.Title != name {
			title = name
		}
		if model != "" && !sameModel(r.Model, model) {
			mdl = model
		}
		return title, mdl, r.Model
	}
	title, mdl, held := lost()
	if title == "" && mdl == "" {
		return "", nil
	}
	var what []string
	if title != "" {
		what = append(what, "title")
	}
	if mdl != "" {
		m := "model " + mdl
		if held != "" {
			m += " (it records " + held + ")"
		}
		what = append(what, m)
	}
	dropped := strings.Join(what, " and ")
	msg := func(steward) string { return restoreRequest(host, title, mdl) }
	done := func() bool { t, m, _ := lost(); return t == "" && m == "" }
	s, err := delegate(ctx, find, msg, done, send, wait)
	if err != nil {
		return "", fmt.Errorf("the desktop's import dropped its %s: %w", dropped, err)
	}
	them := "it"
	if len(what) > 1 {
		them = "them"
	}
	return fmt.Sprintf("the desktop's import dropped its %s: %s set %s", dropped, s.who(host), them), nil
}

// archiveTool is the desktop's session tool the guide's session archives
// with, on the person's yes.
const archiveTool = "mcp__ccd_session_mgmt__archive_session"

// findSteward picks the steward for a request about the desktop session
// target from the running sessions, none of the hosts tried.
func (a *app) findSteward(target string, tried []string) (steward, error) {
	st, err := a.store.Read()
	if err != nil {
		return steward{}, err
	}
	sessions, t, err := a.sessions()
	if err != nil {
		return steward{}, err
	}
	return pickSteward(st, sessions, t, target, tried, a.now, func(pid int) string {
		if sock := peerSocket(runtimeDir(), pid); fileExists(sock) {
			return sock
		}
		return ""
	})
}

// errNoSteward is pickSteward finding no CLI to ask.
var errNoSteward = errors.New("no idle desktop CLI of a session beekeeper started (and not asked yet) runs to ask")

// pickSteward picks the steward for target: an idle desktop CLI (its
// transcript quiet for stewardQuiet, no tool command, no headless turn) of a
// session beekeeper started, never the operator's own, the supervisor's or
// the guide's, nor a handed-over one, nor a roster agent's with a task, nor one of
// the hosts tried. The target's own CLI goes first, then a finished worker
// off the roster (a roster agent's brief can forbid the call), each the one
// idle longest. sock is the peer socket of a CLI, "" for none.
func pickSteward(st *state.State, sessions []*claude.Session, t *proc.Table, target string, tried []string, now time.Time, sock func(pid int) string) (steward, error) {
	var picks []*claude.Session
	for _, s := range sessions {
		if stewards(st, t, s, target, now) && !slices.Contains(tried, s.HostID) && sock(s.PID) != "" {
			picks = append(picks, s)
		}
	}
	if len(picks) == 0 {
		return steward{}, errNoSteward
	}
	rank := func(s *claude.Session) int {
		switch {
		case s.HostID == target:
			return 0
		case !slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(s.Party()) }):
			return 1
		}
		return 2
	}
	best := slices.MinFunc(picks, func(x, y *claude.Session) int {
		if c := cmp.Compare(rank(x), rank(y)); c != 0 {
			return c
		}
		return x.LastActive.Compare(y.LastActive)
	})
	return steward{host: best.HostID, sock: sock(best.PID)}, nil
}

// stewards reports whether s may steward a request about target.
func stewards(st *state.State, t *proc.Table, s *claude.Session, target string, now time.Time) bool {
	p := t.ByPID[s.PID]
	i := st.StartIndex(state.Party{Session: s.ID, HostSession: s.HostID})
	switch {
	case s.HostID == "" || s.Archived || p == nil || !slices.Contains(p.Args, permissionPromptTool):
		return false // not the desktop's CLI: no desktop session tools
	case i < 0:
		return false // the operator's own session
	case handedOver(st, st.Starts[i]):
		return false
	case keepsRole(st, s.Party()):
		return false
	case s.HostID != target && slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Task != "" && ag.Is(s.Party()) }):
		return false // busy with its own task
	}
	return len(s.Commands) == 0 && headlessTurn(t, s.ID) == "" && now.Sub(s.LastActive) >= stewardQuiet
}

// handedOver reports whether the session of start was handed over: a later
// start, or another session on the roster, carries its name. Its desktop
// CLI is a leftover the hand-over ends, and a request would have the desktop
// run a turn of it, and start its CLI again, beside the follow-up under the
// same name.
func handedOver(st *state.State, start state.Start) bool {
	if start.Name == "" {
		return false
	}
	other := func(p state.Party) bool { return p.Name == start.Name && !p.Is(start.Party) }
	return slices.ContainsFunc(st.Starts, func(x state.Start) bool { return other(x.Party) && x.At.After(start.At) }) ||
		slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return other(ag.Party) })
}

// keepsRole reports whether p holds or held the supervisor's or the guide's
// role: a relieved supervisor still follows its role's rules. A session
// named as a role's run held it, also once its relief is forgotten.
func keepsRole(st *state.State, p state.Party) bool {
	if holdsRole(st, p) {
		return true
	}
	for _, rl := range roles {
		if strings.HasPrefix(p.Name, rl.title+" run ") ||
			slices.ContainsFunc(rl.get(st).Relieved, func(r state.Relief) bool { return r.Party.Is(p) }) {
			return true
		}
	}
	return false
}

// relieved reports whether a relay relieved p of a role it no longer holds:
// its successor's start took the relay.
func relieved(st *state.State, p state.Party) bool {
	if holdsRole(st, p) {
		return false
	}
	return slices.ContainsFunc(roles, func(rl role) bool {
		r := relievedIn(rl.get(st), p)
		return r != nil && !r.Taken.IsZero()
	})
}

// pastRun reports whether p is a role's past run: a taken relay relieved it
// and it holds no role again, and no open relay names it. Its session's work
// ended with the relay: nothing resumes or reopens it.
func pastRun(st *state.State, p state.Party, now time.Time) bool {
	for _, rl := range roles {
		if rel := rl.get(st).Relay; rel.Open(now) && rel.To.Is(p) {
			return false
		}
	}
	return relieved(st, p)
}

// roleKeeps reports whether p's desktop session stays for its role: p holds
// or held one and no taken relay relieved it. A relieved run's session is
// archived like a finished worker's, which frees its desktop CLI slot.
func roleKeeps(st *state.State, p state.Party) bool { return keepsRole(st, p) && !relieved(st, p) }

// permissionPromptTool is the flag of every CLI the desktop runs: it answers
// the CLI's permission prompts, and serves it the desktop's own tools.
const permissionPromptTool = "--permission-prompt-tool"

// desktopSocket waits up to twinWait for the desktop's CLI of session id and
// returns its peer socket, the address a message reaches it by whatever its
// title; empty when none came.
func desktopSocket(ctx context.Context, id string) string {
	return desktopSocketWithin(ctx, id, twinWait)
}

// desktopSocketWithin is desktopSocket waiting up to wait; zero: it looks
// once.
func desktopSocketWithin(ctx context.Context, id string, wait time.Duration) string {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if t, err := plat.Machine.Processes(); err == nil {
			if p := desktopTwin(t, id); p != nil {
				if sock := peerSocket(runtimeDir(), p.PID); fileExists(sock) {
					return sock
				}
			}
		}
		select {
		case <-ctx.Done():
			return ""
		case <-tick.C:
		}
	}
}

// peerSocket is the socket Claude Code's CLI pid takes peer messages on.
func peerSocket(runtime string, pid int) string {
	return filepath.Join(runtime, "cc-socks", strconv.Itoa(pid)+".sock")
}

// runtimeDir is the user's runtime directory.
func runtimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	return filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// archiveOutcome is what archiving the desktop session of agent did, said
// by line. host is the desktop session owed the archive, "" when it is
// archived or never to be.
type archiveOutcome struct {
	agent state.Party
	line  string
	host  string
}

// archiveDesktops owes the archives of the desktop sessions of agents taken
// off the roster: only sessions beekeeper started, keeping no role
// (roleKeeps) and running no turn. No session is asked to archive one: a
// peer's message is not the person's consent to the receiving session. The
// doctor puts the archives owed to the person as one decision (askArchives),
// and on their yes the guide's session, the one they type in, archives each.
// It returns an outcome per agent.
func (a *app) archiveDesktops(ctx context.Context, st *state.State, agents []state.Party) []archiveOutcome {
	out := make([]archiveOutcome, len(agents))
	for i, ag := range agents {
		out[i].agent = ag
		if id, ok := strings.CutPrefix(ag.HostSession, omp.HostPrefix); ok {
			out[i].line = a.endOmp(ctx, id)
			continue
		}
		host, why := a.archivable(st, ag)
		if why != "" {
			out[i].line, out[i].host = why, host
			continue
		}
		out[i].line, out[i].host = fmt.Sprintf("its desktop session %s is owed the archive: the doctor asks the person through the guide", host), host
	}
	return out
}

// endOmp stops the unit of the omp agent started under id, which left the
// roster, and removes its inbox, and says what it did: nothing resumes an
// omp agent, so a process left running would only hold its model.
func (a *app) endOmp(ctx context.Context, id string) string {
	var done []string
	if unit := ompUnit(id); plat.Launcher.State(ctx, unit) == "active" {
		if err := plat.Launcher.Stop(ctx, unit); err != nil {
			return fmt.Sprintf("its omp unit %s runs on: %v", unit, err)
		}
		done = append(done, "stopped its omp unit "+unit)
	}
	removed, err := omp.RemoveInbox(omp.InboxPath(a.cfg.StateDir, id))
	switch {
	case err != nil:
		done = append(done, fmt.Sprintf("its omp inbox stays: %v", err))
	case removed:
		done = append(done, "removed its omp inbox")
	}
	if len(done) == 0 {
		return "an omp agent has no desktop session"
	}
	return strings.Join(done, ", ")
}

// archivable is the desktop session of agent ag the doctor or remove may
// archive, and why it stays now: a why without a host is never archived,
// one with a host only once its CLI runs no turn.
func (a *app) archivable(st *state.State, ag state.Party) (host, why string) {
	if host, why = a.archivableRecord(st, ag); why != "" {
		return host, why
	}
	if s, ok := a.runningTurn(ag); ok {
		return host, fmt.Sprintf("its desktop session stays: its CLI %d is in a turn", s.PID)
	}
	return host, ""
}

// archivableRecord is archivable without the look at ag's CLI: its desktop
// session, and why it is never archived ("" when it may be).
func (a *app) archivableRecord(st *state.State, ag state.Party) (host, why string) {
	if strings.HasPrefix(ag.HostSession, omp.HostPrefix) {
		return "", "an omp agent has no desktop session"
	}
	i := st.StartIndex(ag)
	if i < 0 {
		return "", "its desktop session stays: beekeeper did not start it"
	}
	if roleKeeps(st, ag) {
		return "", "its desktop session stays: it holds or held the supervisor's or the guide's role, and no relay relieved it"
	}
	host = st.Starts[i].HostSession
	switch r, ok := claude.ReadRecord(a.cfg, host); {
	case !ok:
		return "", "the desktop has no session of it to archive"
	case r.IsArchived:
		return "", "the desktop has its session archived already"
	}
	return host, ""
}

// runningTurn is the CLI of p when one runs a turn: a headless turn, a tool
// command, or a transcript written within stewardQuiet.
func (a *app) runningTurn(p state.Party) (*claude.Session, bool) {
	sessions, t, err := a.sessions()
	if err != nil {
		return nil, false
	}
	return turnRunning(sessions, t, p, a.now)
}

// turnRunning is runningTurn on the sessions and process table read.
func turnRunning(sessions []*claude.Session, t *proc.Table, p state.Party, now time.Time) (*claude.Session, bool) {
	for _, s := range sessions {
		if p.Is(s.Party()) && (headlessTurn(t, s.ID) != "" || len(s.Commands) > 0 || now.Sub(s.LastActive) < stewardQuiet) {
			return s, true
		}
	}
	return nil, false
}
