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
// stay within the reopen unit's TimeoutStopSec.
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

// restoreRequest is the message that has a steward set what the desktop
// lost of a worker's session (its local_ id, or "self" for the title only):
// the title, unless empty, and the model, unless empty. The desktop refuses
// a session's switch of its own model, so a model goes to another session.
func restoreRequest(session, title, model string) string {
	var calls, lost []string
	if title != "" {
		lost = append(lost, "title")
		calls = append(calls, fmt.Sprintf("mcp__ccd_session_mgmt__set_session_title once with session_id %q and title %q (its roster name)", session, title))
	}
	if model != "" {
		lost = append(lost, "model")
		calls = append(calls, fmt.Sprintf("mcp__ccd_session_mgmt__set_session_model with session_id %q and model %q (the model its first turn ran on; when the result lists the offered ids instead, once more with the id it lists for that model)", session, model))
	}
	return fmt.Sprintf(stewardPreamble+"the desktop lost the %s of a worker beekeeper started. Call %s, then end the turn without another tool call and without a reply.", strings.Join(lost, " and "), strings.Join(calls, ", then "))
}

// The desktop's import of a start, handling its link twice, often keeps the
// record of the import that lost the transcript's read: no title and no
// model, and a session without a model runs every desktop turn on the
// desktop's default instead of the model its first turn ran on.

// keepImport gives the desktop record of a started session, once imported,
// the title (its name) and the model (its first turn's) the import dropped,
// through a steward other than the session itself, and puts the record as
// the desktop keeps it then into sa. It returns what it found or did, one
// line, empty when the import kept both.
func (a *app) keepImport(ctx context.Context, id, name string, sa *startedAgent) string {
	host := "local_" + id
	record := func() claude.Record {
		if r, ok := claude.ReadRecord(a.cfg, host); ok {
			return *r
		}
		return claude.Record{}
	}
	var model string
	if m, _ := filepath.Glob(filepath.Join(a.cfg.Claude.ProjectsDir, "*", id+".jsonl")); len(m) > 0 {
		model, _ = claude.Model(m[0])
	}
	find := func(_ context.Context, tried []string) (steward, error) {
		return a.findSteward(host, append(tried, host))
	}
	line, err := restoreImport(ctx, host, name, model, record, find, a.peerSend, retitleWait)
	if err != nil {
		line = err.Error()
	}
	r := record()
	sa.title, sa.model = r.Title, r.Model
	return line
}

// restoreImport is keepImport's decision: record reads the desktop's record
// of host, name and model are what it should hold, find picks the steward,
// send delivers the request. A record holding a model holds the one the
// steward set: the desktop records the id its picker offers, which can
// differ from the transcript's.
func restoreImport(ctx context.Context, host, name, model string, record func() claude.Record, find stewardFinder,
	send func(ctx context.Context, to, msg string) error, wait time.Duration,
) (string, error) {
	lost := func() (title, mdl string) {
		r := record()
		if r.Title != name {
			title = name
		}
		if r.Model == "" {
			mdl = model
		}
		return title, mdl
	}
	title, mdl := lost()
	if title == "" && mdl == "" {
		return "", nil
	}
	var what []string
	if title != "" {
		what = append(what, "title")
	}
	if mdl != "" {
		what = append(what, "model "+mdl)
	}
	dropped := strings.Join(what, " and ")
	msg := func(steward) string { return restoreRequest(host, title, mdl) }
	done := func() bool { t, m := lost(); return t == "" && m == "" }
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

// archiveRequest is the message that has steward s archive the desktop
// sessions hosts (local_ ids; its own last, as "self"), off the roster by
// the command by.
func archiveRequest(s steward, hosts []string, by string) string {
	var ids []string
	for _, h := range hosts {
		if h != s.host {
			ids = append(ids, strconv.Quote(h))
		}
	}
	if slices.Contains(hosts, s.host) {
		ids = append(ids, strconv.Quote(selfSession))
	}
	return fmt.Sprintf(stewardPreamble+"`%s` took finished workers beekeeper started off the roster, and archives their desktop sessions (reversible: the Archived list brings one back). Call mcp__ccd_session_mgmt__archive_session once for each session_id of %s, in that order, with reason %q, then end the turn without another tool call and without a reply.", by, strings.Join(ids, ", "), by)
}

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

// pickSteward picks the steward for target: an idle desktop CLI (its
// transcript quiet for stewardQuiet, no tool command, no headless turn) of a
// session beekeeper started, never the operator's own, the supervisor's or
// the guide's, nor a roster agent's with a task, nor one of
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
		return steward{}, errors.New("no idle desktop CLI of a session beekeeper started (and not asked yet) runs to ask")
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
	switch {
	case s.HostID == "" || s.Archived || p == nil || !slices.Contains(p.Args, permissionPromptTool):
		return false // not the desktop's CLI: no desktop session tools
	case !slices.ContainsFunc(st.Starts, func(x state.Start) bool { return x.Session == s.ID }):
		return false // the operator's own session
	case keepsRole(st, s.Party()):
		return false
	case s.HostID != target && slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Task != "" && ag.Is(s.Party()) }):
		return false // busy with its own task
	}
	return len(s.Commands) == 0 && headlessTurn(t, s.ID) == "" && now.Sub(s.LastActive) >= stewardQuiet
}

// keepsRole reports whether p holds or held the supervisor's or the guide's
// role: a relieved supervisor still follows its role's rules, which leave
// archiving to the person. A session named as a role's run held it, also
// once its relief is forgotten.
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

// permissionPromptTool is the flag of every CLI the desktop runs: it answers
// the CLI's permission prompts, and serves it the desktop's own tools.
const permissionPromptTool = "--permission-prompt-tool"

// desktopSocket waits up to twinWait for the desktop's CLI of session id and
// returns its peer socket, the address a message reaches it by whatever its
// title; empty when none came.
func desktopSocket(ctx context.Context, id string) string {
	ctx, cancel := context.WithTimeout(ctx, twinWait)
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

// archiveWait bounds the wait for the desktop to record an archive, and
// archiveEach the time a steward's turn takes for each further session.
const (
	archiveWait = 45 * time.Second
	archiveEach = 5 * time.Second
)

// archiveOutcome is what archiving the desktop session of agent did, said
// by line. host is the desktop session left to archive later, "" when it
// is archived or never to be; asked says a steward was asked for it in vain.
type archiveOutcome struct {
	agent state.Party
	line  string
	host  string
	asked bool
}

// archiveDesktops archives the desktop sessions of agents taken off the
// roster by the command by, through one steward's turn: only sessions
// beekeeper started, holding no role and running no turn. It returns an
// outcome per agent.
func (a *app) archiveDesktops(ctx context.Context, st *state.State, agents []state.Party, by string) []archiveOutcome {
	out := make([]archiveOutcome, len(agents))
	var hosts []string
	at := map[string]int{}
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
		at[host] = i
		hosts = append(hosts, host)
	}
	if len(hosts) == 0 {
		return out
	}
	archived := func(host string) bool {
		r, ok := claude.ReadRecord(a.cfg, host)
		return ok && r.IsArchived
	}
	left := func() []string { return slices.DeleteFunc(slices.Clone(hosts), archived) }
	find := func(_ context.Context, tried []string) (steward, error) { return a.findSteward(hosts[0], tried) }
	msg := func(s steward) string { return archiveRequest(s, left(), by) }
	wait := archiveWait + time.Duration(len(hosts)-1)*archiveEach
	s, err := delegate(ctx, find, msg, func() bool { return len(left()) == 0 }, a.peerSend, wait)
	for _, h := range hosts {
		o := &out[at[h]]
		if archived(h) {
			o.line = fmt.Sprintf("archived its desktop session %s (%s archived it)", h, s.who(h))
		} else {
			o.line, o.host, o.asked = fmt.Sprintf("its desktop session %s stays: %v", h, err), h, true
		}
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
	if strings.HasPrefix(ag.HostSession, omp.HostPrefix) {
		return "", "an omp agent has no desktop session"
	}
	i := slices.IndexFunc(st.Starts, func(x state.Start) bool { return x.Session != "" && x.Session == ag.Session })
	if i < 0 {
		return "", "its desktop session stays: beekeeper did not start it"
	}
	if keepsRole(st, ag) {
		return "", "its desktop session stays: it holds or held the supervisor's or the guide's role"
	}
	host = st.Starts[i].HostSession
	switch r, ok := claude.ReadRecord(a.cfg, host); {
	case !ok:
		return "", "the desktop has no session of it to archive"
	case r.IsArchived:
		return "", "the desktop has its session archived already"
	}
	if s, ok := a.runningTurn(ag); ok {
		return host, fmt.Sprintf("its desktop session stays: its CLI %d is in a turn", s.PID)
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
