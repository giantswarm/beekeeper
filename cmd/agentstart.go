package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	// startsKept is how long the record keeps a start: the permission
	// hook stops answering for a session started longer ago.
	startsKept = 30 * 24 * time.Hour
	// replyWait bounds the wait for a started session's first reply, whose
	// model the import reads.
	replyWait = 5 * time.Minute
	// replyQuiet is how long the transcript stays unchanged before the
	// import, which reads it twice and takes no model when it changed
	// in between.
	replyQuiet = 2 * time.Second
	// maxBrief keeps the brief within one command-line argument.
	maxBrief = 100 << 10
	// focusWait bounds the wait for the import to show its session, after
	// which the desktop is switched back to the session it showed.
	focusWait = 15 * time.Second
	// twinWait bounds the wait for the CLI the desktop warms for an import.
	twinWait = 15 * time.Second
)

func (a *app) agentStartCmd() *cobra.Command {
	var model, dir, task string
	c := &cobra.Command{
		Use:   "start <name> <brief file>",
		Short: "Start an agent session in bypass from the command line and import it into the desktop",
		Long: `start starts a Claude Code session without a click: "claude -p" in
bypassPermissions under a session id beekeeper chooses, with the brief file
as its first prompt, in a transient user unit (beekeeper-agent-<id>, its
output in journalctl --user -u <unit>) that the caller's session, scope or
terminal do not take down. Before the session exists, beekeeper records its
id and mode as one of its starts and registers it on the roster under
<name>, busy with --task, by default the brief's first line (or with the
open task of a stopped session's entry under that name, which it takes
over), so the roster shows it at work from its start. Once the
transcript holds the first reply it imports the session into Claude Desktop
(claude://resume?session=<id>): it shows in the sidebar as local_<id> and
takes messages there. The import switches the desktop's main window to the
new session; beekeeper switches it back to the session it showed before
(claude://code/continue), so the person working there stays on it.

Its first turn runs the brief from the command line in bypass. The desktop
runs every later turn in acceptEdits (its import always drops bypass), so
requests no allow rule covers would stop at a card: beekeeper hook
permissionrequest answers them, for beekeeper's starts only. While the first
turn runs, beekeeper stops the CLI the desktop warms for the import, so the
first turn is the session's only CLI and a message by name reaches it; the
desktop starts a new CLI when the person opens the session.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			if name == "" {
				return usageErr("an agent needs a name")
			}
			brief, err := readBrief(args[1])
			if err != nil {
				return err
			}
			if task = strings.TrimSpace(task); task == "" {
				task = briefTask(brief)
			}
			sa, err := a.startAgent(cmd.Context(), agentStart{name: name, brief: brief, task: task, dir: dir, model: model})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "started %s: session %s, desktop local_%s, bypassPermissions, in %s, busy with %q\n"+
				"its first turn runs from the command line (journalctl --user -u %s); later turns are desktop turns in acceptEdits\n",
				name, sa.id, sa.id, sa.dir, sa.task, sa.unit)
			if err == nil && sa.kept != "" {
				_, err = fmt.Fprintf(a.out, "the desktop still shows %s\n", sa.kept)
			}
			if err == nil {
				_, err = fmt.Fprintln(a.out, modelLine(sa.model))
			}
			if err == nil {
				_, err = fmt.Fprintln(a.out, twinLine(sa.twin))
			}
			return err
		},
	}
	c.Flags().StringVar(&model, "model", "", "the session's model (default: Claude Code's)")
	c.Flags().StringVar(&dir, "dir", ".", "the session's working directory")
	c.Flags().StringVar(&task, "task", "", "the task the roster shows it busy with (default: the brief's first line)")
	return c
}

// agentStart is a session `agents start` or `agents handover` starts.
type agentStart struct {
	name, brief, dir, model string
	// task is the roster's task unless the entry taken over holds one.
	task string
	// replaces is the running session a hand-over ends: its roster entry,
	// task and session record go to the new session.
	replaces *state.Party
}

// startedAgent is what startAgent started.
type startedAgent struct {
	id, unit, dir, task string
	// kept is the session the desktop showed before the import and shows
	// again after it; empty when there was none to go back to.
	kept string
	// model is the model the desktop recorded for the session's later
	// turns; empty: none, they run on the desktop's default.
	model string
	// twin is the desktop's CLI of the session the start stopped while the
	// first turn runs; 0: none.
	twin int
}

// startAgent records and registers the session, starts its first turn in a
// transient user unit and imports it into the desktop once the transcript
// holds its first reply, which carries its model.
func (a *app) startAgent(ctx context.Context, sp agentStart) (startedAgent, error) {
	dir, err := filepath.Abs(sp.dir)
	if err != nil {
		return startedAgent{}, err
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return startedAgent{}, usageErr("--dir %s: not a directory", dir)
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		return startedAgent{}, err
	}
	by, err := a.caller()
	if err != nil {
		return startedAgent{}, err
	}
	sessions, _, err := a.sessions()
	if err != nil {
		return startedAgent{}, err
	}
	live := func(p state.Party) bool {
		if sp.replaces != nil && p.Is(*sp.replaces) {
			return false
		}
		_, ok := claude.Live(sessions, p)
		return ok
	}
	id := uuid.NewString()
	s := state.Start{Party: state.Party{Session: id, HostSession: "local_" + id, Name: sp.name}, Mode: state.ModeBypass, Dir: dir, By: by, At: a.now.UTC()}
	var reg registration
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		var err error
		reg, err = recordStart(st, s, sp.task, live)
		if err != nil {
			return nil, err
		}
		if sp.replaces != nil {
			moveRecord(st, *sp.replaces, s.Party)
		}
		return []state.Event{event(by, "agents.start", "%s: session %s in %s, bypassPermissions, busy with %q", sp.name, id, dir, reg.task)}, nil
	})
	if err != nil {
		return startedAgent{}, err
	}
	unit := "beekeeper-agent-" + id[:8]
	self, err := os.Executable()
	if err != nil {
		return startedAgent{}, err
	}
	if err := launch(unit, dir, a.explicitConfig(), []string{self, "agents", "reopen", id}, agentArgv(bin, id, sp.name, sp.model, sp.brief)); err != nil {
		return startedAgent{}, fmt.Errorf("starting %s: %w (the start stays recorded; beekeeper agents remove %q takes it off the roster)", sp.name, err, sp.name)
	}
	if err := awaitReply(ctx, a.cfg.Claude.ProjectsDir, id, func() bool { return unitEnded(ctx, unit) }, replyQuiet, replyWait); err != nil {
		return startedAgent{}, fmt.Errorf("%w, not imported into the desktop: journalctl --user -u %s", err, unit)
	}
	var follow string
	if sp.replaces != nil {
		follow = sp.replaces.HostSession
	}
	kept, err := a.importSession(ctx, id, follow)
	if err != nil {
		return startedAgent{}, err
	}
	sa := startedAgent{id: id, unit: unit, dir: dir, task: reg.task, kept: kept, model: a.desktopModel(ctx, "local_"+id)}
	sa.twin, err = endDesktopTwin(ctx, id)
	return sa, err
}

// endDesktopTwin stops the CLI the desktop warms for an imported session
// while its first turn still runs: two CLIs on one session id are two peers
// under its name, and a message by name could reach the desktop's copy,
// which would run a turn of its own beside the first turn. It waits up to
// twinWait for the desktop's CLI and returns its PID, 0 when none came or
// the first turn's process ended first: from then on the desktop's CLI is
// the session's one (agents reopen warms it once the first turn ended).
func endDesktopTwin(ctx context.Context, id string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, twinWait)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		t, err := proc.Read()
		if err != nil {
			return 0, err
		}
		if !firstTurnRuns(t, id) {
			return 0, nil
		}
		if p := desktopTwin(t, id); p != nil {
			pr, err := os.FindProcess(p.PID)
			if err == nil {
				err = pr.Signal(syscall.SIGTERM)
			}
			if err != nil {
				return 0, fmt.Errorf("stopping the desktop's CLI %d of session %s: %w", p.PID, id, err)
			}
			return p.PID, nil
		}
		select {
		case <-ctx.Done():
			return 0, nil
		case <-tick.C:
		}
	}
}

// firstTurnRuns reports whether the first turn of session id runs: a claude
// process started under --session-id <id>.
func firstTurnRuns(t *proc.Table, id string) bool {
	for _, p := range t.ByPID {
		if i := slices.Index(p.Args, "--session-id"); p.Comm == "claude" && i >= 0 && i+1 < len(p.Args) && p.Args[i+1] == id {
			return true
		}
	}
	return false
}

// desktopTwin is the CLI that resumes session id (the desktop's: the first
// turn runs under --session-id), nil when none runs.
func desktopTwin(t *proc.Table, id string) *proc.Process {
	for _, p := range t.ByPID {
		if p.Comm == "claude" && resumes(p.Args, id) {
			return p
		}
	}
	return nil
}

// resumes reports whether args resume session id: --resume=<id> or
// --resume <id>.
func resumes(args []string, id string) bool {
	for i, a := range args {
		if a == "--resume="+id || a == "--resume" && i+1 < len(args) && args[i+1] == id {
			return true
		}
	}
	return false
}

// desktopModel is the model of host's desktop record, waiting up to
// focusWait for the desktop to write it.
func (a *app) desktopModel(ctx context.Context, host string) string {
	ctx, cancel := context.WithTimeout(ctx, focusWait)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if r, ok := claude.ReadRecord(a.cfg, host); ok {
			return r.Model
		}
		select {
		case <-ctx.Done():
			return ""
		case <-tick.C:
		}
	}
}

// twinLine says whether the first turn is the session's only CLI.
func twinLine(twin int) string {
	if twin == 0 {
		return "the desktop warmed no CLI of its own while the first turn ran: messages by name reach the session's one CLI"
	}
	return fmt.Sprintf("stopped the desktop's CLI %d of the session: the first turn is its only CLI and the only peer under its name, until someone opens it in the desktop", twin)
}

// modelLine says which model the session's desktop turns run on.
func modelLine(model string) string {
	if model == "" {
		return "the desktop recorded no model: its desktop turns run on the desktop's default model"
	}
	return "its desktop turns run on " + model
}

// importSession imports the session into the desktop. The import switches
// the main window to it: once it has, the window goes back to the session
// it showed before, which importSession returns. A desktop that showed no
// session or follow (the session a hand-over ends), or was not running, is
// left on the import.
func (a *app) importSession(ctx context.Context, id, follow string) (string, error) {
	t, err := proc.Read()
	if err != nil {
		return "", err
	}
	running := !desktopStart(t).IsZero()
	prev, err := a.showBriefly(ctx, resumeURL(id), "local_"+id, follow, running)
	if err != nil {
		return "", fmt.Errorf("importing %s into the desktop: %w", id, err)
	}
	return prev, nil
}

// showBriefly opens url, which shows host in the desktop's main window, and
// once it does, shows the session the window showed before again and
// returns it; empty when there was none to go back to, or it was host or
// follow.
func (a *app) showBriefly(ctx context.Context, url, host, follow string, running bool) (string, error) {
	var prev string
	if running {
		prev, _ = claude.DesktopFocus(a.cfg.Claude.DesktopLog) // unreadable: nothing to go back to
	}
	if err := openDesktop(ctx, url, running); err != nil {
		return "", err
	}
	if prev == "" || prev == host || prev == follow || !awaitFocus(ctx, a.cfg.Claude.DesktopLog, host, focusWait) {
		return "", nil
	}
	if err := openDesktop(ctx, continueURL(prev), true); err != nil {
		return "", fmt.Errorf("showing %s again: %w", prev, err)
	}
	return prev, nil
}

// agentReopenCmd is the unit's ExecStopPost: once a start's first turn has
// ended, it shows the session in the desktop for a moment, which warms the
// desktop's CLI of it (endDesktopTwin stopped the one the import warmed), so
// the session is a peer again and takes follow-ups by message. Only a
// session still on the roster is reopened: a hand-over or agents remove
// took the others off, and a handed-over session must not come back.
func (a *app) agentReopenCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "reopen <session id>",
		Short:  "Warm the desktop's CLI of a started session once its first turn ended",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			if !reopens(st, id) {
				_, err := fmt.Fprintf(a.out, "reopen: %s is no start on the roster, left closed\n", id)
				return err
			}
			t, err := proc.Read()
			if err != nil {
				return err
			}
			if desktopStart(t).IsZero() {
				_, err := fmt.Fprintf(a.out, "reopen: the desktop does not run, %s waits for it\n", id)
				return err
			}
			if _, err := a.showBriefly(cmd.Context(), continueURL("local_"+id), "local_"+id, "", true); err != nil {
				return fmt.Errorf("reopening %s in the desktop: %w", id, err)
			}
			_, err = fmt.Fprintf(a.out, "reopen: showed local_%s in the desktop, which warms its CLI\n", id)
			return err
		},
	}
}

// reopens reports whether the session id is one of beekeeper's starts that a
// roster entry still holds.
func reopens(st *state.State, id string) bool {
	p := state.Party{Session: id}
	return slices.ContainsFunc(st.Starts, func(s state.Start) bool { return s.Session == id }) &&
		slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(p) })
}

// awaitFocus reports whether the desktop's main window shows host within
// wait.
func awaitFocus(ctx context.Context, log, host string, wait time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if f, _ := claude.DesktopFocus(log); f == host {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
}

// moveRecord gives from's session record to to.
func moveRecord(st *state.State, from, to state.Party) {
	for i := range st.Records {
		if st.Records[i].Session.Is(from) {
			st.Records[i].Session = to
		}
	}
}

// explicitConfig is the configuration file the caller named (--config or
// $BEEKEEPER_CONFIG), absolute; empty for the default one.
func (a *app) explicitConfig() string {
	p := a.cfgPath
	if p == "" {
		p = os.Getenv("BEEKEEPER_CONFIG")
	}
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func resumeURL(id string) string { return "claude://resume?session=" + id }

// recordStart records s as one of beekeeper's starts, forgetting those older
// than startsKept, and registers it on the roster under its name, busy with
// task unless it takes over a stopped entry's open task.
func recordStart(st *state.State, s state.Start, task string, live func(state.Party) bool) (registration, error) {
	reg, err := registerAgent(st, s.Party, live, s.At)
	if err != nil {
		return registration{}, err
	}
	if reg.task == "" && task != "" {
		reg.task, reg.assignedAt = task, s.At
		ag := &st.Agents[len(st.Agents)-1]
		ag.Task, ag.AssignedAt = task, s.At
	}
	st.Starts = slices.DeleteFunc(st.Starts, func(x state.Start) bool { return s.At.Sub(x.At) > startsKept })
	st.Starts = append(st.Starts, s)
	return reg, nil
}

// readBrief reads the brief file: one command-line argument, not empty.
func readBrief(path string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the caller's brief file
	if err != nil {
		return "", err
	}
	brief := strings.TrimSpace(string(raw))
	switch {
	case brief == "":
		return "", usageErr("%s is empty", path)
	case len(brief) > maxBrief:
		return "", usageErr("%s has %d bytes, more than %d: point the brief at a file instead", path, len(brief), maxBrief)
	}
	return brief, nil
}

// briefTask is the roster's task for a brief: its first line without a
// Markdown heading's hashes.
func briefTask(brief string) string {
	line, _, _ := strings.Cut(brief, "\n")
	return truncate(strings.TrimSpace(strings.TrimLeft(line, "# ")), 80)
}

// agentArgv is the started session's command line: one headless turn in
// bypassPermissions under the id beekeeper recorded.
func agentArgv(bin, id, name, model, brief string) []string {
	argv := []string{bin, "-p", "--session-id", id, "--permission-mode", state.ModeBypass, "-n", name}
	if model != "" {
		argv = append(argv, "--model", model)
	}
	return append(argv, "--", brief)
}

// launch runs argv in a transient user service: it gets the user manager's
// environment, not the caller's session variables, and outlives the caller;
// a configuration file the caller named is passed on as $BEEKEEPER_CONFIG,
// and stopPost runs once argv has ended.
// KillMode=process leaves what the turn started running when it ends, as a
// terminal would.
func launch(unit, dir, config string, stopPost, argv []string) error {
	args := []string{"--user", "--collect", "--quiet", "--unit=" + unit, "-p", "KillMode=process", "--working-directory=" + dir}
	if len(stopPost) > 0 {
		args = append(args, "-p", "ExecStopPost="+strings.Join(stopPost, " "))
	}
	if config != "" {
		args = append(args, "--setenv=BEEKEEPER_CONFIG="+config)
	}
	args = append(append(args, "--"), argv...)
	out, err := exec.Command("systemd-run", args...).CombinedOutput() //nolint:gosec // starting the session is the purpose
	if err != nil {
		return fmt.Errorf("systemd-run: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// awaitReply waits up to wait for the session's transcript to hold its
// first reply and then to stay unchanged for quiet, and fails early once
// ended reports the unit gone without a reply. Claude Desktop's import takes
// a session's model from the transcript's last reply, and takes none when
// the transcript has none or changes while the import reads it; a session
// imported without a model runs every desktop turn on the desktop's default
// instead of --model. A reply that never goes quiet within wait is imported
// all the same: the start reports the model the desktop recorded.
func awaitReply(ctx context.Context, projectsDir, id string, ended func() bool, quiet, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	var (
		last    os.FileInfo
		since   time.Time
		replied bool
	)
	for {
		if m, _ := filepath.Glob(filepath.Join(projectsDir, "*", id+".jsonl")); len(m) > 0 {
			if fi, err := os.Stat(m[0]); err == nil {
				if last == nil || fi.Size() != last.Size() || !fi.ModTime().Equal(last.ModTime()) {
					last, since = fi, time.Now()
					model, _ := claude.Model(m[0])
					replied = model != ""
				}
				if replied && time.Since(since) >= quiet {
					return nil
				}
			}
		}
		if !replied && ended() {
			return fmt.Errorf("session %s ended before its first reply", id)
		}
		select {
		case <-ctx.Done():
			if replied {
				return nil
			}
			return errors.Join(fmt.Errorf("no reply from session %s after %s", id, wait), ctx.Err())
		case <-tick.C:
		}
	}
}

// unitEnded reports whether the transient unit has ended.
func unitEnded(ctx context.Context, unit string) bool {
	out, _ := exec.CommandContext(ctx, "systemctl", "--user", "show", "-p", "ActiveState", "--value", unit).Output() //nolint:gosec // the unit beekeeper named
	s := strings.TrimSpace(string(out))
	return s == "inactive" || s == "failed"
}
