package cmd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// Background sessions (agents.mode background): beekeeper starts a session
// as a Claude Code background session with Remote Control, run by the
// claude daemon, instead of importing it into Claude Desktop, and wakes it
// by its messaging, a stopped one by claude --bg --resume.

const (
	bgFlag            = "--bg"
	remoteControlFlag = "--remote-control"
	// bgStopCmd stops a background session, keeping its conversation.
	bgStopCmd = "stop"
	// modeFlag is agents start's --mode.
	modeFlag = "--mode"
)

// startMode is the mode a start runs in: the --mode flag, else
// agents.mode.
func (a *app) startMode(flag string) (string, error) {
	m := cmp.Or(strings.TrimSpace(flag), a.cfg.Agents.Mode, config.ModeDesktop)
	if m != config.ModeDesktop && m != config.ModeBackground {
		return "", usageErr("--mode %q: %s or %s", m, config.ModeDesktop, config.ModeBackground)
	}
	return m, nil
}

// backgroundAgent reports whether the wakes of p go the background route:
// beekeeper started it as a background session, or the desk runs in
// background mode and p, with no desktop session, is one the claude daemon
// ran (claude agents --json --all), never a session the person runs in a
// terminal.
func (a *app) backgroundAgent(ctx context.Context, st *state.State, p state.Party) bool {
	if i := st.StartIndex(p); i >= 0 && st.Starts[i].Background {
		return true
	}
	if a.cfg.Agents.Mode != config.ModeBackground || strings.HasPrefix(p.HostSession, "local_") || p.Session == "" {
		return false
	}
	return daemonRan(ctx, p.Session)
}

// daemonRan reports whether the claude daemon lists session id as a
// background session, running or stopped.
func daemonRan(ctx context.Context, id string) bool {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return false
	}
	rows, err := bgList(ctx, bin, cleanEnv(os.Environ(), ""), true)
	return err == nil && slices.ContainsFunc(rows, func(r bgSession) bool { return r.SessionID == id && r.Kind == bgKind })
}

// bgKind is the kind claude agents gives a session the daemon runs.
const bgKind = "background"

// desktopStarted reports whether p's session is one of beekeeper's starts
// that the desktop shows: a background start has no desktop row to miss.
func desktopStarted(st *state.State, p state.Party) bool {
	if !started(st, p) {
		return false
	}
	i := slices.IndexFunc(st.Starts, func(s state.Start) bool { return s.Session == p.Session })
	return !st.Starts[i].Background
}

// cleanEnv is environ without the caller's session and harness variables
// (CLAUDE*, CCD*, omp/agy/codex identity and shell/home overrides): a session
// started from inside another one would take its identity, and beekeeper would
// book the new session's commands under the caller. A configuration file the
// caller named goes on as $BEEKEEPER_CONFIG, and a development build stays
// first on PATH, as a transient unit's environment has them (startUnit).
func cleanEnv(environ []string, config string) []string {
	out := slices.DeleteFunc(slices.Clone(environ), isHarnessEnv)
	if config != "" {
		out = append(slices.DeleteFunc(out, func(kv string) bool { return strings.HasPrefix(kv, "BEEKEEPER_CONFIG=") }), "BEEKEEPER_CONFIG="+config)
	}
	if dir, ok := devBuild(); ok {
		out = append(slices.DeleteFunc(out, func(kv string) bool { return strings.HasPrefix(kv, "PATH=") }),
			"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), devBuildEnv+"=1")
	}
	return out
}

func isHarnessEnv(kv string) bool {
	k, v, _ := strings.Cut(kv, "=")
	return strings.HasPrefix(k, "CLAUDE") ||
		strings.HasPrefix(k, "CCD") ||
		k == omp.EnvAgent ||
		k == omp.EnvName ||
		k == omp.EnvCredentials ||
		k == agy.EnvAgent ||
		k == agy.EnvName ||
		k == codex.EnvAgent ||
		k == codex.EnvName ||
		k == "CODEX_HOME" ||
		k == "ZDOTDIR" ||
		(k == "SHELL" && strings.HasSuffix(filepath.ToSlash(v), "/omp/bin/bash"))
}

// bgStartArgv is a background start's command line: a background session
// with Remote Control under name, in bypassPermissions, on model, under the
// profile's flags, with the brief as its first prompt. The claude daemon
// chooses its session id (--bg ignores --session-id).
func bgStartArgv(bin, name, model, brief string, profile ...string) []string {
	argv := []string{bin, bgFlag, remoteControlFlag, name, permissionModeFlag, state.ModeBypass, "-n", name}
	if model != "" {
		argv = append(argv, modelFlag, model)
	}
	argv = append(argv, profile...)
	return append(argv, "--", brief)
}

// bgResumeArgv continues the stopped background session id with msg as its
// next turn. It passes no flag: the session keeps the options it was
// started with (Remote Control, its mode, name and model), where any flag
// would start a copy of it under a new id.
func bgResumeArgv(bin, id, msg string) []string {
	if strings.HasPrefix(msg, "-") {
		msg = " " + msg // a message, never a flag
	}
	return []string{bin, bgFlag, resumeFlag, id, msg}
}

// bgWait bounds a claude --bg call and the daemon's listing of its session.
const bgWait = time.Minute

// bgRun runs argv in dir with env and returns its output; tests replace it.
var bgRun = func(ctx context.Context, dir string, env, argv []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, bgWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // claude's own command line
	cmd.Dir, cmd.Env = dir, env
	// the daemon it hands the session to may keep the output open
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// bgSession is one row of claude agents --json.
type bgSession struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	PID       int    `json:"pid"`
}

// bgList lists the running sessions claude knows (claude agents --json),
// with all its stopped background ones too; tests replace it.
var bgList = func(ctx context.Context, bin string, env []string, all bool) ([]bgSession, error) {
	ctx, cancel := context.WithTimeout(ctx, bgWait)
	defer cancel()
	args := []string{agentsName, "--json"}
	if all {
		args = append(args, "--all")
	}
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // claude's own command line
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var rows []bgSession
	return rows, json.Unmarshal(out, &rows)
}

// backgroundedLine is the line claude --bg prints for the session it
// started or continued: "backgrounded · <short id> · <name>".
var backgroundedLine = regexp.MustCompile(`(?m)^backgrounded\W+([0-9a-f]{6,})\b`)

// bgShort is the short id claude --bg printed, "" for none.
func bgShort(out string) string {
	if m := backgroundedLine.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

// bgPoll paces the look for a new background session in the daemon's list;
// tests shorten it.
var bgPoll = 250 * time.Millisecond

// bgSessionID is the full session id of the background session short,
// which the daemon lists once it runs.
func bgSessionID(ctx context.Context, bin string, env []string, short string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, bgWait)
	defer cancel()
	var last error
	for {
		rows, err := bgList(ctx, bin, env, false)
		last = err
		for _, r := range rows {
			if r.SessionID != "" && (r.ID == short || strings.HasPrefix(r.SessionID, short)) {
				return r.SessionID, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("claude agents lists no session %s after %s%s", short, dur(bgWait), errSuffix(last))
		case <-time.After(bgPoll):
		}
	}
}

func errSuffix(err error) string {
	if err == nil {
		return ""
	}
	return ": " + err.Error()
}

// firstLines is out's first lines for an error, trimmed.
func firstLines(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.Join(lines[:min(3, len(lines))], "; ")
}

// launchBackground starts sp as a background session in dir with none of
// the caller's session variables and returns its session id.
func (a *app) launchBackground(ctx context.Context, sp agentStart, dir string, profiled []string) (string, error) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return "", err
	}
	env := cleanEnv(os.Environ(), a.explicitConfig())
	out, err := bgRun(ctx, dir, env, bgStartArgv(bin, sp.name, sp.model, sp.brief, profiled...))
	if err != nil {
		return "", fmt.Errorf("claude --bg: %w (%s)", err, firstLines(out))
	}
	short := bgShort(out)
	if short == "" {
		return "", fmt.Errorf("claude --bg named no session (%s)", firstLines(out))
	}
	return bgSessionID(ctx, bin, env, short)
}

// startBackground starts sp as a background session (agents.mode
// background) and records it: on the roster under its name, busy with its
// task, when it returns. No desktop import, no claude:// link and no
// reopen unit: the session runs in the claude daemon and Remote Control
// shows it.
func (a *app) startBackground(ctx context.Context, sp agentStart, dir string, profiled []string, by state.Party, live func(state.Party) bool) (startedAgent, error) {
	id, err := a.launchBackground(ctx, sp, dir, profiled)
	if err != nil {
		return startedAgent{}, fmt.Errorf("starting %s: %w", sp.name, err)
	}
	s := state.Start{Party: state.Party{Session: id, Name: sp.name}, Mode: state.ModeBypass, Dir: dir, By: by, At: a.now.UTC(), Model: sp.model, Background: true}
	var reg registration
	var handed []string
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		var err error
		if reg, err = recordStart(st, s, sp.task, sp.serves, live); err != nil {
			return nil, err
		}
		evs := []state.Event{event(by, "agents.start", "%s: background session %s in %s, bypassPermissions, busy with %q", sp.name, id, dir, reg.task)}
		if sp.replaces != nil {
			moveRecord(st, *sp.replaces, s.Party)
			if handed, err = handClaims(st, lease.Dir(a.cfg.LeaseDir), *sp.replaces, s.Party); err != nil {
				return nil, err
			}
			if len(handed) > 0 {
				evs = append(evs, event(by, "lease.handover", "%s: %s from session %s to %s", sp.name, strings.Join(handed, ", "), sp.replaces.Session, id))
			}
		}
		return evs, nil
	})
	if err != nil {
		return startedAgent{}, fmt.Errorf("%s runs as background session %s but is not recorded: %w (claude stop %s ends it)", sp.name, id, err, id[:8])
	}
	return startedAgent{id: id, dir: dir, task: reg.task, handed: handed, want: sp.model, background: true,
		turn: "its task runs as its first turn in a background session with Remote Control (claude attach " + id[:8] + ")"}, nil
}

// resumeBackground continues the stopped background session w with msg as
// its next turn (claude --bg --resume), with none of the caller's session
// variables. A running session is sent its messages by name instead:
// claude --bg --resume would start a copy of it beside it.
func (a *app) resumeBackground(ctx context.Context, w wakeTarget, msg string) error {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return err
	}
	out, err := bgRun(ctx, w.dir, cleanEnv(os.Environ(), a.explicitConfig()), bgResumeArgv(bin, w.id, msg))
	if err != nil {
		return fmt.Errorf("claude --bg --resume: %w (%s)", err, firstLines(out))
	}
	switch short := bgShort(out); {
	case short == "":
		return fmt.Errorf("claude --bg --resume named no session (%s)", firstLines(out))
	case !strings.HasPrefix(w.id, short):
		return fmt.Errorf("claude --bg --resume started a copy %s of session %s instead of continuing it (%s): claude stop %s ends the copy", short, w.id, firstLines(out), short)
	}
	return nil
}

// wakeBackground wakes the background session w, whose CLI does not run,
// with msg (wake): claude --bg --resume continues it in the claude daemon.
func (a *app) wakeBackground(ctx context.Context, by state.Party, w wakeTarget, msg string) error {
	if err := a.resumeBackground(ctx, w, msg); err != nil {
		return fmt.Errorf("waking %s: %w", w.name, err)
	}
	_ = a.store.Record(event(by, "agents.wake", "%s: resumed background session %s in %s with the message as its turn", w.name, w.id, w.dir))
	_, err := fmt.Fprintf(a.out, "wake: %s had no running CLI: resumed background session %s in %s with the message as its turn (claude attach %s)\n",
		w.name, w.id, w.dir, w.id[:min(8, len(w.id))])
	return err
}

// stopBackgroundStart stops p's background session when beekeeper started
// it as one (claude stop, which keeps its conversation) and says what it
// did; false for any other session. One that runs a turn, or holds or held
// the supervisor's or the guide's role, stays.
func (a *app) stopBackgroundStart(ctx context.Context, st *state.State, p state.Party) (string, bool) {
	i := st.StartIndex(p)
	if i < 0 || !st.Starts[i].Background {
		return "", false
	}
	id := st.Starts[i].Session
	short := id[:min(8, len(id))]
	if roleKeeps(st, p) {
		return fmt.Sprintf("its background session %s stays: it holds or held the supervisor's or the guide's role, and no relay relieved it", short), true
	}
	if s, ok := a.runningTurn(p); ok {
		return fmt.Sprintf("its background session %s stays: its CLI %d is in a turn", short, s.PID), true
	}
	if err := a.bgStop(ctx, id); err != nil {
		return fmt.Sprintf("its background session %s stays: %v", short, err), true
	}
	return fmt.Sprintf("stopped its background session %s (claude attach %s brings it back)", short, short), true
}

// errNoBackground is a background wake of a session with no transcript or
// directory to resume.
var errNoBackground = errors.New("no background session to resume")

// reviveBackground resumes rl's background holder with msg for the
// standby watch: claude --bg --resume in the directory beekeeper started it
// in, else its transcript's.
func (a *app) reviveBackground(ctx context.Context, holder state.Party, msg string) error {
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	w := wakeTarget{name: holder.Name, id: holder.Session}
	if i := st.StartIndex(holder); i >= 0 {
		w.dir = st.Starts[i].Dir
	}
	if t := transcriptOf(a.cfg, w.id); w.dir == "" && t != "" {
		w.dir, _ = claude.TranscriptCwd(t)
	}
	if w.dir == "" {
		return fmt.Errorf("%s: %w", holder.Name, errNoBackground)
	}
	return a.resumeBackground(ctx, w, msg)
}

// parkStoppedEvent is the event of a parked background session the watch
// stopped.
const parkStoppedEvent = "agents.parkstop"

// parkedResident is the parked agents of st whose background session,
// started by beekeeper, still runs in sessions after its turn ended: a
// parked session waits on what its park names and keeps nothing in memory
// meanwhile, since its wake resumes it from its transcript (claude --bg
// --resume). A session in a turn (a headless turn of it in t, a running
// command, its transcript or a subagent's written within stewardQuiet of
// now), a park already resumable (the watch resumes it), an agent on an
// execution host and the supervisor's or the guide's session stay.
func parkedResident(st *state.State, sessions []*claude.Session, t *proc.Table, now time.Time) []state.Party {
	var out []state.Party
	for _, ag := range st.Agents {
		if ag.Park == nil || !ag.Park.Resumable.IsZero() || ag.Execution != nil || ag.Session == "" {
			continue
		}
		if i := st.StartIndex(ag.Party); i < 0 || !st.Starts[i].Background {
			continue
		}
		if roleKeeps(st, ag.Party) {
			continue
		}
		live, busy := false, false
		for _, s := range sessions {
			if s.ID != ag.Session {
				continue
			}
			live = true
			if headlessTurn(t, s.ID) != "" || len(s.Commands) > 0 || now.Sub(s.LastActive) < stewardQuiet || now.Sub(s.LastSubagent) < stewardQuiet {
				busy = true
			}
		}
		if live && !busy {
			out = append(out, ag.Party)
		}
	}
	return out
}

// bgStop stops the background session id (claude stop, which keeps its
// conversation for claude --bg --resume).
func (a *app) bgStop(ctx context.Context, id string) error {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return err
	}
	// claude stop takes the daemon's short id, not the session's.
	if out, err := bgRun(ctx, "", cleanEnv(os.Environ(), a.explicitConfig()), []string{bin, bgStopCmd, id[:min(8, len(id))]}); err != nil {
		return fmt.Errorf("claude stop: %w (%s)", err, firstLines(out))
	}
	return nil
}

// parkStopDir holds a lock file per background session beekeeper started:
// the watch's stop of a parked session and every wake of it hold it, so a
// wake never lands in a CLI the stop ends. Beside it, the mtime of
// <session>.woke is the last wake's.
const parkStopDir = "parkstop"

// wokeStamp is the file whose mtime is the last wake of session id.
func (a *app) wokeStamp(id string) string {
	return filepath.Join(a.cfg.StateDir, parkStopDir, filepath.Base(id)+".woke")
}

// parkStopLock is session id's lock (parkStopDir).
func (a *app) parkStopLock(id string) (*flock.Flock, error) {
	dir := filepath.Join(a.cfg.StateDir, parkStopDir)
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // the state directory's own subdirectory
		return nil, err
	}
	return flock.New(filepath.Join(dir, filepath.Base(id)+".lock")), nil
}

// bgExitWait bounds the look for a stopped session's CLI to exit, and
// bgExitPoll paces it; tests shorten them.
var (
	bgExitWait = 15 * time.Second
	bgExitPoll = 100 * time.Millisecond
)

// parkWakeWait bounds a wake's wait for a stop of its session in flight:
// claude stop's own bound and its CLI's exit, with a margin.
var parkWakeWait = bgWait + bgExitWait + 15*time.Second

// lockForWake takes p's lock (parkStopLock) for a wake of p, a background
// session beekeeper started, waiting for a stop in flight to end, and
// returns its release, which marks the wake's time; a no-op for any other
// session.
func (a *app) lockForWake(ctx context.Context, st *state.State, p state.Party) (func(), error) {
	if i := st.StartIndex(p); i < 0 || !st.Starts[i].Background || p.Session == "" {
		return func() {}, nil
	}
	l, err := a.parkStopLock(p.Session)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, parkWakeWait)
	defer cancel()
	if ok, err := l.TryLockContext(ctx, 50*time.Millisecond); err != nil || !ok {
		return nil, fmt.Errorf("waking %s: the stop of its parked background session holds %s: %w", p.Name, l.Path(), errors.Join(err, ctx.Err()))
	}
	return func() {
		stamp, now := a.wokeStamp(p.Session), time.Now()
		_ = os.WriteFile(stamp, nil, 0o600) //nolint:gosec // under the state directory, the session id's base name
		_ = os.Chtimes(stamp, now, now)
		_ = l.Unlock()
	}, nil
}

// freshSessions reads the running sessions and the process table again for
// a stop's last look; tests replace it.
var freshSessions = (*app).sessions

// stopParked stops the background sessions of parkedResident, each once
// its turn ended, so a parked session costs the machine no memory; its wake
// (agents wake, agents resume, the watch's resume or a timer's) resumes it
// from its transcript. A stop that fails is a line, tried again next poll.
func (w *watcher) stopParked(ctx context.Context, st *state.State, sessions []*claude.Session) {
	if !slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Park != nil }) {
		return
	}
	t, err := plat.Machine.Processes()
	if err != nil {
		return // no turn is known to have ended
	}
	for _, p := range parkedResident(st, sessions, t, w.now) {
		key := "park-stop-" + p.Session
		short := p.Session[:min(8, len(p.Session))]
		stopped, err := w.stopParkedSession(ctx, st, p)
		if err != nil {
			w.fail(key, "PARKED SESSION STAYS %q: its background session %s keeps its memory: %v", p.Name, short, err)
			continue
		}
		w.clear(key)
		if !stopped {
			continue
		}
		_ = w.store.Record(event(watchParty, parkStoppedEvent, "%s: stopped its background session %s while it is parked; its wake resumes it", p.Name, p.Session))
		w.emitNow("park-stop", "PARKED SESSION STOPPED %q: background session %s frees its memory while parked; its wake resumes it (claude attach %s)", p.Name, short, short)
	}
}

// stopParkedSession stops p's background session under its lock
// (parkStopLock), which every wake of p takes too: a wake in flight keeps
// it running this poll, one that comes meanwhile waits for the stop and
// then resumes the stopped session. Under the lock it looks again: p still
// parked on the park seen in seen, not woken within stewardQuiet, and,
// read anew, still running with its turn ended; false when it stays. The
// lock holds until the stopped CLI exits, so the next wake finds it gone.
func (a *app) stopParkedSession(ctx context.Context, seen *state.State, p state.Party) (bool, error) {
	l, err := a.parkStopLock(p.Session)
	if err != nil {
		return false, err
	}
	if ok, err := l.TryLock(); err != nil || !ok {
		return false, err // a wake holds it: the next poll looks again
	}
	defer func() { _ = l.Unlock() }()
	if fi, err := os.Stat(a.wokeStamp(p.Session)); err == nil && a.now.Sub(fi.ModTime()) < stewardQuiet {
		return false, nil // woken just now: its CLI may not have written the message yet
	}
	st, err := a.store.Read()
	if err != nil {
		return false, err
	}
	if !samePark(seen, st, p) {
		return false, nil
	}
	sessions, t, err := freshSessions(a)
	if err != nil {
		return false, err
	}
	if !slices.ContainsFunc(parkedResident(st, sessions, t, a.now), p.Is) {
		return false, nil
	}
	if err := a.bgStop(ctx, p.Session); err != nil {
		return false, err
	}
	var pids []int
	for _, s := range sessions {
		if s.ID == p.Session && s.PID > 0 {
			pids = append(pids, s.PID)
		}
	}
	return true, waitExit(ctx, pids)
}

// samePark says whether p is parked in st on the same park it was in seen.
func samePark(seen, st *state.State, p state.Party) bool {
	park := func(st *state.State) *state.Park {
		if i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(p) }); i >= 0 {
			return st.Agents[i].Park
		}
		return nil
	}
	was, now := park(seen), park(st)
	return was != nil && now != nil && now.At.Equal(was.At) && now.On == was.On && now.Waits == was.Waits && now.Resumable.IsZero()
}

// waitExit waits up to bgExitWait for the processes pids to exit.
func waitExit(ctx context.Context, pids []int) error {
	ctx, cancel := context.WithTimeout(ctx, bgExitWait)
	defer cancel()
	for {
		t, err := plat.Machine.Processes()
		if err != nil {
			return err
		}
		pids = slices.DeleteFunc(pids, func(pid int) bool { return t.ByPID[pid] == nil })
		if len(pids) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("claude stop returned, but its CLI %v still runs after %s", pids, dur(bgExitWait))
		case <-time.After(bgExitPoll):
		}
	}
}
