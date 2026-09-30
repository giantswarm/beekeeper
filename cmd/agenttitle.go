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
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
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
	record := func() string {
		if r, ok := claude.ReadRecord(a.cfg, host); ok {
			return r.Title
		}
		return ""
	}
	find := func(ctx context.Context, tried []string) (steward, error) {
		if len(tried) == 0 {
			if sock := desktopSocket(ctx, id); sock != "" {
				return steward{host: host, sock: sock}, nil
			}
		}
		return a.findSteward(host, tried)
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
	msg := func(session string) string { return retitleRequest(session, name) }
	s, err := delegate(ctx, host, find, msg, func() bool { return title() == name }, send, wait)
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

// delegate hands the request msg (given the session_id the steward passes)
// about target to stewards find picks, one after another until done holds
// or stewardTries of them were asked, each given wait. It returns the
// steward that did it.
func delegate(ctx context.Context, target string, find stewardFinder, msg func(session string) string, done func() bool,
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
		err = askSteward(ctx, s, msg(s.sessionArg(target)), done, send, wait)
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
	if s.host == target {
		return "the session"
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
	return fmt.Sprintf(stewardPreamble+"the desktop lost the title of a worker beekeeper started. Call mcp__ccd_session_mgmt__set_session_title once with session_id %q and title %q (its roster name), then end the turn without another tool call and without a reply.", session, name)
}

// archiveRequest is the message that has a steward archive session (its
// local_ id, or "self").
func archiveRequest(session, name string) string {
	return fmt.Sprintf(stewardPreamble+"the operator ran `beekeeper agents remove %s`: the worker beekeeper started for it is finished and off the roster, and remove archives its desktop session (reversible: the Archived list brings it back). Call mcp__ccd_session_mgmt__archive_session once with session_id %q and reason %q, then end the turn without another tool call and without a reply.", name, session, "beekeeper agents remove "+name)
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
// archiving to the person.
func keepsRole(st *state.State, p state.Party) bool {
	if holdsRole(st, p) {
		return true
	}
	for _, rl := range roles {
		if slices.ContainsFunc(rl.get(st).Relieved, func(r state.Relief) bool { return r.Party.Is(p) }) {
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

// archiveWait bounds the wait for the desktop to record an archive.
const archiveWait = 45 * time.Second

// archiveDesktop archives the desktop session of agent ag, just taken off
// the roster, through a steward: only a session beekeeper started, holding
// no role and running no turn. It returns what it did or why not, one line.
func (a *app) archiveDesktop(ctx context.Context, st *state.State, ag state.Party) string {
	i := slices.IndexFunc(st.Starts, func(x state.Start) bool { return x.Session != "" && x.Session == ag.Session })
	if i < 0 {
		return "its desktop session stays: beekeeper did not start it"
	}
	if keepsRole(st, ag) {
		return "its desktop session stays: it holds or held the supervisor's or the guide's role"
	}
	host := st.Starts[i].HostSession
	archived := func() (bool, bool) {
		r, ok := claude.ReadRecord(a.cfg, host)
		return ok, ok && r.IsArchived
	}
	switch ok, done := archived(); {
	case !ok:
		return "the desktop has no session of it to archive"
	case done:
		return "the desktop has its session archived already"
	}
	if s, ok := a.runningTurn(ag); ok {
		return fmt.Sprintf("its desktop session stays: its CLI %d is in a turn", s.PID)
	}
	find := func(_ context.Context, tried []string) (steward, error) { return a.findSteward(host, tried) }
	msg := func(session string) string { return archiveRequest(session, ag.Name) }
	s, err := delegate(ctx, host, find, msg, func() bool { _, done := archived(); return done }, a.peerSend, archiveWait)
	if err != nil {
		return fmt.Sprintf("its desktop session %s stays: %v", host, err)
	}
	return fmt.Sprintf("archived its desktop session %s (%s archived it)", host, s.who(host))
}

// runningTurn is the CLI of p when one runs a turn: a headless turn, a tool
// command, or a transcript written within stewardQuiet.
func (a *app) runningTurn(p state.Party) (*claude.Session, bool) {
	sessions, t, err := a.sessions()
	if err != nil {
		return nil, false
	}
	for _, s := range sessions {
		if p.Is(s.Party()) && (headlessTurn(t, s.ID) != "" || len(s.Commands) > 0 || a.now.Sub(s.LastActive) < stewardQuiet) {
			return s, true
		}
	}
	return nil, false
}
