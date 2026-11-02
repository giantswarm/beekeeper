package cmd

import (
	"bytes"
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
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/lease"
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
func screenLocked() bool { return screenLocker() != "" }

// desktopInput watches the person's keyboards and pointers; tests replace
// it.
var desktopInput = func(ctx context.Context) (func() time.Time, error) { return plat.Input.Watch(ctx) }

func (a *app) agentStartCmd() *cobra.Command {
	var nameFlag, model, dir, task, harness, mode, choiceLine string
	var host, repository, gitRef string
	var exclude []string
	var desktop, builds, force, dryRun, review, ofSelfHosted bool
	c := &cobra.Command{
		Use:   agentStartName + " [--name <name>] [<name>] <brief file>",
		Short: "Start an agent session in bypass: imported into the desktop, or a background session with Remote Control",
		Long: `start starts a Claude Code session without a click and runs it in
Claude Desktop from its first turn. Before the session exists, beekeeper
records its id as one of its starts and registers it on the roster under
<name>, busy with --task, by default the brief's first line (or with the
open task of a stopped session's entry under that name, which it takes
over), so the roster shows it at work from its start. A brief naming an
issue or pull request by URL makes the first one named the session's serve
record (as sessions serve records it), so board next never offers that
item again while the worker runs. That record is the desk's claim of the
issue on GitHub too (board.claims): while another desk's live claim holds
the issue, start refuses with that claim's desk, session and time, exit 3;
--force starts anyway and the event log records the claim overridden
(agents.start). A stale claim is said, never honoured.

The start is judged first, as beekeeper capacity judges one: the busy
agents against capacity.ceiling, MemAvailable, disk swap's growth, the
1-minute load and the kind labs. --builds marks a task that builds, tests
or lints (its commands take the build slot under beekeeper run): such a
start, refused by a busy build slot alone, waits its turn in the build
slot queue it shares with beekeeper run's waiters, first in first out, up
to MEMCAP_WAIT, else 8m, or 60m in a background Bash call, and is judged
again once its turn came; a turn that does not come refuses it with the
waiters still ahead. Without
--builds the start is one that does not build (a filer, a dispatcher, a
reviewer, a promoter proving a release read-only): the slot is no guard
of it, and a busy slot is a note in the verdict, never a refusal. A
verdict without room refuses the start with exit 3, the guards named,
and no session is created; --force starts it anyway and records the
verdict overridden in the event log (agents.start).

A seed turn creates the session: "claude -p" under a session id beekeeper
chooses, without tools, with the brief as its first prompt, in a transient
user unit (beekeeper-agent-<id>, its output in journalctl --user -u <unit>);
it only answers "ready" and gives the transcript its model. Once it ended,
beekeeper imports the session into the desktop (claude://resume?session=<id>):
it shows in the sidebar as local_<id>, titled with <name> (beekeeper appends
the name's custom-title line to the transcript first, and a steward restores
a title or model the import dropped; the session never sets its own model,
which the desktop refuses). --model is the model its turns keep: while the
desktop records another, its turns run headless on --model, and start says
so with the call that sets it. The import switches the desktop's main
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

Under a worker profile (agents.profile: one of agents.profiles, or the
built-in minimal) no seed turn runs: the task runs as a headless turn under
the profile (--tools and a --settings file of its plugins, MCP servers and
deny rules, ranked over the person's settings for that process only), the
desktop imports the session beside it, and start says "its task runs as a
headless turn under profile <name>". The desktop spawns its own CLI for a
desktop turn, which no profile reaches. --dry-run prints the profile
applied, the first turn's command line and the first prompt, and starts
nothing; with --builds it first prints the build slot as the start would
find it, "free" or the wait with the slots' holders and the waiters ahead,
without joining the queue or holding a slot. The dry run is judged by the
same start gate a real start is, and prints its verdict: the auto choice,
and with --force the verdict it goes over; a verdict without room refuses
the dry run with exit 3 too (--force aside).

A start that delivers no turn of the task fails: it exits non-zero, logs
"task not delivered" with the reason, and the roster's REACHABLE (and the
watch's AGENTS STOPPED line) says "task not delivered" until agents wake
delivers a turn.

The first prompt is the worker rules beekeeper ships with its role skills
(the worker-rules skill, under the binary's version), then the brief as the
task: a brief carries only its task, and a worker reports to "the
supervisor", which the PreToolUse hook delivers to the role's holder. The
rules name the topic skills a worker loads before the step that needs
them (beekeeper:lease, beekeeper:merge, beekeeper:github, beekeeper:park);
a topic no enabled beekeeper plugin installed serves as this binary embeds
it (none installed, or out of date) follows the rules in the first prompt
itself, and start says so with the command that updates the plugin.

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

--mode background (default: agents.mode, else desktop) starts the session
as a Claude Code background session instead: "claude --bg" with Remote
Control under <name>, in bypassPermissions, on --model (default:
agents.model), under the worker profile's flags, with the brief as its
first prompt, which its first turn works. No desktop is involved: no
seed turn, no import, no claude:// link and no reopen unit. The call runs
with none of the caller's CLAUDE* or CCD* variables, so the session is
its own, never the caller's: its commands, notes and holds are booked
under <name>. The claude daemon chooses the session id; start reads it
from claude agents and has the session on the roster under <name>, busy
with --task, when it returns, so no agents assign follows. agents wake,
and every wake beekeeper sends (a note's answer, a timer, the gate's
word, a parked agent's resume), messages a running background session by name, and
continues a stopped one with "claude --bg --resume <id> <message>", which
keeps its Remote Control, mode, name and model. A role's successor and
agents handover start in the mode agents.mode names.

--harness omp starts an omp (oh-my-pi) agent instead: "omp --mode rpc"
in yolo approval mode on --model (default: omp.model; with neither, or a
model omp does not list as provider/id, the start is refused; a bare id,
as capacity pick returns an omp choice, is the one provider/id omp lists
for it, and an id two providers list is refused), in
a transient user unit beekeeper-omp-<id>, with its stdin on a FIFO inbox
in beekeeper's state folder and the brief as its first message. It is
registered on the roster as omp_<id> under <name> and shows in sessions
and agents with harness omp; agents wake writes a message to its inbox,
which omp delivers at its next tool round or as its next turn. No desktop
is involved and no import happens.

--harness agy starts an Antigravity CLI (agy) agent instead: "beekeeper
agy-run" driving "agy --input-format stream-json --output-format
stream-json --dangerously-skip-permissions" on --model (required; no
harness default, validated against agy models), in a transient user unit
beekeeper-agy-<id>, with its persistent FIFO inbox in beekeeper's state
folder and the worker rules and brief as its first message, confirming
the brief arrived in its own conversation. It is registered on the roster
as agy_<id> under <name> and shows in sessions and agents with harness
agy; agents wake queues a message in its inbox, which the PreInvocation
hook injects before its next model call inside a running turn or agy-run
sends as its next turn. No desktop is involved and no import happens.

--harness codex starts a Codex CLI agent instead: "beekeeper codex-run"
driving "codex exec --json" / "codex exec resume --json" in bypass mode on
--model (default: codex.model; with neither, the start is refused), in a
transient user unit beekeeper-codex-<id> with an isolated CODEX_HOME, its
FIFO inbox in beekeeper's state folder and the worker rules + brief as its
first message. It is registered on the roster as codex_<id> under <name>
and shows in sessions and agents with harness codex; agents wake queues a
message in its inbox, which codex-run delivers as the next turn via codex
exec resume.

--harness auto picks the first non-excluded (harness, model) pair in
capacity.choices whose provider has room (5-hour and 7-day windows below
their stops, weekly use within the choice's sevenDayMax worker cap and not
ahead of the even pace), the pick beekeeper capacity pick prints. Without
--harness a desk with capacity.choices starts on that pick too (unless
--model names a model none of them has: claude then), one without them on
claude. --exclude skips matching choices (by model, harness/model, provider,
or harness; repeatable). --review marks a review start, which may use
capacity.reviewHeadroom extra subscription sessions above the worker ceiling;
--review-of-self-hosted marks the review of a self-hosted model's
work, which takes no choice on a self-hosted provider.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var name, briefFile string
			switch len(args) {
			case 1:
				if strings.TrimSpace(nameFlag) == "" {
					return usageErr("accepts 2 arg(s), received 1")
				}
				name, briefFile = strings.TrimSpace(nameFlag), args[0]
			case 2:
				if strings.TrimSpace(nameFlag) != "" && strings.TrimSpace(nameFlag) != strings.TrimSpace(args[0]) {
					return usageErr("--name %q and positional <name> %q disagree", nameFlag, args[0])
				}
				name, briefFile = strings.TrimSpace(args[0]), args[1]
			}
			if name == "" {
				return usageErr("an agent needs a name")
			}
			harness = a.defaultStartHarness(harness, model)
			kind := startKind(review, ofSelfHosted)
			if host != "" && host != "local" {
				base, ok := harnessByName(harness)
				if harness == harnessAuto {
					base, ok = claudeHarness{}, true
				}
				if !ok {
					return validateStartHarness(harness, mode)
				}
				h, err := executionHarness(base, executionStartOptions{Host: host, Repository: repository, GitRef: gitRef, Harness: harness, Model: model, Mode: mode, Dir: dir, Task: task, Desktop: desktop, Builds: builds, Force: force, Review: review, ReviewOfSelfHosted: ofSelfHosted, Exclude: exclude}, briefFile)
				if err != nil {
					return err
				}
				sp := agentStart{name: name, task: task, model: model}
				if dryRun {
					return h.DryRun(cmd.Context(), a, sp, nil)
				}
				return h.Start(cmd.Context(), a, sp)
			}
			if repository != "" || gitRef != "" {
				return usageErr("--repo and --ref require an SSH --host")
			}
			if harness == harnessAuto {
				choices := a.liveCapacity().Choices
				if len(choices) == 0 {
					return usageErr("--harness auto: no capacity.choices configured")
				}
				if m := strings.TrimSpace(model); m != "" && !choicesHaveModel(choices, m) {
					return usageErr("--harness auto and --model %q: model is not in capacity.choices", m)
				}
			}
			resolvedMode, err := a.startMode(mode)
			if err != nil {
				return err
			}
			if resolvedMode == config.ModeBackground && desktop {
				return usageErr("--mode background and --desktop: a background session has no desktop turn")
			}
			if harness != harnessAuto {
				if err := validateStartHarness(harness, mode); err != nil {
					return err
				}
				if harness == harnessAgy && strings.TrimSpace(model) == "" && !dryRun {
					return usageErr("an agy agent needs its model: --model (agy models lists them)")
				}
			}
			// a sandboxed session's start reads its brief and gives the
			// agent its directory through the broker, within its own lists
			if err := a.sandboxPaths([]string{briefFile}, []string{dir}); err != nil {
				return err
			}
			brief, err := readBrief(briefFile)
			if err != nil {
				return err
			}
			// another desk's claim of the brief's issue refuses the start,
			// --force aside, before anything is judged or recorded
			if err := a.claimGate(cmd.Context(), name, briefIssue(brief), force, dryRun); err != nil {
				return err
			}
			// an omp agent whose provider's key is in the vault starts where
			// the vault session is: the host's broker
			if !dryRun && harness == omp.Harness && model != "" {
				// a bare id resolves once here; the broker and the start get
				// the selector omp lists
				if q := a.ompQualified(model); q != model {
					if err := cmd.Flags().Set("model", q); err != nil {
						return err
					}
					model = q
				}
			}
			if !dryRun && harness == omp.Harness && a.ompStartBrokered(model) {
				if err := a.preflightStartTarget(cmd.Context(), harness, model, dir); err != nil {
					return err
				}
				return a.agentsOnHost(cmd, []string{name, briefFile}, agentsBrokeredTimeout+time.Minute)
			}
			if task = strings.TrimSpace(task); task == "" {
				task = briefTask(brief)
			}
			gaps := pluginGaps(cmd.Context())
			// The dry run is judged by the same start gate a real start is,
			// and prints its verdict: the machine's guards — MemAvailable,
			// swap, load, the kind labs — weigh on it as on any start, only
			// the build slot is the dry run's to show, with --builds, as the
			// wait it would find, never joined. The gate itself never queues.
			// A verdict without room still refuses the dry run with exit 3
			// (--force aside), its words in the error.
			if dryRun {
				if builds {
					if err := a.dryRunSlot(); err != nil {
						return err
					}
				}
				over, v, err := a.startGateWithOptions(cmd.Context(), name, harness, model, false, force, exclude, kind)
				if err != nil {
					return err
				}
				if over == "" && v != nil && v.Room > 0 {
					if _, err := fmt.Fprintf(a.out, "%s\n", v.verdict()); err != nil {
						return err
					}
				}
				if harness == harnessAuto {
					ch, err := pickAutoChoice(v, force)
					if err != nil {
						return err
					}
					if err := validateStartHarness(ch.Harness, mode); err != nil {
						return err
					}
					if _, err := fmt.Fprintf(a.out, "choice: %s\n", ch.line()); err != nil {
						return err
					}
					harness = ch.Harness
					model = ch.Model
				}
				if over != "" {
					if _, err := fmt.Fprintf(a.out, "dry run over %s (--force)\n", over); err != nil {
						return err
					}
				}
				sp := agentStart{name: name, role: workerRole, brief: workerPrompt(taskPrompt(brief), gaps...), dir: dir, model: model, mode: mode}
				h, _ := harnessByName(harness)
				return h.DryRun(cmd.Context(), a, sp, gaps)
			}
			// The start gate: a verdict without room refuses the start
			// before anything is recorded; --force goes through on record.
			over, v, err := a.startGateWithOptions(cmd.Context(), name, harness, model, builds, force, exclude, kind)
			if err != nil {
				return err
			}
			if harness == harnessAuto {
				ch, err := pickAutoChoice(v, force)
				if err != nil {
					return err
				}
				if err := validateStartHarness(ch.Harness, mode); err != nil {
					return err
				}
				if ch.Harness == omp.Harness {
					// the pick names an omp model by its bare id: the
					// broker and the start get the selector omp lists
					ch.Model = a.ompQualified(ch.Model)
				}
				if err := a.preflightStartTarget(cmd.Context(), ch.Harness, ch.Model, dir); err != nil {
					return err
				}
				if ch.Harness == omp.Harness && a.ompStartBrokered(ch.Model) {
					if err := forwardBrokeredChoice(cmd, ch.Harness, ch.Model, ch.line()); err != nil {
						return err
					}
					return a.agentsOnHost(cmd, []string{name, briefFile}, agentsBrokeredTimeout+time.Minute)
				}
				harness = ch.Harness
				model = ch.Model
				choiceLine = ch.line()
			} else if err := a.preflightStartTarget(cmd.Context(), harness, model, dir); err != nil {
				return err
			}
			if choiceLine != "" {
				if _, err := fmt.Fprintf(a.out, "choice: %s\n", choiceLine); err != nil {
					return err
				}
			}
			if over != "" {
				by, err := a.caller()
				if err != nil {
					return err
				}
				if err := a.store.Record(event(by, verbStart, "%s: started over %s (--force)", name, over)); err != nil {
					return err
				}
				if _, err := fmt.Fprintf(a.out, "started over %s (--force)\n", over); err != nil {
					return err
				}
			}
			// Every worker gets the shipped rules ahead of its task, whatever
			// its harness.
			if note := gapsNote(gaps); note != "" {
				if _, err := fmt.Fprintln(a.out, note); err != nil {
					return err
				}
			}
			sp := agentStart{name: name, role: workerRole, brief: workerPrompt(taskPrompt(brief), gaps...), task: task, serves: briefIssue(brief), dir: dir, model: model, desktop: desktop, mode: mode}
			h, _ := harnessByName(harness)
			defer a.warnClaims(cmd.Context(), cmd.ErrOrStderr())
			return h.Start(cmd.Context(), a, sp)
		},
	}
	c.Flags().StringVar(&nameFlag, "name", "", "the session's name (alternative to the first positional argument)")
	c.Flags().StringVar(&model, "model", "", "the session's model (default: Claude Code's; omp: omp.model; agy: required; codex: codex.model; auto: the chosen model)")
	c.Flags().StringVar(&host, "host", "", "execution host selector; empty or local keeps local behavior")
	c.Flags().StringVar(&repository, "repo", "", "canonical owner/repository mapped on the SSH host")
	c.Flags().StringVar(&gitRef, "ref", "", "host-local source ref to pin in an isolated clone")
	c.Flags().StringVar(&dir, "dir", "", "the session's working directory (default: agents.dir, else the current one)")
	c.Flags().StringVar(&task, "task", "", "the task the roster shows it busy with (default: the brief's first line)")
	c.Flags().StringVar(&harness, "harness", "", "the agent harness: claude, omp, agy, codex, or auto (default: auto with capacity.choices, else claude)")
	c.Flags().StringArrayVar(&exclude, "exclude", nil, "exclude a choice from --harness auto (model, harness/model, provider, or harness; repeatable)")
	c.Flags().BoolVar(&review, reviewFlag, false, "a desk review start, using reserved review headroom")
	c.Flags().BoolVar(&ofSelfHosted, reviewOfSelfHostedFlag, false, "the review of a self-hosted model's work: no choice on a self-hosted provider")
	c.Flags().StringVar(&mode, "mode", "", "desktop (imported into Claude Desktop) or background (a claude --bg session with Remote Control); default: agents.mode")
	c.Flags().BoolVar(&desktop, "desktop", false, "the task needs desktop turns: import it past the desktop window's focus, as agents desktop does")
	c.Flags().BoolVar(&builds, buildsFlag, false, "the task builds, tests or lints: the start waits its turn for a build slot")
	c.Flags().BoolVar(&force, "force", false, "start although capacity says no start, or another desk claims the brief's issue; the event log records what it overrode")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print how the session would start (the profile applied and its first turn's command line) and start nothing")
	c.Flags().StringVar(&choiceLine, "choice", "", "the auto choice line forwarded to a brokered start")
	_ = c.Flags().MarkHidden("choice")
	return c
}

// defaultStartHarness is a start's harness: the one named, else auto (the
// pick of capacity.choices) on a desk with choices, unless model names a
// model none of them has, else claude.
func (a *app) defaultStartHarness(harness, model string) string {
	if harness = strings.TrimSpace(harness); harness != "" {
		return harness
	}
	choices := a.liveCapacity().Choices
	if m := strings.TrimSpace(model); len(choices) == 0 || m != "" && !choicesHaveModel(choices, m) {
		return claudeComm
	}
	return harnessAuto
}

func validateStartHarness(harness, mode string) error {
	if _, ok := harnessByName(harness); !ok {
		return usageErr("--harness %q: claude, omp, agy, codex, or auto", harness)
	}
	switch harness {
	case omp.Harness, harnessAgy:
		if strings.TrimSpace(mode) == config.ModeBackground {
			return usageErr("--mode background and --harness %s: an %s agent is no Claude Code background session", harness, harness)
		}
	case codex.Harness:
		if strings.TrimSpace(mode) == config.ModeBackground {
			return usageErr("--mode background and --harness %s: a %s agent is no Claude Code background session", harness, harness)
		}
	}
	return nil
}

func forwardBrokeredChoice(cmd *cobra.Command, harness, model string, choiceLine ...string) error {
	if err := cmd.Flags().Set("harness", harness); err != nil {
		return err
	}
	if err := cmd.Flags().Set("model", model); err != nil {
		return err
	}
	if len(choiceLine) > 0 && choiceLine[0] != "" && cmd.Flags().Lookup("choice") != nil {
		return cmd.Flags().Set("choice", choiceLine[0])
	}
	return nil
}

func (a *app) preflightStartTarget(ctx context.Context, harness, model, dir string) error {
	switch harness {
	case harnessAgy:
		resolvedDir, err := a.agentDir(dir, "")
		if err != nil {
			return err
		}
		if fi, err := os.Stat(resolvedDir); err != nil || !fi.IsDir() {
			return usageErr("--dir %s: not a directory", resolvedDir)
		}
		bin, err := exec.LookPath(agy.Comm)
		if err != nil {
			return err
		}
		_, err = agyModel(model, func() ([]string, error) { return agyModels(ctx, bin) })
		return err
	case omp.Harness:
		resolvedDir, err := filepath.Abs(dir)
		if err != nil {
			return err
		}
		if fi, err := os.Stat(resolvedDir); err != nil || !fi.IsDir() {
			return usageErr("--dir %s: not a directory", resolvedDir)
		}
		m, err := ompModel(model, a.cfg.Omp.Model, func() ([]string, error) { return a.ompListed(ctx) })
		if err != nil {
			return err
		}
		_, err = a.ompCredentials(m)
		return err
	case codex.Harness:
		resolvedDir, err := a.agentDir(dir, "")
		if err != nil {
			return err
		}
		if fi, err := os.Stat(resolvedDir); err != nil || !fi.IsDir() {
			return usageErr("--dir %s: not a directory", resolvedDir)
		}
		if _, err := a.codexModel(model); err != nil {
			return err
		}
		_, err = exec.LookPath(codex.Comm)
		return err
	default:
		resolvedDir, err := a.agentDir(dir, "")
		if err != nil {
			return err
		}
		if fi, err := os.Stat(resolvedDir); err != nil || !fi.IsDir() {
			return usageErr("--dir %s: not a directory", resolvedDir)
		}
		_, err = exec.LookPath(claudeComm)
		return err
	}
}

func pickAutoChoice(v *capacityView, force bool) (*choiceView, error) {
	if v != nil && v.Stopped != "" {
		return nil, refused("%s", v.verdict())
	}
	if v != nil && v.Choice != nil && v.Choice.Harness != "" && v.Choice.Model != "" {
		return v.Choice, nil
	}
	if force && v != nil && v.Choice != nil {
		for _, cand := range v.Choice.Candidates {
			// --force overrides capacity, never an exclusion or the one
			// restriction on the review of a self-hosted model's work.
			if cand.Reason != "excluded" && cand.Reason != selfHostedReviewer {
				return &choiceView{
					Harness:  cand.Harness,
					Model:    cand.Model,
					Provider: cand.Provider,
					Bucket:   cand.Bucket,
					Review:   v.Choice.Review,
				}, nil
			}
		}
		return nil, refused("no start: all capacity.choices excluded or barred from the review of a self-hosted model's work")
	}
	if v != nil && len(v.Blocks) > 0 {
		return nil, refused("no start: %s", strings.Join(v.Blocks, "; "))
	}
	return nil, refused("no start: no choice in capacity.choices has room")
}

// agentStart is a session `agents start` or `agents handover` starts.
type agentStart struct {
	name, brief, dir, model string
	// role is the role whose profile the session gets (workerRole, or a
	// relayed role's name); empty: none.
	role string
	// task is the roster's task unless the entry taken over holds one.
	task string
	// serves is the issue the task works (briefIssue), recorded as the
	// session's serve record with the task; empty: none.
	serves string
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
	// mode is the start's mode (--mode): config.ModeDesktop or
	// config.ModeBackground; empty: agents.mode.
	mode string
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
	// want is the model the start chose (--model, as its first reply names
	// it), which its turns keep; empty: none was chosen.
	want string
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
	// handed are the leases and grants a hand-over gave it from the session
	// it replaces (handClaims).
	handed []string
	// background says it runs as a background session (startBackground):
	// no desktop fields are set.
	background bool
}

// startAgent records and registers the session, starts its first turn in a
// transient user unit and imports it into the desktop once the transcript
// holds its first reply, which carries its model.
func (a *app) startAgent(ctx context.Context, sp agentStart) (startedAgent, error) {
	st, err := a.store.Read()
	if err != nil {
		return startedAgent{}, err
	}
	if err := a.refuseStopped(st, "starting "+sp.name); err != nil {
		return startedAgent{}, err
	}
	dir, err := a.agentDir(sp.dir, sp.role)
	if err != nil {
		return startedAgent{}, err
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return startedAgent{}, usageErr("--dir %s: not a directory", dir)
	}
	mode, err := a.startMode(sp.mode)
	if err != nil {
		return startedAgent{}, err
	}
	sp.model = cmp.Or(sp.model, a.cfg.Agents.Model)
	// A profiled session's task runs as a headless turn under its profile:
	// the desktop spawns its own CLI for a desktop turn, with the person's
	// whole tool surface, and no seed turn is needed before it.
	profile := profileName(a.cfg, sp.role)
	var profiled []string
	if profile != "" {
		if profiled, err = a.profileFlags(ctx, profile); err != nil {
			return startedAgent{}, err
		}
		sp.headless = true
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
	if mode == config.ModeBackground {
		return a.startBackground(ctx, sp, dir, profiled, by, live)
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
	id := sp.id
	if id == "" {
		id = uuid.NewString()
	}
	sp.brief = fillScratchpad(sp.brief, dir, id)
	s := state.Start{Party: state.Party{Session: id, HostSession: "local_" + id, Name: sp.name}, Mode: state.ModeBypass, Dir: dir, By: by, At: a.now.UTC(), Model: sp.model}
	var reg registration
	var handed []string
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		var err error
		reg, err = recordStart(st, s, sp.task, sp.serves, live)
		if err != nil {
			return nil, err
		}
		if sp.desktop {
			st.Agents[len(st.Agents)-1].DesktopTurn = s.At
		}
		evs := []state.Event{event(by, verbStart, "%s: session %s in %s, bypassPermissions, busy with %q", sp.name, id, dir, reg.task)}
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
		return startedAgent{}, err
	}
	unit := "beekeeper-agent-" + id[:8]
	if !sp.headless {
		sa, err := a.startVisible(ctx, d, sp, bin, unit, dir, startedAgent{id: id, unit: unit, dir: dir, task: reg.task, handed: handed}, by)
		if err != nil {
			return sa, a.undelivered(by, s.Party, err)
		}
		return sa, nil
	}
	self, err := os.Executable()
	if err != nil {
		return startedAgent{}, err
	}
	if err := launch(unit, dir, a.explicitConfig(), reopenStopPost(self, id), headlessStartArgv(bin, id, sp.name, sp.model, sp.brief, profiled...)); err != nil {
		return startedAgent{}, fmt.Errorf("starting %s: %w (the start stays recorded; beekeeper agents remove %q takes it off the roster)", sp.name, err, sp.name)
	}
	if err := awaitReply(ctx, a.cfg.Claude.ProjectsDir, id, func() bool { return unitEnded(ctx, unit) }, replyQuiet, replyWait); err != nil {
		return startedAgent{}, fmt.Errorf("%w, not imported into the desktop: journalctl --user -u %s", err, unit)
	}
	a.resolveStartModel(id)
	var follow string
	if sp.replaces != nil {
		follow = sp.replaces.HostSession
	}
	sa := startedAgent{id: id, unit: unit, dir: dir, task: reg.task, handed: handed, want: a.chosenModel(id)}
	d.urgent = a.asksDesktop(id)
	if profile != "" {
		sa.turn = profiledTurn(profile)
		_ = a.store.Record(event(by, verbStart, "%s: %s", sp.name, sa.turn))
		// its row shows from its first turn, as a desktop turn's does
		d.urgent = func() bool { return true }
	}
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
		return []state.Event{event(by, verbStart, "%s: task not delivered: %v", p.Name, why)}, nil
	})
	if strings.HasPrefix(p.HostSession, agy.HostPrefix) {
		return fmt.Errorf("%s: task not delivered: %w (start it again with agents start --harness agy, or beekeeper agents remove %q takes it off the roster)",
			p.Name, why, p.Name)
	}
	if strings.HasPrefix(p.HostSession, codex.HostPrefix) {
		return fmt.Errorf("%s: task not delivered: %w (start it again with agents start --harness codex, or beekeeper agents remove %q takes it off the roster)",
			p.Name, why, p.Name)
	}
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
	a.resolveStartModel(id)
	sa.want = a.chosenModel(id)
	// Until its task's turn runs, the session has no CLI and maybe no row:
	// a wake meanwhile would resume it headless beside the CLI the import
	// warms.
	a.importPending(id, d.turnWait()+awayPoll+focusWait+2*twinWait)
	defer a.importEnded(id)
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
			_ = a.store.Record(event(by, verbStart, "%s: the desktop did not import it (%v)", sp.name, err))
		}
	}
	w := wakeTarget{name: sp.name, id: id, host: "local_" + id, dir: dir, mode: state.ModeBypass, model: sa.want}
	line, err := a.turnInDesktop(ctx, w, taskTurn, twinWait)
	if err == nil {
		sa.turn = "its task runs as a desktop turn: " + line
		_ = a.store.Record(event(by, verbStart, "%s: %s", sp.name, sa.turn))
		return sa, nil
	}
	sa.turn = fmt.Sprintf("the desktop runs no turn of it (%v): its task runs headless", err)
	if errors.As(err, new(modelNotHeld)) {
		sa.turn += " on " + w.model
	}
	_ = a.store.Record(event(by, verbStart, "%s: %s", sp.name, sa.turn))
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
	err := whileFrozen(ctx, a.store, sessionUnits(ctx, id, false), func() error {
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
// ending) has no writer and is not frozen. The units freeze while no process
// holds store's lock, so none of theirs is frozen holding it (its session
// start's hook for one) and starves every other command for the import.
func whileFrozen(ctx context.Context, store state.Store, units []string, fn func() error) error {
	var frozen []string
	thaw := func(err error) error {
		for _, u := range frozen {
			if terr := plat.Launcher.Thaw(context.WithoutCancel(ctx), u); terr != nil {
				err = errors.Join(err, fmt.Errorf("thawing %s: %w (systemctl --user thaw %s resumes its turn)", u, terr, u))
			}
		}
		return err
	}
	freeze := func() error {
		for _, u := range units {
			if err := plat.Launcher.Freeze(ctx, u); err != nil {
				if plat.Launcher.State(ctx, u) != "active" {
					continue
				}
				return fmt.Errorf("freezing %s for the import: %w", u, err)
			}
			frozen = append(frozen, u)
		}
		return nil
	}
	if err := store.Exclusive(freeze); err != nil {
		return thaw(err)
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

// modelLine says which model the session's desktop turns run on: model, the
// one the desktop recorded for host, and want, the one it was started on
// (none when empty).
func modelLine(model, want, host string) string {
	switch {
	case want != "" && !sameModel(model, want):
		return fmt.Sprintf("the model %s could not be set: the desktop records %s for its desktop turns, so they run headless on %s until a session other than it calls %s with session_id %q and model %q",
			want, cmp.Or(model, "no model (its default)"), want, setModelTool, host, want)
	case model == "":
		return "the desktop recorded no model: its desktop turns run on the desktop's default model"
	}
	return "its desktop turns run on " + model
}

// resolveStartModel records the model session id's first reply ran on as
// the model its start chose, when it chose one: --model can be an alias,
// the reply names the model's id.
func (a *app) resolveStartModel(id string) {
	path := transcriptOf(a.cfg, id)
	if path == "" {
		return
	}
	model, _ := claude.Model(path)
	if model == "" {
		return
	}
	_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
		for i := range st.Starts {
			if s := &st.Starts[i]; s.Session == id && s.Model != "" {
				s.Model = model
			}
		}
		for i := range st.Agents {
			if ag := &st.Agents[i]; ag.Session == id && ag.Model != "" {
				ag.Model = model
			}
		}
		return nil, nil
	})
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
// returns it; empty when there was none to go back to, or it was host,
// follow or a superseded session's (supersededHost), whose CLI a show would
// warm again. A running desktop gets the link only once d takes it, waiting up
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
	if prev == "" || prev == host || prev == follow || a.supersededHost(prev) || !awaitFocus(ctx, a.cfg.Claude.DesktopLog, host, focusWait) {
		return "", nil
	}
	if err := plat.Opener.Open(ctx, continueURL(prev), true); err != nil {
		return "", fmt.Errorf("showing %s again: %w", prev, err)
	}
	return prev, nil
}

// supersededHost reports whether the desktop session host is one beekeeper
// started whose work ended: a relieved role run's or a hand-over's
// predecessor's (superseded). The desktop warms the CLI of the session its
// window shows, which the watch stops as revived; a window sent back to it
// after each show would warm it again on every start, reopen and wake.
func (a *app) supersededHost(host string) bool {
	st, err := a.store.Read()
	if err != nil {
		return false
	}
	i := slices.IndexFunc(st.Starts, func(x state.Start) bool {
		return x.HostSession == host || x.Session != "" && "local_"+x.Session == host
	})
	if i < 0 {
		return false
	}
	by, _ := superseded(st, st.Starts[i].Party, time.Now())
	return by != ""
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
	if !a.desktopMode() {
		_, err := fmt.Fprintf(a.out, "reopen: %s left closed: agents.mode is %s\n", id, a.cfg.Agents.Mode)
		return err
	}
	if s := a.readStop(); s != nil {
		_, err := fmt.Fprintf(a.out, "reopen: %s left closed: %s\n", id, a.stopText(s))
		return err
	}
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
	if !a.desktopMode() {
		_, err := fmt.Fprintf(a.out, "reopen: %s left closed: agents.mode is %s\n", id, a.cfg.Agents.Mode)
		return err
	}
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	if st.Stop != nil {
		_, err := fmt.Fprintf(a.out, "reopen: %s left closed: %s\n", id, a.stopText(st.Stop))
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
	// A reopen an install restarted decided that before.
	if os.Getenv(restartedEnv) == "" {
		if resumed, err := a.resumeOnWait(ctx, id, turn); resumed || err != nil {
			if err != nil {
				return a.reopenMissed(name, err)
			}
			return nil
		}
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
	err = a.awaitRestartable(wctx, d, a.importWaits(id, name, reopenAwayWait))
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
	// started meanwhile, or any other headless turn of the session, is
	// looked for right before it.
	ready := func() error {
		if u := wakeRunning(ctx, id); u != "" {
			return wakeStarted(u)
		}
		return noHeadlessTurn(id)
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

// awaitRestartable waits for the person to leave the desktop's window, as
// a reopen does before its show, and restarts the reopen on the installed
// binary when an install asks meanwhile: the wait is all it did so far, and
// the restarted reopen waits again. A binary that does not start leaves
// the wait to go on under this one.
func (a *app) awaitRestartable(ctx context.Context, d desk, onHeld func(error)) error {
	rw, poll := newRestartWatch(a.cfg.StateDir), reopenPoll
	for {
		actx, cancel := context.WithCancelCause(ctx)
		done := make(chan struct{})
		go func() {
			tick := time.NewTicker(poll)
			defer tick.Stop()
			for {
				select {
				case <-done:
					return
				case <-tick.C:
					if req := rw.due(); req != nil {
						cancel(restartCause{req})
						return
					}
				}
			}
		}()
		err := d.await(actx, reopenAwayWait, onHeld)
		close(done)
		cancel(nil)
		r, ok := errors.AsType[restartCause](context.Cause(actx))
		if !ok || ctx.Err() != nil {
			return err
		}
		_, _ = fmt.Fprintf(a.out, "reopen: %s asked this reopen onto the installed %s; re-executing it\n", r.req.Binary, project.Name)
		if err := rw.exec(r.req); err != nil {
			_, _ = fmt.Fprintf(a.out, "reopen: the installed %s does not start (%v): waiting on under %s\n", r.req.Binary, err, project.Version())
		}
	}
}

// restartCause ends a reopen's wait for an install's restart request.
type restartCause struct{ req *restartRequest }

func (restartCause) Error() string { return errRestart.Error() }

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
		if i < 0 || st.Agents[i].Execution != nil {
			return "", false
		}
		return st.Agents[i].Name, true
	}
	if !slices.ContainsFunc(st.Starts, func(s state.Start) bool { return s.Execution == nil && !s.Background && s.Session == id }) {
		return "", false
	}
	p := state.Party{Session: id}
	i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(p) })
	if i < 0 || st.Agents[i].Execution != nil {
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

// handClaims gives the leases from holds and the grants queued for it to
// to, in their places, so a hand-over's follow-up holds and claims what
// its predecessor did. It returns them as "<resource> held" and
// "<resource> granted".
func handClaims(st *state.State, dir lease.Dir, from, to state.Party) ([]string, error) {
	holders, err := dir.List()
	if err != nil {
		return nil, err
	}
	var handed []string
	for _, h := range holders {
		if !h.Party().Is(from) {
			continue
		}
		h.Session, h.HostSession, h.Name = to.Session, to.HostSession, to.Name
		if err := dir.Hand(h); err != nil {
			return handed, fmt.Errorf("handing %s to %s: %w", h.Env, to.Name, err)
		}
		handed = append(handed, h.Env+" held")
	}
	for i := range st.Grants {
		if g := &st.Grants[i]; g.To.Is(from) {
			g.To = to
			handed = append(handed, g.Resource+" granted")
		}
	}
	return handed, nil
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

// verbStart logs a start and what it overrode.
const verbStart = "agents.start"

func resumeURL(id string) string { return "claude://resume?session=" + id }

// recordStart records s as one of beekeeper's starts, forgetting those older
// than startsKept, and registers it on the roster under its name, busy with
// task unless it takes over a stopped entry's open task. A task of its own
// serving an issue (owner/repo#n, briefIssue) is the session's serve record
// from the start, so board next never offers that item again.
func recordStart(st *state.State, s state.Start, task, serves string, live func(state.Party) bool) (registration, error) {
	reg, err := registerAgent(st, s.Party, live, s.At)
	if err != nil {
		return registration{}, err
	}
	ag := &st.Agents[len(st.Agents)-1]
	ag.Harness, ag.Model = s.Harness, s.Model
	if reg.task == "" && task != "" {
		reg.task, reg.assignedAt = task, s.At
		ag.Task, ag.AssignedAt = task, s.At
		if serves != "" {
			st.Records = slices.DeleteFunc(st.Records, func(o state.Record) bool { return o.Session.Is(s.Party) })
			st.Records = append(st.Records, state.Record{Session: s.Party, Issue: serves, By: s.By, At: s.At})
		}
	}
	// A released execution never ran: nothing reconciles it again.
	st.Starts = slices.DeleteFunc(st.Starts, func(x state.Start) bool {
		return (x.Execution == nil || !x.Execution.Released.IsZero()) && s.At.Sub(x.At) > startsKept
	})
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

// scratchpadMark stands in the worker rules for the started session's
// scratchpad, which agents start fills in by absolute path once it knows
// the session's id.
const scratchpadMark = "<scratchpad>"

// fillScratchpad names, in a first prompt, the scratchpad of the session id
// started in dir by absolute path.
func fillScratchpad(prompt, dir, id string) string {
	return strings.ReplaceAll(prompt, scratchpadMark, guard.Scratchpad(dir, id))
}

// workerPrompt is a worker's first prompt: the worker rules beekeeper
// ships with its role skills, versioned, ahead of prompt, with the topic
// skills in inline (pluginGaps) after them. A brief carries only its task.
func workerPrompt(prompt string, inline ...string) string {
	rules := plugin.WorkerRules()
	if len(inline) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "%s\n\nThe installed beekeeper plugin does not serve %s as this beekeeper ships them, so they follow here.",
			rules, topicSkills(inline))
		for _, t := range inline {
			b.WriteString("\n\n" + plugin.Skill(t))
		}
		rules = b.String()
	}
	return fmt.Sprintf("Beekeeper %s gives every worker it starts these rules; they hold for the whole task.\n\n%s\n\n%s",
		project.Version(), rules, prompt)
}

// topicSkills names topics as the skills a worker loads.
func topicSkills(topics []string) string {
	names := make([]string, len(topics))
	for i, t := range topics {
		names[i] = "beekeeper:" + t
	}
	return strings.Join(names, ", ")
}

// pluginGaps are the worker rules' topic skills a started worker cannot
// load as this binary ships them: no enabled beekeeper plugin installed
// carries the same SKILL.md (none is installed, or it is out of date). The
// first prompt carries those itself, so no rule is lost to a stale plugin.
func pluginGaps(ctx context.Context) []string {
	ps, _ := listPlugins(ctx) // none listed: every topic goes inline
	var gaps []string
	for _, t := range plugin.Topics {
		if !slices.ContainsFunc(ps, func(p installedPlugin) bool { return servesSkill(p, t) }) {
			gaps = append(gaps, t)
		}
	}
	return gaps
}

// pluginName is the name of the plugin beekeeper ships, in any marketplace.
const pluginName = "beekeeper"

// servesSkill says whether p is an enabled beekeeper plugin whose skill
// name is the one this binary embeds.
func servesSkill(p installedPlugin, name string) bool {
	if id, _, _ := strings.Cut(p.ID, "@"); id != pluginName || !p.Enabled && !p.ProjectEnabled || p.InstallPath == "" {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(p.InstallPath, plugin.SkillsDir, name, "SKILL.md")) //nolint:gosec // Claude Code's own plugin cache
	return err == nil && bytes.Equal(raw, plugin.SkillFile(name))
}

// gapsNote is what start says about the topics pluginGaps put inline, ""
// when there are none.
func gapsNote(gaps []string) string {
	if len(gaps) == 0 {
		return ""
	}
	return fmt.Sprintf("the installed beekeeper plugin does not serve %s as %s ships them: the first prompt carries them (claude plugin marketplace update beekeeper && claude plugin update beekeeper@beekeeper)",
		topicSkills(gaps), project.Version())
}

// briefURL is an issue's or a pull request's URL in a brief.
var briefURL = regexp.MustCompile(`https://github\.com/([\w.-]+/[\w.-]+)/(?:issues|pull)/(\d+)`)

// briefIssue is the issue a brief works, owner/repo#n: the first issue or
// pull request its text names by URL; empty when it names none.
func briefIssue(brief string) string {
	m := briefURL.FindStringSubmatch(brief)
	if m == nil {
		return ""
	}
	return m[1] + "#" + m[2]
}

// briefTask is the roster's task for a brief: its first line without a
// Markdown heading's hashes.
func briefTask(brief string) string {
	line, _, _ := strings.Cut(brief, "\n")
	return truncate(strings.TrimSpace(strings.TrimLeft(line, "# ")), 80)
}

// headlessStartArgv is the first turn of a headless start: in bypass with the
// CLI's own Chrome connection, as every bypass wake turn (wakeArgv), and the
// session's profile flags. The bypass is the turn's scope, so its Chrome tools
// are not narrowed to browse's.
func headlessStartArgv(bin, id, name, model, brief string, profile ...string) []string {
	return agentArgv(bin, id, name, model, brief, append([]string{chromeFlag}, profile...)...)
}

// profiledTurn says how a profiled start runs its task.
func profiledTurn(profile string) string {
	return "its task runs as a headless turn under profile " + profile
}

// dryRunStart prints how agents start would start sp, and starts nothing:
// the profile applied, the first turn's command line with the prompt left
// out, then the first prompt, sp.brief, with its size and what start says
// about the topics gaps put inline.
func (a *app) dryRunStart(ctx context.Context, sp agentStart, gaps []string) error {
	if err := a.dryRunTurn(ctx, sp); err != nil {
		return err
	}
	if note := gapsNote(gaps); note != "" {
		if _, err := fmt.Fprintln(a.out, note); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(a.out, "first prompt: %d bytes\n%s\n", len(sp.brief), sp.brief)
	return err
}

func (a *app) dryRunOmpStart(sp agentStart, gaps []string) error {
	dir, err := a.agentDir(sp.dir, sp.role)
	if err != nil {
		return err
	}
	sp.brief = projectPrompt(dir, sp.brief)
	model := cmp.Or(sp.model, a.cfg.Omp.Model, "<model>")
	argv := ompArgv(omp.Comm, model)
	if _, err := fmt.Fprintf(a.out, "dry run: %s would start in %s as an omp agent on %s, yolo, no desktop import\nstart: %s\n",
		sp.name, dir, model, strings.Join(argv, " ")); err != nil {
		return err
	}
	if note := gapsNote(gaps); note != "" {
		if _, err := fmt.Fprintln(a.out, note); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(a.out, "first prompt: %d bytes\n%s\n", len(sp.brief), sp.brief)
	return err
}

// dryRunTurn prints the profile and the first turn's command line of sp.
func (a *app) dryRunTurn(ctx context.Context, sp agentStart) error {
	dir, err := a.agentDir(sp.dir, sp.role)
	if err != nil {
		return err
	}
	const id = "<session id>"
	mode, err := a.startMode(sp.mode)
	if err != nil {
		return err
	}
	sp.model = cmp.Or(sp.model, a.cfg.Agents.Model)
	profile := profileName(a.cfg, sp.role)
	if mode == config.ModeBackground {
		var flags []string
		if profile != "" {
			if flags, err = a.profileFlags(ctx, profile); err != nil {
				return err
			}
		}
		argv := bgStartArgv("claude", sp.name, sp.model, "<rules and brief>", flags...)
		_, err = fmt.Fprintf(a.out, "dry run: %s would start in %s as a background session with Remote Control under %s, none of the caller's CLAUDE* variables, no desktop import\nstart: %s\n",
			sp.name, dir, cmp.Or(profile, "no profile"), strings.Join(argv, " "))
		return err
	}
	if profile == "" {
		argv := agentArgv("claude", id, sp.name, sp.model, "<rules and brief>\n\n"+seedNote, toolsFlag, "", strictMCPConfigFlag)
		_, err = fmt.Fprintf(a.out, "dry run: %s would start in %s under no profile: a seed turn without tools, then its task as a desktop turn with the person's whole tool surface\nseed turn: %s\n",
			sp.name, dir, strings.Join(argv, " "))
		return err
	}
	flags, err := a.profileFlags(ctx, profile)
	if err != nil {
		return err
	}
	argv := headlessStartArgv("claude", id, sp.name, sp.model, "<rules and brief>", flags...)
	_, err = fmt.Fprintf(a.out, "dry run: %s would start in %s under profile %s: %s, no seed turn\nfirst turn: %s\n",
		sp.name, dir, profile, profiledTurn(profile), strings.Join(argv, " "))
	return err
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
		// Its beekeeper commands run the build that started it, which
		// the PATH no longer tells from the installed one.
		u.Env = append(u.Env, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), devBuildEnv+"=1")
	}
	return plat.Launcher.Start(u)
}

// devBuildEnv marks the environment of a unit a development build or a
// downloaded release candidate started: its PATH leads to that build.
const devBuildEnv = "BEEKEEPER_DEV_BUILD"

// devBuild is the folder of the running beekeeper when it is not the one on
// PATH: a development build or a downloaded release candidate, which the
// sessions it starts run too.
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
