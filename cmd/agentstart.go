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
	// stopPostWait bounds the reopen's work past its wait for the person:
	// the desktop's CLI, the retitle and model requests and the desktop
	// recording them. With reopenAwayWait it is the reopen unit's runtime
	// cap (RuntimeMaxSec).
	stopPostWait = 10 * time.Minute
	// turnStopWait bounds the stop of a start's or wake's unit: claude -p
	// ends on SIGTERM within seconds, KillMode=process signals only it, and
	// the stop-post starts the reopen's unit and returns. Under the user
	// manager's default stop timeout (90 s upstream), so a shutdown never
	// waits on a turn; what runs past it is killed.
	turnStopWait = time.Minute
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
	// compositor which window has focus: each ask runs hyprctl, and a
	// reopen may wait reopenAwayWait.
	awayPoll = 2 * time.Second
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
		Use:   agentStartName + " <name> <brief file>",
		Short: "Start an agent session in bypass from the command line and import it into the desktop",
		Long: `start starts a Claude Code session without a click and runs it in
Claude Desktop from its first turn. Before the session exists, beekeeper
records its id as one of its starts and registers it on the roster under
<name>, busy with --task, by default the brief's first line (or with the
open task of a stopped session's entry under that name, which it takes
over), so the roster shows it at work from its start.

A seed turn creates the session: "claude -p" under a session id beekeeper
chooses, without tools, with the brief as its first prompt, in a transient
user unit (beekeeper-agent-<id>, its output in journalctl --user -u <unit>);
it only answers "ready" and gives the transcript its model. Once it ended,
beekeeper imports the session into the desktop (claude://resume?session=<id>):
it shows in the sidebar as local_<id>, titled with <name> (beekeeper appends
the name's custom-title line to the transcript first, and a steward restores
a title or model the import dropped). The import switches the desktop's main
window to the new session for a moment and back to the session it showed
(claude://code/continue); the window's focus does not hold it, the person's
typing for 1 minute at most, and a locked screen not at all.

The task then runs as a desktop turn, which the person sees in the
session's row: the desktop's CLI of the session, which the import warms,
takes the message that starts it; when the desktop runs none, a steward's
send through the desktop's session messaging starts one with it. Before
either spawn beekeeper keeps the desktop under its cap of CLIs by ending one
of its own finished or parked workers' CLIs, never a person's session. Only
where the desktop cannot run the turn (it does not run, it did not import
the session, at its cap with no CLI of beekeeper's to end or the person
still typing, so the session has no row, or no steward took the send) is
the session resumed headless, as agents wake does, and start says why; the
reopen after that turn imports a session the desktop did not.

A start that delivers no turn of the task fails: it exits non-zero, logs
"task not delivered" with the reason, and the roster's REACHABLE (and the
watch's AGENTS STOPPED line) says "task not delivered" until agents wake
delivers a turn.

The first prompt is the worker rules beekeeper ships with its role skills
(the worker-rules skill, under the binary's version), then the brief as the
task: a brief carries only its task, and a worker reports to "the
supervisor", which the PreToolUse hook delivers to the role's holder.

The desktop runs the session's turns in acceptEdits (its import always
drops bypass), so requests no allow rule covers would stop at a card:
beekeeper hook permissionrequest answers them, for beekeeper's starts only.
The desktop's browser asks: Claude Desktop holds a desktop turn's navigate
to a site the session was not allowed on yet for a person's site request in
its row, which no hook answers, unless the session runs in auto or bypass
(the import never keeps bypass). So beekeeper hook pretooluse refuses the
Claude in Chrome tools in the desktop turns of beekeeper's starts and names
beekeeper browse, which runs the browser steps in a headless turn that has
the CLI's own Chrome tools and nothing else; every headless turn of a start (--chrome, in
bypass) has that connection too. Neither ever waits on a site request.
start says which Chrome mode the desktop recorded, and agents shows it per
agent (BROWSER asks or skips). --desktop
is kept for scripts: every start imports past the window's focus now.

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
			// an omp agent whose provider's key is in the vault starts where
			// the vault session is: the host's broker
			if harness == omp.Harness && a.ompStartBrokered(model) {
				return a.agentsOnHost(cmd, args, agentsBrokeredTimeout+time.Minute)
			}
			// a sandboxed session's start reads its brief and gives the
			// agent its directory through the broker, within its own lists
			if err := a.sandboxPaths([]string{args[1]}, []string{dir}); err != nil {
				return err
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
			_, err = fmt.Fprintf(a.out, "started %s: session %s, desktop local_%s, in %s, busy with %q\n%s\n",
				name, sa.id, sa.id, sa.dir, sa.task, sa.turn)
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
			return err
		},
	}
	c.Flags().StringVar(&model, "model", "", "the session's model (default: Claude Code's; omp: omp.model)")
	c.Flags().StringVar(&dir, "dir", "", "the session's working directory (default: agents.dir, else the current one)")
	c.Flags().StringVar(&task, "task", "", "the task the roster shows it busy with (default: the brief's first line)")
	c.Flags().StringVar(&harness, "harness", "claude", "the agent harness: claude or omp")
	c.Flags().BoolVar(&desktop, "desktop", false, "the task needs desktop turns: import it past the desktop window's focus, as agents desktop does")
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
	// headless runs the brief as the session's first turn from the command
	// line beside the import: a role's successor, whose first turn only takes
	// the role and whose relay hands it its desktop turn. Otherwise the first
	// turn only seeds the session and the brief runs as a desktop turn.
	headless bool
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
	// turn says how the task's first turn runs: a desktop turn, or why it
	// runs headless.
	turn string
}

// startAgent records and registers the session, starts its first turn in a
// transient user unit and imports it into the desktop once the transcript
// holds its first reply, which carries its model.
func (a *app) startAgent(ctx context.Context, sp agentStart) (startedAgent, error) {
	dir, err := a.agentDir(sp.dir)
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
	if !sp.headless {
		sa, err := a.startVisible(ctx, d, sp, bin, unit, dir, startedAgent{id: id, unit: unit, dir: dir, task: reg.task}, by)
		if err != nil {
			return sa, a.undelivered(by, s.Party, err)
		}
		return sa, nil
	}
	self, err := os.Executable()
	if err != nil {
		return startedAgent{}, err
	}
	if err := launch(unit, dir, a.explicitConfig(), reopenStopPost(self, id), headlessStartArgv(bin, id, sp.name, sp.model, sp.brief)); err != nil {
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
	if err := a.importBeside(ctx, d, id, sp.name, follow, &sa); err != nil {
		return sa, err
	}
	sa.restored = a.keepImport(ctx, id, sp.name, &sa)
	return sa, nil
}

// undelivered records that the start of p delivered no turn of its task:
// the roster entry says so until agents wake delivers one, the event log
// has the reason, and the returned error makes the start fail, so its
// caller retries instead of counting on a worker that never got its task.
func (a *app) undelivered(by, p state.Party, why error) error {
	_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
		if i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(p) }); i >= 0 {
			st.Agents[i].Undelivered = why.Error()
		}
		return []state.Event{event(by, "agents.start", "%s: task not delivered: %v", p.Name, why)}, nil
	})
	return fmt.Errorf("%s: task not delivered: %w (beekeeper agents wake %q <message> delivers it, beekeeper agents remove %q takes it off the roster)",
		p.Name, why, p.Name, p.Name)
}

// seedNote ends a seed turn's prompt: the turn only creates the session.
const seedNote = "beekeeper: this first turn only creates your session and has no tools. Reply with the single word ready and nothing else. " +
	"Your task starts with the next message, a turn in the desktop, where you work it."

// taskTurn is the message that starts a seeded worker's task as a desktop
// turn.
const taskTurn = "beekeeper: this desktop turn starts your task. Work the task of your first prompt above to the finish, under the worker rules it gives."

// startVisible runs a started session in the desktop from its first turn:
// a seed turn without tools creates the session from the command line with
// the brief as its first prompt (the transcript, its title and model, which
// the desktop's import reads), the desktop imports it under its name once
// the seed ended, past the window's focus and the person's typing within
// desktopTurnWait, and the task's first turn then runs as a desktop turn
// (turnInDesktop). Only a desktop that cannot run it gets the turn headless,
// as agents wake resumes a session, and sa.turn says why.
func (a *app) startVisible(ctx context.Context, d desk, sp agentStart, bin, unit, dir string, sa startedAgent, by state.Party) (startedAgent, error) {
	id := sa.id
	argv := agentArgv(bin, id, sp.name, sp.model, sp.brief+"\n\n"+seedNote, toolsFlag, "", strictMCPConfigFlag)
	if err := launch(unit, dir, a.explicitConfig(), nil, argv); err != nil {
		return startedAgent{}, fmt.Errorf("starting %s: %w (the start stays recorded; beekeeper agents remove %q takes it off the roster)", sp.name, err, sp.name)
	}
	if err := awaitReply(ctx, a.cfg.Claude.ProjectsDir, id, func() bool { return unitEnded(ctx, unit) }, replyQuiet, replyWait); err != nil {
		return startedAgent{}, fmt.Errorf("%w, not imported into the desktop: journalctl --user -u %s", err, unit)
	}
	if err := awaitTurnEnd(ctx, id, replyWait); err != nil {
		return startedAgent{}, fmt.Errorf("its seed turn: %w (journalctl --user -u %s)", err, unit)
	}
	var follow string
	if sp.replaces != nil {
		follow = sp.replaces.HostSession
	}
	d.urgent = func() bool { return true }
	t, err := plat.Machine.Processes()
	if err != nil {
		return sa, err
	}
	if !plat.Opener.Running(t).IsZero() {
		// An import the desktop does not take (at its cap of CLIs, the
		// person still typing) leaves the session without a row: its task
		// runs headless, whose reopen imports it later.
		if err := a.importVisible(ctx, d, id, sp.name, follow, &sa); err != nil {
			sa.deferred = err
			_ = a.store.Log(event(by, "agents.start", "%s: the desktop did not import it (%v)", sp.name, err))
		}
	}
	w := wakeTarget{name: sp.name, id: id, host: "local_" + id, dir: dir, mode: state.ModeBypass, model: sp.model}
	line, err := a.turnInDesktop(ctx, w, taskTurn, twinWait)
	if err == nil {
		sa.turn = "its task runs as a desktop turn: " + line
		_ = a.store.Log(event(by, "agents.start", "%s: %s", sp.name, sa.turn))
		return sa, nil
	}
	sa.turn = fmt.Sprintf("the desktop runs no turn of it (%v): its task runs headless", err)
	_ = a.store.Log(event(by, "agents.start", "%s: %s", sp.name, sa.turn))
	if err := a.resumeTurn(ctx, by, w, taskTurn); err != nil {
		return sa, err
	}
	return sa, nil
}

// importVisible imports a seeded session into the running desktop once the
// person's typing paused, within desktopTurnWait, and gives its row the
// session's name.
func (a *app) importVisible(ctx context.Context, d desk, id, name, follow string, sa *startedAgent) error {
	if err := d.await(ctx, d.turnWait()+awayPoll, nil); err != nil {
		return err
	}
	if err := a.importBeside(ctx, d, id, name, follow, sa); err != nil {
		return err
	}
	sa.restored = a.keepImport(ctx, id, name, sa)
	// No other steward ran to restore a dropped title: the session's own
	// desktop CLI, which the import warmed, sets it before its task, so the
	// row and the roster carry its name from its first turn.
	if sa.title != name {
		line, err := a.keepTitle(ctx, id, name)
		if err != nil {
			line = err.Error()
		}
		sa.restored = strings.TrimPrefix(sa.restored+"; "+line, "; ")
		if r, ok := claude.ReadRecord(a.cfg, "local_"+id); ok {
			sa.title, sa.model = r.Title, r.Model
		}
	}
	return nil
}

// importBeside titles session id name and imports it into the desktop
// while a headless turn of it may run (the first turn, or a wake that keeps
// a role's watch and does not end): the turns' units stay frozen until the
// desktop wrote its record, and the CLI the desktop warms for the import is
// stopped while a turn runs, so the session runs one CLI. It fills sa's
// kept, title, model, chrome and twin.
func (a *app) importBeside(ctx context.Context, d desk, id, name, follow string, sa *startedAgent) error {
	err := whileFrozen(ctx, sessionUnits(ctx, id, false), func() error {
		if err := titleTranscript(a.cfg.Claude.ProjectsDir, id, name); err != nil {
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
		return err
	}
	sa.twin, err = endDesktopTwin(ctx, id)
	return err
}

// whileFrozen runs fn with the units of the session's headless turns
// frozen, and thaws them once fn returned, whatever fn returned. The
// desktop's import reads the transcript's identity and then its end for the
// session's title and model, and drops that read when the transcript
// changed in between: a turn that appends a line meanwhile leaves the import
// untitled, without a model. A unit no longer active (its turn ended, or is
// ending) has no writer and is not frozen.
func whileFrozen(ctx context.Context, units []string, fn func() error) error {
	var frozen []string
	thaw := func(err error) error {
		for _, u := range frozen {
			if terr := plat.Launcher.Thaw(context.WithoutCancel(ctx), u); terr != nil {
				err = errors.Join(err, fmt.Errorf("thawing %s: %w (systemctl --user thaw %s resumes its turn)", u, terr, u))
			}
		}
		return err
	}
	for _, u := range units {
		if err := plat.Launcher.Freeze(ctx, u); err != nil {
			if plat.Launcher.State(ctx, u) != "active" {
				continue
			}
			return thaw(fmt.Errorf("freezing %s for the import: %w", u, err))
		}
		frozen = append(frozen, u)
	}
	return thaw(fn())
}

// endDesktopTwin stops the CLI the desktop warms for an imported session
// while a headless turn of it (the first turn, a wake turn) still runs: two
// CLIs on one session id are two peers under its name, and a message by
// name could reach the desktop's copy, which would run a turn of its own
// beside the headless one. It waits up to twinWait for the desktop's CLI
// and returns its PID, 0 when none came (a desktop at its cap warms none)
// or the headless turn ended first: from then on the desktop's CLI is the
// session's one (agents reopen warms it once the headless turn ended).
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
		if headlessTurn(t, id) == "" {
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

// startsSession reports whether p is a first turn of session id: a claude
// process started under --session-id <id>.
func startsSession(p *proc.Process, id string) bool {
	i := slices.Index(p.Args, sessionIDFlag)
	return p.Comm == claudeComm && i >= 0 && i+1 < len(p.Args) && p.Args[i+1] == id
}

// desktopTwin is the desktop's CLI of session id, nil when none runs: one
// that resumes it and prints no turn (a first turn runs under --session-id,
// a wake turn is a headless --resume).
func desktopTwin(t *proc.Table, id string) *proc.Process {
	for _, p := range t.ByPID {
		if p.Comm == claudeComm && resumes(p.Args, id) && !printsTurn(p) {
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
		return "the desktop recorded no Chrome permission mode: each navigate to a site it was not allowed on yet waits on a person's site request in its desktop row, so its desktop turns run browser steps through beekeeper browse"
	}
	return fmt.Sprintf("the desktop recorded the Chrome permission mode %s: each navigate to a site it was not allowed on yet waits on a person's site request in its desktop row, so its desktop turns run browser steps through beekeeper browse", chrome)
}

// twinLine says whether the headless turn is the session's only CLI.
func twinLine(twin int) string {
	if twin == 0 {
		return "the desktop warmed no CLI of its own while the headless turn ran: messages by name reach the session's one CLI"
	}
	return fmt.Sprintf("stopped the desktop's CLI %d of the session: the headless turn is its only CLI and the only peer under its name, until someone opens it in the desktop", twin)
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
	// The caller waited for the window already; the link checks once more.
	// A desktop turn's link does not: the caller's wait gave the person's
	// typing its bound, and a keystroke since then must not miss the import.
	away := awayPoll
	if d.urgent != nil && d.urgent() {
		away = 0
	}
	prev, err := a.showBriefly(ctx, d, resumeURL(id), "local_"+id, follow, running, away, nil)
	if err != nil {
		return "", fmt.Errorf("importing %s into the desktop: %w", id, err)
	}
	return prev, nil
}

// showBriefly opens url, which shows host in the desktop's main window, and
// once it does, shows the session the window showed before again and
// returns it; empty when there was none to go back to, or it was host or
// follow. A running desktop gets the link only once d takes it, waiting up
// to away (zero: the caller's wait stands): errDesktopInUse or errTyping
// when it did not. ready, unless nil,
// is asked right before the link opens, and its error ends the show.
func (a *app) showBriefly(ctx context.Context, d desk, url, host, follow string, running bool, away time.Duration, ready func() error) (string, error) {
	var prev string
	if running {
		if away > 0 {
			if err := d.await(ctx, away, nil); err != nil {
				return "", err
			}
		}
		prev = awaitFocusOff(ctx, a.cfg.Claude.DesktopLog, host, settleWait)
	}
	if ready != nil {
		if err := ready(); err != nil {
			return "", err
		}
	}
	if running {
		if err := a.makeRoom(ctx, host); err != nil {
			return "", err
		}
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

// agentReopenCmd runs once a start's or wake's headless turn has ended: it
// shows the session in the desktop for a moment, which warms the desktop's
// CLI of it (endDesktopTwin stopped the one the import warmed), so the
// session is a peer again and takes follow-ups by message. The turn's unit
// runs it as its ExecStopPost with --detach, which starts the reopen in a
// unit of its own (beekeeper-reopen-<id>-…, bounded by RuntimeMaxSec) and
// returns at once: the wait for the person is no part of a unit's stop,
// which a shutdown waits for (the doctor starts such a unit for a rowless
// worker too); --turn names the turn's unit, whose leftover processes the
// reopen reads. Only a session still on the roster is reopened: a hand-over
// or agents remove took the others off, and a handed-over session must not
// come back. A wake names the desktop id of any roster agent it resumed.
func (a *app) agentReopenCmd() *cobra.Command {
	var detach bool
	var turn string
	c := &cobra.Command{
		Use:    "reopen [--detach | --turn <unit>] <session id | local_ desktop id>",
		Short:  "Warm the desktop's CLI of a started session once its first turn ended",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if detach {
				return a.detachReopen(args[0])
			}
			return a.reopenSession(cmd.Context(), args[0], turn)
		},
	}
	c.Flags().BoolVar(&detach, detachFlag, false, "start the reopen in a transient unit of its own and return")
	c.Flags().StringVar(&turn, turnFlag, "", "the start's or wake's unit whose turn ended")
	return c
}

// reopenStopPost is a start's or wake's ExecStopPost: agents reopen --detach
// of arg (the session id, or a wake's local_ desktop id), which starts the
// reopen in a unit of its own and returns.
func reopenStopPost(self, arg string) []string {
	return []string{self, agentsName, reopenName, "--" + detachFlag, arg}
}

// detachReopen is the stop-post of a start's or wake's unit: it starts the
// reopen of arg in a unit of its own, in the turn's working directory, and
// names the turn's unit (this process's own) for its leftover processes. A
// reopen unit that does not start (at a shutdown, whose transaction refuses
// a start) is a missed reopen in the event log, never a failed unit.
func (a *app) detachReopen(arg string) error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	id := strings.TrimPrefix(arg, "local_")
	unit := reopenUnit(id)
	if err := a.launchReopen(unit, dir, arg, ownUnit()); err != nil {
		name := id
		if st, err := a.store.Read(); err == nil {
			if n, ok := reopens(st, arg); ok {
				name = n
			}
		}
		return a.reopenMissed(name, fmt.Errorf("starting its reopen %s: %w", unit, err))
	}
	_, err = fmt.Fprintf(a.out, "reopen: %s shows %s in the desktop once its turn's unit stopped\n", unit, id)
	return err
}

// reopenSession shows the session of a start or wake (its session id, or a
// desktop id local_…) in the desktop once its headless turn ended; turn is
// the start's or wake's unit that ran the turn, empty for a doctor's
// reopen. Its wait for the person to leave the desktop's window is recorded
// on the agent (agents, the watch's IMPORT WAITS) while it runs, and skipped
// for an agent that asked for a desktop turn.
func (a *app) reopenSession(ctx context.Context, arg, turn string) error {
	// A wake names the desktop id (local_…), a start its session id, which
	// is the desktop id's too.
	id := strings.TrimPrefix(arg, "local_")
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	name, ok := reopens(st, arg)
	if ok && pastRun(st, state.Party{Session: id, HostSession: "local_" + id, Name: name}, a.now) {
		_, err := fmt.Fprintf(a.out, "reopen: %s (%s) is a relieved role run: its work ended with the relay, left closed\n", name, id)
		return err
	}
	if !ok {
		_, err := fmt.Fprintf(a.out, "reopen: %s is no start on the roster, left closed\n", id)
		return err
	}
	// A headless turn that ended on a background wait, its task open, is
	// resumed headless instead: the wait's completion notice never wakes it.
	if resumed, err := a.resumeOnWait(ctx, id, turn); resumed || err != nil {
		if err != nil {
			return a.reopenMissed(name, err)
		}
		return nil
	}
	release, waits, err := a.holdReopen(id)
	if err != nil {
		return a.reopenMissed(name, err)
	}
	if waits {
		_, err := fmt.Fprintf(a.out, "reopen: a reopen of %s already waits to show it, left to that one\n", id)
		return err
	}
	defer release()
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
	wctx, urgent, stop := a.watchReopen(ctx, arg, id)
	defer stop()
	d.urgent = urgent
	err = d.await(wctx, reopenAwayWait, a.importWaits(id, name, reopenAwayWait))
	a.importEnded(id)
	if end, ok := errors.AsType[reopenEnd](context.Cause(wctx)); ok {
		return a.reopenEnded(ctx, id, name, end)
	}
	if err != nil {
		return a.reopenMissed(name, fmt.Errorf("reopening %s in the desktop: %w", id, err))
	}
	if u := wakeRunning(ctx, id); u != "" {
		return a.yieldToWake(ctx, d, id, name, u)
	}
	// The show waits again for the person's typing to pause: a resume that
	// started meanwhile is looked for right before it.
	ready := func() error {
		if u := wakeRunning(ctx, id); u != "" {
			return wakeStarted(u)
		}
		return nil
	}
	shownAt := time.Now()
	if _, err := a.showBriefly(wctx, d, url, "local_"+id, "", true, reopenAwayWait, ready); err != nil {
		if end, ok := errors.AsType[reopenEnd](context.Cause(wctx)); ok {
			return a.reopenEnded(ctx, id, name, end)
		}
		if u, ok := errors.AsType[wakeStarted](err); ok {
			return a.yieldToWake(ctx, d, id, name, string(u))
		}
		return a.reopenMissed(name, fmt.Errorf("reopening %s in the desktop: %w", id, err))
	}
	stop()
	if _, err := fmt.Fprintf(a.out, "reopen: showed local_%s in the desktop, which warms its CLI\n", id); err != nil {
		return err
	}
	return a.reopenWarmed(ctx, id, name, shownAt)
}

// reopenEnded ends the reopen of session id once it has nothing left to
// show: a desktop CLI of the session that runs already is kept as the
// warmed one, and its title and model are seen to as after a show.
func (a *app) reopenEnded(ctx context.Context, id, name string, end reopenEnd) error {
	if !end.twin {
		_, err := fmt.Fprintf(a.out, "reopen: %s (%s) left closed: %s\n", name, id, end.why)
		return err
	}
	if _, err := fmt.Fprintf(a.out, "reopen: no show of local_%s: %s\n", id, end.why); err != nil {
		return err
	}
	return a.reopenWarmed(ctx, id, name, time.Now())
}

// reopenWarmed waits for the desktop CLI of session id that the show at
// shownAt warms, and keeps the session's title and model.
func (a *app) reopenWarmed(ctx context.Context, id, name string, shownAt time.Time) error {
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

// wakeStarted is the wake unit whose headless turn started before a reopen
// showed its session.
type wakeStarted string

func (u wakeStarted) Error() string { return "its wake turn " + string(u) + " runs" }

// yieldToWake ends the reopen of session id, whose wake turn runs in unit
// u: the standby watch resumed a role's holder headless while the reopen
// waited for the person to leave the desktop's window, and the desktop
// warms no second CLI beside that turn, whose own reopen follows it. That
// turn keeps the role's watch and may not end before the next relay, so a
// session the desktop never imported gets its row and title now.
func (a *app) yieldToWake(ctx context.Context, d desk, id, name, u string) error {
	if _, ok := claude.ReadRecord(a.cfg, "local_"+id); ok {
		_, err := fmt.Fprintf(a.out, "reopen: %s was resumed headless meanwhile (%s), whose reopen follows its turn\n", id, u)
		return err
	}
	return a.importBesideWake(ctx, d, id, name, u)
}

// importBesideWake imports session id, which the desktop never imported,
// beside the wake turn of unit: the row and title appear in the desktop,
// and the turn stays the session's only CLI.
func (a *app) importBesideWake(ctx context.Context, d desk, id, name, unit string) error {
	var sa startedAgent
	if err := a.importBeside(ctx, d, id, name, "", &sa); err != nil {
		return a.reopenMissed(name, fmt.Errorf("importing %s beside its headless turn %s: %w", id, unit, err))
	}
	if _, err := fmt.Fprintf(a.out, "reopen: imported local_%s into the desktop beside its headless turn (%s); %s; %s\n",
		id, unit, titleLine(name, sa.title), twinLine(sa.twin)); err != nil {
		return err
	}
	if line := a.keepImport(ctx, id, name, &sa); line != "" {
		_, err := fmt.Fprintln(a.out, "reopen: "+line)
		return err
	}
	return nil
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
	// typing holds it for turn at most. Nil: none asked.
	urgent func() bool
	// turn bounds how long the person's typing holds a desktop turn's link;
	// zero: desktopTurnWait.
	turn time.Duration
}

// turnWait is how long the person's typing holds a desktop turn's link.
func (d desk) turnWait() time.Duration {
	if d.turn > 0 {
		return d.turn
	}
	return desktopTurnWait
}

// watchDesk watches the person's input until ctx ends, unless
// desktop.typingQuiet is negative.
func (a *app) watchDesk(ctx context.Context) (desk, error) {
	d := desk{quiet: a.cfg.Desktop.TypingQuiet.Duration, locked: every(lockPoll, screenLocked)}
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
// d.turnWait() at most when it did.
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
	if d.last == nil || time.Since(d.last()) >= d.quiet || urgent && time.Since(start) >= d.turnWait() {
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

// headlessStartArgv is the first turn of a headless start: in bypass with the
// CLI's own Chrome connection, as every bypass wake turn (wakeArgv). The bypass
// is the turn's scope, so its Chrome tools are not narrowed to browse's.
func headlessStartArgv(bin, id, name, model, brief string) []string {
	return agentArgv(bin, id, name, model, brief, chromeFlag)
}

// agentArgv is the started session's command line: one headless turn in
// bypassPermissions under the id beekeeper recorded; flags go before the
// brief.
func agentArgv(bin, id, name, model, brief string, flags ...string) []string {
	argv := []string{bin, "-p", sessionIDFlag, id, permissionModeFlag, state.ModeBypass, "-n", name}
	if model != "" {
		argv = append(argv, modelFlag, model)
	}
	argv = append(argv, flags...)
	return append(argv, "--", brief)
}

// launch runs argv, a headless turn, in a transient user service (startUnit)
// with env (KEY=value) added; stopPost runs once argv has ended, within the
// unit's stop budget of turnStopWait, which a stop-post that returns at once
// (reopenStopPost) keeps. KillMode=process leaves what the turn started
// running when it ends, as a terminal would.
func launch(unit, dir, config string, stopPost, argv []string, env ...string) error {
	// A session beekeeper stops (SIGTERM) ended as asked, not failed.
	return startUnit(platform.Unit{Name: unit, Dir: dir, Argv: argv, KeepChildren: true, TermIsSuccess: true, StopPost: stopPost, StopTimeout: turnStopWait}, config, env...)
}

// startUnit starts u as a transient user service: it gets the user
// manager's environment, not the caller's session variables, and outlives
// the caller; a configuration file the caller named is passed on as
// $BEEKEEPER_CONFIG, a scratch state it keeps as $BEEKEEPER_STATE_FROM, and
// env (KEY=value) is added.
func startUnit(u platform.Unit, config string, env ...string) error {
	u.Env = append(u.Env, env...)
	if config != "" {
		u.Env = append(u.Env, "BEEKEEPER_CONFIG="+config)
	}
	if f := os.Getenv(stateFromEnv); f != "" {
		u.Env = append(u.Env, stateFromEnv+"="+f)
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
