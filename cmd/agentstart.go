package cmd

import (
	"context"
	"encoding/json"
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
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/project"
	"github.com/giantswarm/beekeeper/plugin"
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
	// stopPostWait bounds the reopen after the first turn: the desktop's
	// CLI, the retitle and model requests and the desktop recording them.
	stopPostWait = 10 * time.Minute
	// importAwayWait bounds how long a start's import waits for the person
	// to leave the desktop's window; past it the reopen after the first
	// turn imports the session.
	importAwayWait = 2 * time.Minute
	// reopenAwayWait bounds how long a reopen waits for the person to leave
	// the desktop's window.
	reopenAwayWait = 25 * time.Minute
	// desktopTurnWait bounds how long a link for an agent that asked for a
	// desktop turn waits for the person's input to pause; the window's focus
	// does not hold it.
	desktopTurnWait = time.Minute
	// settleWait bounds the wait for the desktop's main window to leave the
	// session a link is about to show, a switch back still landing.
	settleWait = 5 * time.Second
	// awayPoll is how often a wait for the desktop's window asks the
	// compositor which window has focus.
	awayPoll = 500 * time.Millisecond
)

// errDesktopInUse is a claude:// link not opened: the desktop's window kept
// the focus, and the link would have switched its main window under the
// person working there.
var errDesktopInUse = errors.New("the desktop's window kept the focus")

// errTyping is a claude:// link not opened: the person kept typing or
// pointing within desktop.typingQuiet, and a stray keystroke would have
// gone into the window the link switches.
var errTyping = errors.New("the person kept typing within desktop.typingQuiet")

// desktopWindowActive reports whether the desktop's window has the focus;
// tests replace it.
var desktopWindowActive = claude.DesktopWindowActive

// screenLocked reports whether a screen locker runs: nobody reads or types
// in the desktop's window, whatever the compositor still names as focused,
// and keystrokes go to the locker.
func screenLocked() bool {
	t, err := plat.Machine.Processes()
	return err == nil && claude.ScreenLocked(t)
}

// desktopInput watches the person's keyboards and pointers; tests replace
// it.
var desktopInput = func(ctx context.Context) (func() time.Time, error) { return plat.Input.Watch(ctx) }

func (a *app) agentStartCmd() *cobra.Command {
	var model, dir, task, harness string
	var desktop bool
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
(claude://resume?session=<id>): it shows in the sidebar as local_<id>,
titled with <name> (beekeeper appends the name's custom-title line to the
transcript first, within the last 256 KiB the import reads, and freezes the
first turn's unit until the desktop recorded the session, so the transcript
does not change under the import), and takes messages there. The desktop
handles the link twice and sometimes keeps an untitled record: once the
first turn has ended, beekeeper has the session set its own title with the
desktop's set_session_title. The import switches the desktop's main window to the
new session; beekeeper switches it back to the session it showed before
(claude://code/continue), so the person working there stays on it. Both
links wait while the desktop's window has the focus (Hyprland's active
window), so the switch never happens under someone reading or typing
there: up to 2 minutes, after which the start leaves the import to the
reopen once the first turn has ended, which waits up to 25 minutes more
(agents and the watch's IMPORT WAITS show it waiting). A locked screen
(a running hyprlock, swaylock, gtklock or waylock) holds no link: nobody
works in the window and keystrokes go to the locker. An agent
that needs a desktop turn, its browser's (the Claude in Chrome tools exist
only in a desktop CLI), is started with --desktop or asks with agents
desktop: its import and reopen do not wait for the window's focus, and wait
for the person's typing to pause for 1 minute at most.

The first prompt is the worker rules beekeeper ships with its role skills
(the worker-rules skill, under the binary's version), then the brief as the
task: a brief carries only its task, and a worker reports to "the
supervisor", which the PreToolUse hook delivers to the role's holder.

Its first turn runs that prompt from the command line in bypass. The desktop
runs every later turn in acceptEdits (its import always drops bypass), so
requests no allow rule covers would stop at a card: beekeeper hook
permissionrequest answers them, for beekeeper's starts only. The browser is
the desktop's own: the import gives the session the Chrome permission mode
skip_all_permission_checks only when the desktop allows all browser actions
(a person's "Allow all sites" on a Claude in Chrome site request turns that
on for every session), and otherwise each navigate to a site the session
was not allowed on yet waits on a site request in its desktop row, which no
hook answers. start says which mode the desktop recorded, and agents shows
it per agent (BROWSER asks or skips). While the first
turn runs, beekeeper stops the CLI the desktop warms for the import, so the
first turn is the session's only CLI and a message by name reaches it; the
desktop starts a new CLI when the person opens the session.

--harness omp starts an omp (oh-my-pi) agent instead: "omp --mode rpc"
in yolo approval mode on --model (default: omp.model; with neither, or a
model omp does not list as provider/id, the start is refused), in
a transient user unit beekeeper-omp-<id>, with its stdin on a FIFO inbox
in beekeeper's state folder and the brief as its first message. It is
registered on the roster as omp_<id> under <name> and shows in sessions
and agents with harness omp; agents wake writes a message to its inbox,
which omp delivers at its next tool round or as its next turn. No desktop
is involved and no import happens.`,
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
			// Every worker gets the shipped rules ahead of its task, whatever
			// its harness.
			sp := agentStart{name: name, brief: workerPrompt(taskPrompt(brief)), task: task, dir: dir, model: model, desktop: desktop}
			switch harness {
			case omp.Harness:
				return a.startOmpAgent(cmd.Context(), sp)
			case "", "claude":
			default:
				return usageErr("--harness %q: claude or omp", harness)
			}
			sa, err := a.startAgent(cmd.Context(), sp)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "started %s: session %s, desktop local_%s, bypassPermissions, in %s, busy with %q\n"+
				"its first turn runs from the command line (journalctl --user -u %s); later turns are desktop turns in acceptEdits\n",
				name, sa.id, sa.id, sa.dir, sa.task, sa.unit)
			if err == nil && sa.deferred != nil {
				_, err = fmt.Fprintf(a.out, "%v: not imported yet, the reopen after its first turn imports it\n", sa.deferred)
				return err
			}
			if err == nil && sa.kept != "" {
				_, err = fmt.Fprintf(a.out, "the desktop still shows %s\n", sa.kept)
			}
			if err == nil && sa.restored != "" {
				_, err = fmt.Fprintln(a.out, sa.restored)
			}
			if err == nil {
				_, err = fmt.Fprintln(a.out, titleLine(name, sa.title))
			}
			if err == nil {
				_, err = fmt.Fprintln(a.out, modelLine(sa.model))
			}
			if err == nil {
				_, err = fmt.Fprintln(a.out, browserLine(sa.chrome))
			}
			if err == nil {
				_, err = fmt.Fprintln(a.out, twinLine(sa.twin))
			}
			return err
		},
	}
	c.Flags().StringVar(&model, "model", "", "the session's model (default: Claude Code's; omp: omp.model)")
	c.Flags().StringVar(&dir, "dir", ".", "the session's working directory")
	c.Flags().StringVar(&task, "task", "", "the task the roster shows it busy with (default: the brief's first line)")
	c.Flags().StringVar(&harness, "harness", "claude", "the agent harness: claude or omp")
	c.Flags().BoolVar(&desktop, "desktop", false, "the task needs desktop turns (the browser): import it past the desktop window's focus, as agents desktop does")
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
	// id is the session id when the caller chose it (a role's successor,
	// whose relay names it before it starts); empty: a new one.
	id string
	// by is who starts it when that is not the calling session (the
	// standby watch); nil: the caller.
	by *state.Party
	// desktop asks for its desktop turn from the start (--desktop).
	desktop bool
}

// startedAgent is what startAgent started.
type startedAgent struct {
	id, unit, dir, task string
	// kept is the session the desktop showed before the import and shows
	// again after it; empty when there was none to go back to.
	kept string
	// title is the title the desktop recorded, the sidebar's; empty: none.
	title string
	// model is the model the desktop recorded for the session's later
	// turns; empty: none, they run on the desktop's default.
	model string
	// chrome is the Chrome permission mode the desktop recorded for the
	// session's later turns; empty: none, its browser actions ask.
	chrome string
	// twin is the desktop's CLI of the session the start stopped while the
	// first turn runs; 0: none.
	twin int
	// restored says how a steward gave the desktop's record the title and
	// model its import dropped; empty: the import kept both.
	restored string
	// deferred is what held the import (the desktop's window kept the
	// focus, the person kept typing): the reopen after the first turn
	// imports the session.
	deferred error
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
	// Watched from the start, the person's input has mostly been quiet
	// for desktop.typingQuiet by the first reply, or not.
	deskCtx, stopDesk := context.WithCancel(ctx)
	defer stopDesk()
	d, err := a.watchDesk(deskCtx)
	if err != nil {
		return startedAgent{}, err
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		return startedAgent{}, err
	}
	var by state.Party
	if sp.by != nil {
		by = *sp.by
	} else if by, err = a.caller(); err != nil {
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
	id := sp.id
	if id == "" {
		id = uuid.NewString()
	}
	s := state.Start{Party: state.Party{Session: id, HostSession: "local_" + id, Name: sp.name}, Mode: state.ModeBypass, Dir: dir, By: by, At: a.now.UTC()}
	var reg registration
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		var err error
		reg, err = recordStart(st, s, sp.task, live)
		if err != nil {
			return nil, err
		}
		if sp.desktop {
			st.Agents[len(st.Agents)-1].DesktopTurn = s.At
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
	if err := launch(unit, dir, a.explicitConfig(), []string{self, agentsName, reopenName, id}, agentArgv(bin, id, sp.name, sp.model, sp.brief)); err != nil {
		return startedAgent{}, fmt.Errorf("starting %s: %w (the start stays recorded; beekeeper agents remove %q takes it off the roster)", sp.name, err, sp.name)
	}
	if err := awaitReply(ctx, a.cfg.Claude.ProjectsDir, id, func() bool { return unitEnded(ctx, unit) }, replyQuiet, replyWait); err != nil {
		return startedAgent{}, fmt.Errorf("%w, not imported into the desktop: journalctl --user -u %s", err, unit)
	}
	var follow string
	if sp.replaces != nil {
		follow = sp.replaces.HostSession
	}
	sa := startedAgent{id: id, unit: unit, dir: dir, task: reg.task}
	d.urgent = a.asksDesktop(id)
	if sa.deferred = d.await(ctx, importAwayWait, nil); sa.deferred != nil {
		return sa, nil
	}
	err = whileFrozen(ctx, unit, func() error {
		if err := titleTranscript(a.cfg.Claude.ProjectsDir, id, sp.name); err != nil {
			return fmt.Errorf("%w, not imported into the desktop", err)
		}
		var err error
		if sa.kept, err = a.importSession(ctx, d, id, follow); err != nil {
			return err
		}
		if r := a.desktopRecord(ctx, "local_"+id); r != nil {
			sa.title, sa.model, sa.chrome = r.Title, r.Model, r.ChromePermissionMode
		}
		return nil
	})
	if err != nil {
		return startedAgent{}, err
	}
	if sa.twin, err = endDesktopTwin(ctx, id); err != nil {
		return sa, err
	}
	sa.restored = a.keepImport(ctx, id, sp.name, &sa)
	return sa, nil
}

// whileFrozen runs fn with the first turn's unit frozen, and thaws it once
// fn returned, whatever fn returned. The desktop's import reads the
// transcript's identity and then its end for the session's title and model,
// and drops that read when the transcript changed in between: a first turn
// that appends a line meanwhile leaves the import untitled, without a model.
// A unit no longer active (its first turn ended, or is ending) has no
// writer and is not frozen.
func whileFrozen(ctx context.Context, unit string, fn func() error) error {
	if err := plat.Launcher.Freeze(ctx, unit); err != nil {
		if plat.Launcher.State(ctx, unit) != "active" {
			return fn()
		}
		return fmt.Errorf("freezing %s for the import: %w", unit, err)
	}
	err := fn()
	if terr := plat.Launcher.Thaw(context.WithoutCancel(ctx), unit); terr != nil {
		err = errors.Join(err, fmt.Errorf("thawing %s: %w (systemctl --user thaw %s resumes its first turn)", unit, terr, unit))
	}
	return err
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
		t, err := plat.Machine.Processes()
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

// The claude CLI's process name and the flag a start names its session
// with.
const (
	claudeComm    = "claude"
	sessionIDFlag = "--session-id"
)

// firstTurnRuns reports whether the first turn of session id runs.
func firstTurnRuns(t *proc.Table, id string) bool {
	for _, p := range t.ByPID {
		if startsSession(p, id) {
			return true
		}
	}
	return false
}

// startsSession reports whether p is a first turn of session id: a claude
// process started under --session-id <id>.
func startsSession(p *proc.Process, id string) bool {
	i := slices.Index(p.Args, sessionIDFlag)
	return p.Comm == claudeComm && i >= 0 && i+1 < len(p.Args) && p.Args[i+1] == id
}

// desktopTwin is the CLI that resumes session id (the desktop's: the first
// turn runs under --session-id), nil when none runs.
func desktopTwin(t *proc.Table, id string) *proc.Process {
	for _, p := range t.ByPID {
		if p.Comm == claudeComm && resumes(p.Args, id) {
			return p
		}
	}
	return nil
}

// resumes reports whether args resume session id: --resume=<id> or
// --resume <id>.
func resumes(args []string, id string) bool {
	for i, a := range args {
		if a == "--resume="+id || a == resumeFlag && i+1 < len(args) && args[i+1] == id {
			return true
		}
	}
	return false
}

// desktopRecord is host's desktop record, waiting up to focusWait for the
// desktop to write it; nil when it wrote none.
func (a *app) desktopRecord(ctx context.Context, host string) *claude.Record {
	ctx, cancel := context.WithTimeout(ctx, focusWait)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if r, ok := claude.ReadRecord(a.cfg, host); ok {
			return r
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// titleLine says which title the sidebar shows the session under.
func titleLine(name, title string) string {
	switch title {
	case name:
		return fmt.Sprintf("the desktop titled it %q", title)
	case "":
		return fmt.Sprintf("the desktop recorded no title: the sidebar shows it untitled, not as %q, until the session retitles itself after its first turn", name)
	}
	return fmt.Sprintf("the desktop titled it %q, not %q, until the session retitles itself after its first turn", title, name)
}

// browserLine says whether the session's browser actions wait on its
// person: the desktop holds every navigate to a site not allowed yet unless
// it recorded the Chrome permission mode skip_all_permission_checks.
func browserLine(chrome string) string {
	switch chrome {
	case claude.ChromeSkipAll:
		return "its browser actions run without the desktop's site requests (Chrome permission mode " + chrome + ")"
	case "":
		return "the desktop recorded no Chrome permission mode: each navigate to a site it was not allowed on yet waits on a person's site request in its desktop row"
	}
	return fmt.Sprintf("the desktop recorded the Chrome permission mode %s: each navigate to a site it was not allowed on yet waits on a person's site request in its desktop row", chrome)
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
func (a *app) importSession(ctx context.Context, d desk, id, follow string) (string, error) {
	t, err := plat.Machine.Processes()
	if err != nil {
		return "", err
	}
	running := !plat.Opener.Running(t).IsZero()
	// startAgent waited for the window already.
	prev, err := a.showBriefly(ctx, d, resumeURL(id), "local_"+id, follow, running, awayPoll)
	if err != nil {
		return "", fmt.Errorf("importing %s into the desktop: %w", id, err)
	}
	return prev, nil
}

// showBriefly opens url, which shows host in the desktop's main window, and
// once it does, shows the session the window showed before again and
// returns it; empty when there was none to go back to, or it was host or
// follow. A running desktop gets the link only once d takes it, waiting up
// to away: errDesktopInUse or errTyping when it did not.
func (a *app) showBriefly(ctx context.Context, d desk, url, host, follow string, running bool, away time.Duration) (string, error) {
	var prev string
	if running {
		if err := d.await(ctx, away, nil); err != nil {
			return "", err
		}
		prev = awaitFocusOff(ctx, a.cfg.Claude.DesktopLog, host, settleWait)
	}
	if err := plat.Opener.Open(ctx, url, running); err != nil {
		return "", err
	}
	if prev == "" || prev == host || prev == follow || !awaitFocus(ctx, a.cfg.Claude.DesktopLog, host, focusWait) {
		return "", nil
	}
	if err := plat.Opener.Open(ctx, continueURL(prev), true); err != nil {
		return "", fmt.Errorf("showing %s again: %w", prev, err)
	}
	return prev, nil
}

// agentReopenCmd is the unit's ExecStopPost: once a start's first turn has
// ended, it shows the session in the desktop for a moment, which warms the
// desktop's CLI of it (endDesktopTwin stopped the one the import warmed), so
// the session is a peer again and takes follow-ups by message. Only a
// session still on the roster is reopened: a hand-over or agents remove
// took the others off, and a handed-over session must not come back. A
// wake names the desktop id of any roster agent it resumed.
func (a *app) agentReopenCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "reopen <session id | local_ desktop id>",
		Short:  "Warm the desktop's CLI of a started session once its first turn ended",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE:   func(cmd *cobra.Command, args []string) error { return a.reopenSession(cmd.Context(), args[0]) },
	}
}

// reopenSession shows the session of a start or wake (its session id, or a
// desktop id local_…) in the desktop once its headless turn ended. Its wait
// for the person to leave the desktop's window is recorded on the agent
// (agents, the watch's IMPORT WAITS) while it runs, and skipped for an agent
// that asked for a desktop turn.
func (a *app) reopenSession(ctx context.Context, arg string) error {
	// A wake names the desktop id (local_…), a start its session id, which
	// is the desktop id's too.
	id := strings.TrimPrefix(arg, "local_")
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	name, ok := reopens(st, arg)
	if !ok {
		_, err := fmt.Fprintf(a.out, "reopen: %s is no start on the roster, left closed\n", id)
		return err
	}
	// A headless turn that ended on a background wait, its task open, is
	// resumed headless instead: the wait's completion notice never wakes it.
	if resumed, err := a.resumeOnWait(ctx, id); resumed || err != nil {
		if err != nil {
			return a.reopenMissed(name, err)
		}
		return nil
	}
	t, err := plat.Machine.Processes()
	if err != nil {
		return err
	}
	if plat.Opener.Running(t).IsZero() {
		_, err := fmt.Fprintf(a.out, "reopen: the desktop does not run, %s waits for it\n", id)
		return err
	}
	// A start whose import waited out the focus has no desktop record yet:
	// the reopen imports it.
	url := continueURL("local_" + id)
	if _, ok := claude.ReadRecord(a.cfg, "local_"+id); !ok {
		url = resumeURL(id)
	}
	d, err := a.watchDesk(ctx)
	if err != nil {
		return a.reopenMissed(name, err)
	}
	d.urgent = a.asksDesktop(id)
	err = d.await(ctx, reopenAwayWait, a.importWaits(id, name, reopenAwayWait))
	a.importEnded(id)
	if err != nil {
		return a.reopenMissed(name, fmt.Errorf("reopening %s in the desktop: %w", id, err))
	}
	// The standby watch resumes a role's holder headless while this reopen
	// waits for the person to leave the desktop's window: the desktop warms
	// no second CLI beside that turn, whose own reopen follows it.
	if u := wakeRunning(ctx, id); u != "" {
		_, err := fmt.Fprintf(a.out, "reopen: %s was resumed headless meanwhile (%s), whose reopen follows its turn\n", id, u)
		return err
	}
	shownAt := time.Now()
	if _, err := a.showBriefly(ctx, d, url, "local_"+id, "", true, reopenAwayWait); err != nil {
		return a.reopenMissed(name, fmt.Errorf("reopening %s in the desktop: %w", id, err))
	}
	if _, err := fmt.Fprintf(a.out, "reopen: showed local_%s in the desktop, which warms its CLI\n", id); err != nil {
		return err
	}
	if err := a.awaitWarmed(ctx, id, name, shownAt); err != nil {
		return err
	}
	line, err := a.keepTitle(ctx, id, name)
	if err != nil {
		return a.reopenMissed(name, err)
	}
	if _, err := fmt.Fprintln(a.out, "reopen: "+line); err != nil {
		return err
	}
	// A start whose import waited for the reopen, or whose steward did not
	// set its model, has none yet.
	var sa startedAgent
	if line := a.keepImport(ctx, id, name, &sa); line != "" {
		_, err = fmt.Fprintln(a.out, "reopen: "+line)
	}
	return err
}

// reopenMissed records a reopen the desktop did not take (not shown, its
// title not restored) in the event log and ends the unit successfully: the
// session's turn ended as it should, the desktop's record is what fell
// short, and its owner reads that in the log and in `agents`, not in a
// failed unit.
func (a *app) reopenMissed(name string, why error) error {
	_ = a.store.Update(func(*state.State) ([]state.Event, error) {
		return []state.Event{event(state.Party{Name: name}, "agent.reopen", "missed: %v", why)}, nil
	})
	_, err := fmt.Fprintf(a.out, "reopen: missed, %v\n", why)
	return err
}

// reopens reports whether the session id is one of beekeeper's starts that a
// roster entry still holds, and the entry's name, the session's title.
func reopens(st *state.State, id string) (string, bool) {
	if strings.HasPrefix(id, "local_") {
		i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool {
			return ag.HostSession == id || ag.Session != "" && "local_"+ag.Session == id
		})
		if i < 0 {
			return "", false
		}
		return st.Agents[i].Name, true
	}
	if !slices.ContainsFunc(st.Starts, func(s state.Start) bool { return s.Session == id }) {
		return "", false
	}
	p := state.Party{Session: id}
	i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(p) })
	if i < 0 {
		return "", false
	}
	return st.Agents[i].Name, true
}

// awaitFocusOff is the session the desktop's main window shows once it
// shows another than host, waiting up to wait; host when it stays, empty
// when the log is unreadable (nothing to go back to). A reopen right after
// its start's import finds the window on host until the start's switch
// back lands, and would otherwise leave it there.
func awaitFocusOff(ctx context.Context, log, host string, wait time.Duration) string {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		f, err := claude.DesktopFocus(log)
		if err != nil || f != host {
			return f
		}
		select {
		case <-ctx.Done():
			return f
		case <-tick.C:
		}
	}
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

// desk is the desktop as a claude:// link finds it: whether its window has
// the focus, and when the person last typed or pointed.
type desk struct {
	// quiet is desktop.typingQuiet; negative, input is not watched.
	quiet time.Duration
	last  func() time.Time
	// locked reports whether the screen is locked (screenLocked); nil: it
	// is not asked.
	locked func() bool
	// urgent reports whether the session the link shows asked for a desktop
	// turn: the window's focus does not hold the link, and the person's
	// typing holds it for desktopTurnWait at most. Nil: none asked.
	urgent func() bool
}

// watchDesk watches the person's input until ctx ends, unless
// desktop.typingQuiet is negative.
func (a *app) watchDesk(ctx context.Context) (desk, error) {
	d := desk{quiet: a.cfg.Desktop.TypingQuiet.Duration, locked: screenLocked}
	if d.quiet < 0 {
		return d, nil
	}
	last, err := desktopInput(ctx)
	if err != nil {
		return d, fmt.Errorf("watching the person's input before a desktop link (desktop.typingQuiet: -1s opens links without it): %w", err)
	}
	d.last = last
	return d, nil
}

// await waits up to wait for the desktop to take a link: its window without
// the focus, and the person's input quiet for desktop.typingQuiet. It
// returns what held the link at the end, errDesktopInUse or errTyping, and
// tells onHeld (when not nil) each time what holds it changes. A compositor
// that cannot be asked counts as the window keeping the focus.
func (d desk) await(ctx context.Context, wait time.Duration, onHeld func(error)) error {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(awayPoll)
	defer tick.Stop()
	var said error
	for {
		held := d.holds(ctx, start)
		if held == nil {
			return nil
		}
		if onHeld != nil && held != said {
			onHeld(held)
			said = held
		}
		select {
		case <-ctx.Done():
			return held
		case <-tick.C:
		}
	}
}

// holds is what holds a link a wait that began at start, nil when nothing
// does: under a locked screen nothing; else the window's focus, unless the
// session asked for a desktop turn, then the person's typing, for
// desktopTurnWait at most when it did.
func (d desk) holds(ctx context.Context, start time.Time) error {
	if d.locked != nil && d.locked() {
		return nil
	}
	urgent := d.urgent != nil && d.urgent()
	if !urgent {
		if active, err := desktopWindowActive(ctx); err != nil || active {
			return errDesktopInUse
		}
	}
	if d.last == nil || time.Since(d.last()) >= d.quiet || urgent && time.Since(start) >= desktopTurnWait {
		return nil
	}
	return errTyping
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

// taskPrompt is the part of a worker's first prompt that is its task: the
// brief, enclosed so that a hand-over passes on the brief alone.
func taskPrompt(brief string) string {
	return "Your task:\n" + briefOpen + "\n" + brief + "\n" + briefClose
}

// workerPrompt is a worker's first prompt: the worker rules beekeeper
// ships with its role skills, versioned, ahead of prompt. A brief carries
// only its task.
func workerPrompt(prompt string) string {
	return fmt.Sprintf("Beekeeper %s gives every worker it starts these rules; they hold for the whole task.\n\n%s\n\n%s",
		project.Version(), plugin.WorkerRules(), prompt)
}

// briefTask is the roster's task for a brief: its first line without a
// Markdown heading's hashes.
func briefTask(brief string) string {
	line, _, _ := strings.Cut(brief, "\n")
	return truncate(strings.TrimSpace(strings.TrimLeft(line, "# ")), 80)
}

// agentArgv is the started session's command line: one headless turn in
// bypassPermissions under the id beekeeper recorded; flags go before the
// brief.
func agentArgv(bin, id, name, model, brief string, flags ...string) []string {
	argv := []string{bin, "-p", sessionIDFlag, id, "--permission-mode", state.ModeBypass, "-n", name}
	if model != "" {
		argv = append(argv, modelFlag, model)
	}
	argv = append(argv, flags...)
	return append(argv, "--", brief)
}

// launch runs argv in a transient user service: it gets the user manager's
// environment, not the caller's session variables, and outlives the caller;
// a configuration file the caller named is passed on as $BEEKEEPER_CONFIG,
// env (KEY=value) is added, and stopPost runs once argv has ended.
// KillMode=process leaves what the turn started running when it ends, as a
// terminal would.
func launch(unit, dir, config string, stopPost, argv []string, env ...string) error {
	// A session beekeeper stops (SIGTERM) ended as asked, not failed.
	u := platform.Unit{Name: unit, Dir: dir, Argv: argv, KeepChildren: true, TermIsSuccess: true, StopPost: stopPost,
		// The reopen may wait for the session to retitle itself.
		StopTimeout: reopenAwayWait + stopPostWait}
	u.Env = append(u.Env, env...)
	if config != "" {
		u.Env = append(u.Env, "BEEKEEPER_CONFIG="+config)
	}
	if dir, ok := devBuild(); ok {
		// Its beekeeper commands run the build that started it.
		u.Env = append(u.Env, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	return plat.Launcher.Start(u)
}

// devBuild is the folder of the running beekeeper when it is not the one on
// PATH: a development build, which the sessions it starts run too.
func devBuild() (string, bool) {
	self, err := os.Executable()
	if err != nil {
		return "", false
	}
	onPath, err := exec.LookPath("beekeeper")
	if err == nil {
		if a, err1 := os.Stat(self); err1 == nil {
			if b, err2 := os.Stat(onPath); err2 == nil && os.SameFile(a, b) {
				return "", false
			}
		}
	}
	return filepath.Dir(self), true
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

// importTitleWindow is how much of a transcript's end Claude Desktop's
// import reads for the session's title (its last custom-title line).
const importTitleWindow = 256 << 10

// titleTranscript appends the session's name as its title to its transcript,
// the line the desktop's own rename writes, so that the import finds it: -n
// writes it once at the start, and a first turn that grew the transcript
// past importTitleWindow left the session untitled in the sidebar. One
// append of one line, as the running CLI appends its own.
func titleTranscript(projectsDir, id, name string) error {
	m, _ := filepath.Glob(filepath.Join(projectsDir, "*", id+".jsonl"))
	if len(m) == 0 {
		return fmt.Errorf("session %s has no transcript to title", id)
	}
	line, err := json.Marshal(struct {
		Type        string `json:"type"`
		CustomTitle string `json:"customTitle"`
		SessionID   string `json:"sessionId"`
	}{"custom-title", name, id})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(m[0], os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return fmt.Errorf("titling session %s: %w", id, err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("titling session %s: %w", id, err)
	}
	return f.Close()
}

// unitEnded reports whether the transient unit has ended.
func unitEnded(ctx context.Context, unit string) bool {
	s := plat.Launcher.State(ctx, unit)
	return s == "inactive" || s == "failed"
}
