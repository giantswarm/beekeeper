package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/peer"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) agentWakeCmd() *cobra.Command {
	var mode string
	c := &cobra.Command{
		Use:   agentWakeName + " <agent> <message>",
		Short: "Message an agent, starting its desktop CLI when none runs (headless where the desktop cannot)",
		Long: `wake delivers a message to a registered agent without Claude Desktop's
route, whose cap pauses a session's messages to local_ ids after ten sends
since its person last typed in it.

An SSH execution receives a durable ordered envelope. Success requires a
harness acceptance event containing its message ID. A pending or uncertain
delivery exits nonzero; agents messages shows its receipt and agents hosts
sync reconciles it without creating another process. Reuse --request-id for
an identical retry; use a new ID for another identical message.

An omp agent (agents start --harness omp) or an agy agent (agents start
--harness agy) gets the message in its inbox, at its next tool round / model
call or as its next turn; one whose process ended is refused: nothing resumes
it.

A session whose CLI runs gets the message by name, as SendMessage by name
does: it queues and runs at the session's next tool call or as its next turn.
A background session (agents start --mode background, or, while agents.mode
is background, a session without a desktop one that claude agents --json
--all lists as a background session; never one run in a terminal) whose
CLI does not run is continued by "claude --bg --resume <id> <message>", with none of
the caller's CLAUDE* variables, and keeps its Remote Control, mode, name
and model; no desktop route, no claude:// link and no reopen unit.
A session whose import is pending (agents shows it importing) is refused: a
headless turn would run beside the CLI the import warms.
A session with no running CLI (after a reboot, a crash, the desktop's idle
drop) and a row in the running desktop gets the message as a desktop turn:
a steward's send through the desktop's session messaging starts its desktop
CLI with it, under the desktop's cap (beekeeper ends one of its own finished
or parked workers' CLIs first). Only where the desktop cannot run the turn
(it does not run, the session has no row, no steward took the send) wake
says why, and the session is resumed from the command line: "claude -p --resume <id>" with the
message as its turn, in the session's directory, permission mode (a
start's bypassPermissions, else the mode the desktop recorded for it;
--permission-mode overrides) and the model the desktop recorded for it, in a transient user unit beekeeper-wake-<id>-<wake>
(its output in journalctl --user -u <unit>) the caller does not take down.
The turn continues the session's own transcript. Once it ends, the session
is shown in the desktop for a moment, which warms the desktop's CLI of it,
so later messages by name reach it again. While the headless turn runs,
beekeeper agents shows it "wake turn running", and the PreToolUse hook sends a
desktop send to its local_ id by name to it, so no second copy starts.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			msg := strings.TrimSpace(args[1])
			if msg == "" {
				return usageErr("an empty message wakes nothing")
			}
			by, err := a.caller()
			if err != nil {
				return err
			}
			return a.wakeAgent(cmd.Context(), by, args[0], msg, mode)
		},
	}
	c.Flags().StringVar(&mode, "permission-mode", "", "the resumed turn's permission mode (default: the start's, else the desktop record's)")
	return c
}

// wakeTarget is the session a wake reaches: the CLI session id it resumes,
// its directory and permission mode, and its desktop id.
type wakeTarget struct {
	name, id, host, dir, mode string
	// model is the model the desktop recorded for the session's turns;
	// empty: Claude Code's default.
	model string
	// profile is the profile a headless turn of one of beekeeper's starts
	// runs under (its role's); empty: none.
	profile string
}

// wakeAgent delivers msg from by to the registered agent q (wake).
func (a *app) wakeAgent(ctx context.Context, by state.Party, q, msg, mode string) (err error) {
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	if err := a.refuseStopped(st, "waking "+q); err != nil {
		return err
	}
	i, err := findAgent(st, q)
	if err != nil {
		return err
	}
	ag := st.Agents[i]
	// Reserve the local entry before delivering a wake so retirement cannot
	// race a turn's startup. A failed delivery restores the previous age.
	if ag.Execution == nil {
		err = a.store.Update(func(st *state.State) ([]state.Event, error) {
			for i := range st.Agents {
				if st.Agents[i].Is(ag.Party) {
					st.Agents[i].ActivityAt = a.now.UTC()
					return nil, nil
				}
			}
			return nil, refused("%s left the roster before its wake", ag.Name)
		})
		if err != nil {
			return err
		}
		defer func() {
			if err != nil {
				_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
					for i := range st.Agents {
						if st.Agents[i].Is(ag.Party) && st.Agents[i].ActivityAt.Equal(a.now) {
							st.Agents[i].ActivityAt = ag.ActivityAt
						}
					}
					return nil, nil
				})
			}
		}()
	}
	if ag.Execution != nil {
		return a.enqueueExecutionWake(ctx, by, ag, msg, nil)
	}
	if st.Supervisor != nil && st.Supervisor.Is(by) {
		// The supervisor's `yours <resource>` is the grant, as in a
		// SendMessage; the record follows the word.
		if named := guard.Yours(msg, a.cfg.Leasable()); len(named) > 0 {
			sessions, _, err := a.sessions()
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(a.out, "wake: "+a.yoursGrants(sessions, st.Supervisor.Party, ag.Name, named)); err != nil {
				return err
			}
		}
		// A wake carrying the next task to an agent that reported its
		// last idle restores its task record, as a SendMessage does.
		line, err := a.retaskAgent(by, rosterIndex(ag.Party), msg)
		if err != nil {
			return err
		}
		if line != "" {
			if _, err := fmt.Fprintln(a.out, "wake: "+line); err != nil {
				return err
			}
		}
	}
	if ag.Undelivered != "" {
		// A delivered turn is the task's start agents start did not make.
		defer func() {
			if err == nil {
				a.markDelivered(ag.Party)
			}
		}()
	}
	if h, id, ok := harnessForHost(ag.HostSession); ok {
		return h.Wake(a, by, ag, id, msg)
	}
	// A stop of the parked background session in flight ends before the
	// wake looks whether its CLI runs (stopParkedSession).
	release, err := a.lockForWake(ctx, st, ag.Party)
	if err != nil {
		return err
	}
	defer release()
	sessions, _, err := a.sessions()
	if err != nil {
		return err
	}
	w, err := resolveWake(a.cfg, st, ag)
	if err != nil {
		return err
	}
	if mode != "" {
		w.mode = mode
	}
	if s, ok := wakeLive(sessions, ag.Party, w.id); ok {
		name, err := uniqueName(sessions, s)
		if err != nil {
			return err
		}
		if _, err := (peer.Sender{Dir: a.cfg.StateDir}).Send(ctx, name, msg); err != nil {
			return fmt.Errorf("waking %s: %w", ag.Name, err)
		}
		_ = a.store.Record(event(by, "agents.wake", "%s: sent by name to its running CLI %d", ag.Name, s.PID))
		_, err = fmt.Fprintf(a.out, "wake: %s runs (CLI %d): sent by name, it runs the message at its next tool call or as its next turn\n", ag.Name, s.PID)
		return err
	}
	if u := wakeRunning(ctx, w.id); u != "" {
		return refused("%s: its wake turn %s is starting and not yet reachable by name: send again in a minute", ag.Name, u)
	}
	if a.backgroundAgent(ctx, st, ag.Party) {
		return a.wakeBackground(ctx, by, w, msg)
	}
	if ag.Import.Pending(a.now) {
		return a.importRefusal(by, ag)
	}
	if line, err := a.turnInDesktop(ctx, w, msg, 0); err == nil {
		_ = a.store.Record(event(by, "agents.wake", "%s: %s", ag.Name, line))
		_, err = fmt.Fprintf(a.out, "wake: %s: %s\n", ag.Name, line)
		return err
	} else if _, err := fmt.Fprintf(a.out, "wake: %s gets no desktop turn (%v): its turn runs headless\n", ag.Name, err); err != nil {
		return err
	}
	return a.resumeTurn(ctx, by, w, msg)
}

// importRefusal refuses the wake of ag, whose import is pending: the CLI the
// import warms would run beside a headless turn of the wake.
func (a *app) importRefusal(by state.Party, ag state.Agent) error {
	_ = a.store.Record(event(by, "agents.wake", "%s: refused, its import is pending", ag.Name))
	return refused("%s: %s; a headless turn would run beside the CLI the import warms, so wake starts none: send again once beekeeper agents shows it live",
		ag.Name, importStatus(ag, a.now))
}

// markDelivered clears the roster's "task not delivered" of p.
func (a *app) markDelivered(p state.Party) {
	_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
		if i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(p) }); i >= 0 {
			st.Agents[i].Undelivered = ""
		}
		return nil, nil
	})
}

// resumeTurn resumes the session w headless with msg as its turn, in a
// transient user unit whose stop-post starts the reopen that shows it in the
// desktop for a moment.
func (a *app) resumeTurn(ctx context.Context, by state.Party, w wakeTarget, msg string) error {
	if pid, err := a.toDesktopCLI(ctx, w.id, msg); pid != 0 || err != nil {
		if err != nil {
			return fmt.Errorf("waking %s: %w", w.name, err)
		}
		_ = a.store.Record(event(by, "agents.wake", "%s: sent to its desktop CLI %d, which started meanwhile, instead of resuming it headless", w.name, pid))
		_, err = fmt.Fprintf(a.out, "wake: %s: its desktop CLI %d runs: sent the message to it instead of resuming the session headless beside it\n", w.name, pid)
		return err
	}
	unit := wakeUnit(w.id)
	bin, err := exec.LookPath("claude")
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	var stopPost []string
	if w.host != "" {
		stopPost = reopenStopPost(self, w.host)
	}
	argv, err := a.profiledWakeArgv(ctx, bin, w, msg)
	if err != nil {
		return fmt.Errorf("waking %s: %w", w.name, err)
	}
	if err := launch(unit, w.dir, a.explicitConfig(), stopPost, argv); err != nil {
		return fmt.Errorf("waking %s: %w", w.name, err)
	}
	_ = a.store.Record(event(by, "agents.wake", "%s: resumed session %s headless in %s, %s%s (%s)", w.name, w.id, w.dir, w.mode, underProfile(w.profile), unit))
	_, err = fmt.Fprintf(a.out, "wake: %s had no running CLI: resumed session %s headless in %s, %s, with the message as its turn (journalctl --user -u %s)\n",
		w.name, w.id, w.dir, w.mode, unit)
	if err == nil && w.host != "" {
		_, err = fmt.Fprintf(a.out, "once the turn ends, %s is shown in the desktop for a moment, which warms its desktop CLI; the moment waits while the desktop's window has the focus\n", w.host)
	}
	return err
}

// toDesktopCLI sends msg to the desktop's CLI of session id when one runs,
// so a headless resume never starts a second CLI beside it: two CLIs on one
// session are two peers under its name, each running turns of its own. It
// returns the CLI's PID, 0 when none runs or the process table cannot be
// read (a platform without one); a CLI that takes no message within twinWait
// is an error.
func (a *app) toDesktopCLI(ctx context.Context, id, msg string) (int, error) {
	t, err := plat.Machine.Processes()
	if err != nil {
		return 0, nil
	}
	p := desktopTwin(t, id)
	if p == nil {
		return 0, nil
	}
	sock := desktopSocketWithin(ctx, id, twinWait)
	if sock == "" {
		return p.PID, refused("its desktop CLI %d runs and takes no message yet: a headless resume would run beside it, send again in a minute", p.PID)
	}
	if err := cliSend(ctx, a, "uds:"+sock, msg); err != nil {
		return p.PID, fmt.Errorf("sending to its desktop CLI %d: %w", p.PID, err)
	}
	return p.PID, nil
}

// cliSend sends msg to the running CLI to; tests replace it.
var cliSend = func(ctx context.Context, a *app, to, msg string) error { return a.peerSend(ctx, to, msg) }

// awaitNoHeadless waits up to wait for the headless turn of session id to
// end: a desktop send or reopen starts the desktop's CLI, which must not run
// beside a headless turn of the same session. An error names the turn still
// running; a process table that cannot be read shows none.
func awaitNoHeadless(ctx context.Context, id string, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		t, err := plat.Machine.Processes()
		if err != nil {
			return nil
		}
		turn := headlessTurn(t, id)
		if turn == "" {
			return nil
		}
		select {
		case <-ctx.Done():
			return refused("its headless %s still runs after %s: the desktop's CLI would start beside it", turn, dur(wait))
		case <-tick.C:
		}
	}
}

// modelNotHeld is a desktop turn refused because the desktop records
// another model for the session than the one it was started on.
type modelNotHeld struct {
	recorded, want string
	why            error
}

func (e modelNotHeld) Error() string {
	return fmt.Sprintf("the desktop records %s for its desktop turns, not %s, and no session set it (%v)", cmp.Or(e.recorded, "no model"), e.want, e.why)
}

// holdModel has a steward give the desktop record r of session w the model
// w was started on. When none does, it ends the idle desktop CLI of w, which
// runs the recorded model, so the headless turn that follows on w.model is
// the session's one CLI, and returns modelNotHeld.
func (a *app) holdModel(ctx context.Context, w wakeTarget, r *claude.Record) error {
	_, err := a.restoreRecord(ctx, w.host, r.Title, w.model)
	if err == nil {
		return nil
	}
	if t, terr := plat.Machine.Processes(); terr == nil {
		if p := desktopTwin(t, w.id); p != nil {
			if pr, ferr := os.FindProcess(p.PID); ferr == nil {
				_ = pr.Signal(syscall.SIGTERM)
			}
		}
	}
	return modelNotHeld{recorded: r.Model, want: w.model, why: err}
}

// turnInDesktop has the desktop run msg as a turn of the session w, whose
// row the person sees it run in: its desktop CLI, when one runs or the
// desktop warms one within warm, takes msg by its socket; else a steward's
// send through the desktop's session messaging starts one with msg (the
// route a relay revives a role holder by). It returns how, one line; an
// error when the desktop cannot run the turn (it does not run, the session
// has no row, no steward took the send), whose turn then runs headless.
func (a *app) turnInDesktop(ctx context.Context, w wakeTarget, msg string, warm time.Duration) (string, error) {
	if w.host == "" || !strings.HasPrefix(w.host, "local_") {
		return "", errors.New("it has no desktop session")
	}
	r, ok := claude.ReadRecord(a.cfg, w.host)
	if !ok {
		return "", fmt.Errorf("%s has no row in the desktop", w.host)
	}
	t, err := plat.Machine.Processes()
	if err != nil {
		return "", err
	}
	if plat.Opener.Running(t).IsZero() {
		return "", errors.New("the desktop does not run")
	}
	if w.model != "" && !sameModel(r.Model, w.model) {
		if err := a.holdModel(ctx, w, r); err != nil {
			return "", err
		}
	}
	if sock := desktopSocketWithin(ctx, w.id, warm); sock != "" {
		if err := a.peerSend(ctx, "uds:"+sock, msg); err != nil {
			return "", fmt.Errorf("sending to its desktop CLI: %w", err)
		}
		return fmt.Sprintf("its desktop CLI took the message as its turn, in the desktop's row %s", w.host), nil
	}
	running := func() bool {
		t, err := plat.Machine.Processes()
		return err == nil && desktopTwin(t, w.id) != nil
	}
	s, err := a.sendThroughDesktop(ctx, w.host, msg, running)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s had the desktop start its CLI with the message as its turn, in the desktop's row %s", s.who(w.host), w.host), nil
}

// wakeOmp writes msg to the inbox of the omp agent ag, started under id.
func (a *app) wakeOmp(by state.Party, ag state.Agent, id, msg string) error {
	if err := a.sendOmp(ag.Name, id, msg); err != nil {
		return err
	}
	_ = a.store.Record(event(by, "agents.wake", "%s: written to its omp inbox", ag.Name))
	_, err := fmt.Fprintf(a.out, "wake: %s runs (omp): written to its inbox, it runs the message at its next tool round or as its next turn\n", ag.Name)
	return err
}

// sendOmp writes msg to the inbox of the omp agent name, started under id;
// one whose process ended is refused.
func (a *app) sendOmp(name, id, msg string) error {
	err := omp.Send(omp.InboxPath(a.cfg.StateDir, id), msg)
	if errors.Is(err, omp.ErrNotRunning) {
		return refused("%s: its omp agent no longer runs (%s ended): start it again with agents start --harness omp", name, ompUnit(id))
	}
	if err != nil {
		return fmt.Errorf("messaging %s: %w", name, err)
	}
	return nil
}

// wakeAgy writes msg to the inbox of the agy agent ag, started under id.
func (a *app) wakeAgy(by state.Party, ag state.Agent, id, msg string) error {
	if err := a.sendAgy(ag.Name, id, msg); err != nil {
		return err
	}
	_ = a.store.Record(event(by, "agents.wake", "%s: written to its agy inbox", ag.Name))
	_, err := fmt.Fprintf(a.out, "wake: %s runs (agy): written to its inbox, it runs the message before its next model call or as its next turn\n", ag.Name)
	return err
}

// sendAgy writes msg to the inbox of the agy agent name, started under id;
// one whose process ended is refused.
func (a *app) sendAgy(name, id, msg string) error {
	err := agy.Send(agy.InboxPath(a.cfg.StateDir, id), msg)
	if errors.Is(err, agy.ErrNotRunning) {
		return refused("%s: its agy agent no longer runs (%s ended): start it again with agents start --harness agy", name, agyUnit(id))
	}
	if err != nil {
		return fmt.Errorf("messaging %s: %w", name, err)
	}
	return nil
}

// wakeCodex writes msg to the inbox of the codex agent ag, started under id.
func (a *app) wakeCodex(by state.Party, ag state.Agent, id, msg string) error {
	if err := a.sendCodex(ag.Name, id, msg); err != nil {
		return err
	}
	_ = a.store.Record(event(by, "agents.wake", "%s: written to its codex inbox", ag.Name))
	_, err := fmt.Fprintf(a.out, "wake: %s runs (codex): written to its inbox, it runs the message as its next turn\n", ag.Name)
	return err
}

// sendCodex writes msg to the inbox of the codex agent name, started under id;
// one whose unit ended is refused.
func (a *app) sendCodex(name, id, msg string) error {
	err := codex.Send(codex.InboxPath(a.cfg.StateDir, id), msg)
	if errors.Is(err, codex.ErrNotRunning) {
		return refused("%s: its codex agent no longer runs (%s ended): start it again with agents start --harness codex", name, codexUnit(id))
	}
	if err != nil {
		return fmt.Errorf("messaging %s: %w", name, err)
	}
	return nil
}

// resolveWake finds the session an agent's wake resumes. A desktop session
// runs under the CLI session id its record names now (a restart gives it a
// new one), in the record's directory and mode; beekeeper's own start keeps
// its bypass. A session with no transcript cannot be resumed.
func resolveWake(cfg *config.Config, st *state.State, ag state.Agent) (wakeTarget, error) {
	w := wakeTarget{name: ag.Name, id: ag.Session, host: ag.HostSession}
	if _, ok := claude.ReadRecord(cfg, "local_"+ag.Session); w.host == "" && ag.Session != "" && ok {
		// A session the roster knows by its CLI id alone, which the desktop
		// imported: its wake gets the reopen too.
		w.host = "local_" + ag.Session
	}
	if w.host != "" {
		if r, ok := claude.ReadRecord(cfg, w.host); ok {
			if r.CLISessionID != "" {
				w.id = r.CLISessionID
			}
			w.dir, w.mode, w.model = r.Cwd, r.PermissionMode, r.Model
			if r.Title != "" {
				w.name = r.Title
			}
		}
	}
	for _, p := range []state.Party{{Session: w.id, HostSession: w.host}, ag.Party} {
		if s, ok := st.BypassStartOf(p); ok {
			// the model it was started on, which its turns keep
			if s.Model != "" && !sameModel(w.model, s.Model) {
				w.model = s.Model
			}
			w.mode = s.Mode
			if w.dir == "" {
				w.dir = s.Dir
			}
			w.profile = profileName(cfg, agentRole(st, ag.Party))
			break
		}
	}
	if w.id == "" {
		return wakeTarget{}, refused("%s has no session id to resume", ag.Name)
	}
	transcript := transcriptOf(cfg, w.id)
	if transcript == "" {
		return wakeTarget{}, refused("%s: session %s has no transcript under %s to resume", ag.Name, w.id, cfg.Claude.ProjectsDir)
	}
	if w.dir == "" {
		w.dir, _ = claude.TranscriptCwd(transcript)
	}
	if w.dir == "" {
		return wakeTarget{}, refused("%s: no directory is recorded for session %s", ag.Name, w.id)
	}
	if w.mode == "" {
		w.mode = guard.ModeAcceptEdits
	}
	return w, nil
}

// transcriptOf is the transcript of session id under the projects
// directory, "" for none.
func transcriptOf(cfg *config.Config, id string) string {
	return claude.Transcript(cfg, id, "")
}

// wakeLive is the running CLI of the agent: its party's, or one that runs
// the session id the wake would resume.
func wakeLive(sessions []*claude.Session, p state.Party, id string) (*claude.Session, bool) {
	if s, ok := claude.Live(sessions, p); ok {
		return s, true
	}
	i := slices.IndexFunc(sessions, func(s *claude.Session) bool { return id != "" && s.ID == id })
	if i < 0 {
		return nil, false
	}
	return sessions[i], true
}

// uniqueName is the name s takes messages under, refused when another
// running CLI carries it too: a send by that name reaches neither for sure.
// Two CLIs of one session (a headless turn and a desktop CLI beside it) are
// two such peers.
func uniqueName(sessions []*claude.Session, s *claude.Session) (string, error) {
	var pids []string
	for _, o := range sessions {
		if strings.EqualFold(o.Address(), s.Address()) {
			pids = append(pids, fmt.Sprint(o.PID))
		}
	}
	if len(pids) > 1 {
		return "", refused("%d running CLIs are named %q (PIDs %s): a message by name reaches neither for sure; stop the one that should not run",
			len(pids), s.Address(), strings.Join(pids, ", "))
	}
	return s.Address(), nil
}

// resumeFlag continues a session in a new CLI.
const resumeFlag = "--resume"

// wakePrefix starts the name of every wake unit of session id.
func wakePrefix(id string) string { return "beekeeper-wake-" + id[:min(8, len(id))] }

// wakeUnit is the transient unit a wake of session id runs in, a new name
// for every wake: an earlier wake's unit stays loaded while a process its
// turn started runs on (KillMode=process), and systemd refuses a transient
// unit under a loaded unit's name.
func wakeUnit(id string) string { return wakePrefix(id) + "-" + uuid.NewString()[:8] }

// wakeRunning is an active or activating wake unit of session id, "" for
// none.
func wakeRunning(ctx context.Context, id string) string {
	if u := plat.Launcher.Running(ctx, false, wakePrefix(id)+"*"); len(u) > 0 {
		return u[0]
	}
	return ""
}

// profiledWakeArgv is wakeArgv under w's profile, whose flag settings it
// writes first.
func (a *app) profiledWakeArgv(ctx context.Context, bin string, w wakeTarget, msg string) ([]string, error) {
	if w.profile == "" {
		return wakeArgv(bin, w, msg), nil
	}
	flags, err := a.profileFlags(ctx, w.profile)
	if err != nil {
		return nil, err
	}
	return wakeArgv(bin, w, msg, flags...), nil
}

// underProfile names a headless turn's profile for a log line; empty for
// none.
func underProfile(profile string) string {
	if profile == "" {
		return ""
	}
	return ", under profile " + profile
}

// wakeArgv is a wake's command line: one headless turn resuming session id
// in its mode and on its model, under its profile's flags, the message as
// the turn; flags go before the message.
func wakeArgv(bin string, w wakeTarget, msg string, profile ...string) []string {
	argv := []string{bin, "-p", resumeFlag, w.id, permissionModeFlag, w.mode}
	if w.name != "" {
		argv = append(argv, "-n", w.name)
	}
	if w.model != "" {
		argv = append(argv, modelFlag, w.model)
	}
	if w.mode == state.ModeBypass {
		// the CLI's own Chrome connection, which in bypass never waits on
		// a person's site approval as the desktop's does
		argv = append(argv, chromeFlag)
	}
	argv = append(argv, profile...)
	return append(argv, "--", msg)
}

// permissionModeFlag sets a turn's permission mode.
const permissionModeFlag = "--permission-mode"

// chromeFlag connects a headless turn to the person's Chrome through the
// CLI's own Claude in Chrome integration.
const chromeFlag = "--chrome"

// headlessTurn says which headless beekeeper turn of session id runs: "first
// turn" (agents start), "wake turn" (agents wake), "" for none.
func headlessTurn(t *proc.Table, id string) string {
	if t == nil || id == "" {
		return ""
	}
	for _, p := range t.ByPID {
		if !printsTurn(p) {
			continue
		}
		switch {
		case startsSession(p, id):
			return "first turn"
		case resumes(p.Args, id):
			return "wake turn"
		}
	}
	return ""
}

// headlessRuns is a desktop CLI not warmed for a session because its
// headless turn runs: two CLIs on one session are twins.
type headlessRuns string

func (h headlessRuns) Error() string {
	return "its headless " + string(h) + " runs, and a desktop CLI warmed beside it would be its twin"
}

// noHeadlessTurn is the last look before a show warms the desktop's CLI of
// session id: headlessRuns when a headless turn of it runs. A process table
// that cannot be read shows none.
func noHeadlessTurn(id string) error {
	t, err := plat.Machine.Processes()
	if err != nil {
		return nil
	}
	if turn := headlessTurn(t, id); turn != "" {
		return headlessRuns(turn)
	}
	return nil
}

// printsTurn reports whether p is a headless claude turn (-p, --print).
func printsTurn(p *proc.Process) bool {
	return p.Comm == claudeComm && (slices.Contains(p.Args, "-p") || slices.Contains(p.Args, "--print"))
}

// roleTarget is the PreToolUse hook's lookup for a message to a role (the
// supervisor, the guide): relayAddress of its record now. A relay moves the
// role, so a brief names the role, never the holder.
func (a *app) roleTarget(name string) (string, error) {
	if err := a.loadConfig(); err != nil {
		return "", fmt.Errorf("beekeeper's configuration does not load, so the %s is unknown: %w", name, err)
	}
	rl := supervisorRole
	if name == guard.RoleGuide {
		rl = guideRole
	}
	store, err := state.Open(a.cfg.StateDir)
	if err != nil {
		return "", err
	}
	st, err := store.Read()
	if err != nil {
		return "", err
	}
	now := time.Now()
	var sessions []*claude.Session
	if t, err := plat.Machine.Processes(); err == nil {
		sessions = claude.Discover(a.cfg, t, now)
	}
	return relayAddress(rl.get(st), rl, sessions, liveSocket, now, rl.cfg(a.cfg).RelayTTL.Duration)
}

// liveSocket is the peer socket of the CLI pid, "" when it has none.
func liveSocket(pid int) string {
	if sock := peerSocket(runtimeDir(), pid); fileExists(sock) {
		return sock
	}
	return ""
}

// heldRole is the PreToolUse hook's lookup for a SendMessage by name: the
// role whose holder the name addresses (holderRole), "" when none.
func (a *app) heldRole(name string) string {
	if a.loadConfig() != nil {
		return ""
	}
	store, err := state.Open(a.cfg.StateDir)
	if err != nil {
		return ""
	}
	st, err := store.Read()
	if err != nil {
		return ""
	}
	return holderRole(st, name)
}

// holderRole names the role whose holder name addresses: the holder's
// recorded name or session id, or the name of its roster entry, which keeps
// the run name ("Supervisor run 82") after the holder takes its desktop
// title; "" when name addresses no holder.
func holderRole(st *state.State, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	for _, rl := range roles {
		h := rl.get(st).Holder
		if h == nil {
			continue
		}
		if strings.EqualFold(h.Name, name) || h.Session == name || h.HostSession == name {
			return rl.name
		}
		for _, ag := range st.Agents {
			if (strings.EqualFold(ag.Name, name) || ag.Session == name) && ag.Is(h.Party) {
				return rl.name
			}
		}
	}
	return ""
}

// relayAddress is where a message to rl goes: its holder's roleAddress,
// never a run a relay relieved, which status no longer counts as the
// holder. In the relay window, from the successor's start for window, a
// message while the successor's CLI runs without a peer socket (its
// headless first turn, its desktop CLI coming up) is refused with when to
// send again: by name nothing answers it, and its desktop session id would
// start a second CLI beside it.
func relayAddress(r state.Role, rl role, sessions []*claude.Session, sock func(pid int) string, now time.Time, window time.Duration) (string, error) {
	rel := r.Relay
	if r.Holder != nil && rel != nil && !rel.Taken.IsZero() && rel.To.Is(r.Holder.Party) && now.Sub(rel.Taken) < window {
		if s, ok := claude.Live(sessions, r.Holder.Party); ok && sock(s.PID) == "" {
			return "", fmt.Errorf("the %s is %q since %s, whose CLI %d takes no message yet (its first turn runs headless, or the desktop is warming its CLI): send again in a minute; the relieved %q gets none",
				rl.name, r.Holder.Name, clock(now, rel.Taken), s.PID, rel.From.Name)
		}
	}
	return roleAddress(r.Holder, rl, sessions, sock)
}

// roleAddress is where a message to rl's holder goes: the name its running
// CLI takes messages under, or that CLI's own socket (sock) while another
// live session carries the same name, else its desktop session id, which the
// desktop starts, else its name. The holder is beekeeper's record, matched to
// its CLI by session id, so a shared title never makes the role ambiguous.
func roleAddress(holder *state.Supervisor, rl role, sessions []*claude.Session, sock func(pid int) string) (string, error) {
	if holder == nil {
		return "", fmt.Errorf("no session holds the %s's role (beekeeper %s status): the message has nobody to go to", rl.name, rl.name)
	}
	if s, ok := claude.Live(sessions, holder.Party); ok {
		name, err := uniqueName(sessions, s)
		if err != nil {
			if path := sock(s.PID); path != "" {
				return "uds:" + path, nil
			}
		}
		return name, err
	}
	return cmp.Or(holder.HostSession, holder.Name), nil
}

// desktopPeer is the PreToolUse hook's lookup for a SendMessage to a
// desktop session id: the name its running CLI takes messages under, "" when
// none runs (the desktop starts one), an error when the name is ambiguous.
func (a *app) desktopPeer(host string) (string, error) {
	if a.loadConfig() != nil {
		return "", nil
	}
	t, err := plat.Machine.Processes()
	if err != nil {
		return "", nil
	}
	sessions := claude.Discover(a.cfg, t, time.Now())
	s, ok := wakeLive(sessions, state.Party{HostSession: host}, a.cliSession(host))
	if !ok {
		if s, ok = wakeLive(sessions, state.Party{}, strings.TrimPrefix(host, "local_")); !ok {
			return "", nil
		}
	}
	name, err := uniqueName(sessions, s)
	if err != nil {
		if path := liveSocket(s.PID); path != "" {
			return "uds:" + path, nil
		}
		return "", fmt.Errorf("%w. A desktop send to %s would start yet another copy", err, host)
	}
	return name, nil
}

// agentPeer is the PreToolUse hook's lookup for a SendMessage by a name
// several running CLIs carry: the address sharedNameAddress gives.
func (a *app) agentPeer(name string) (string, error) {
	if a.loadConfig() != nil {
		return "", nil
	}
	t, err := plat.Machine.Processes()
	if err != nil {
		return "", nil
	}
	store, err := state.Open(a.cfg.StateDir)
	if err != nil {
		return "", nil
	}
	st, err := store.Read()
	if err != nil {
		return "", nil
	}
	return sharedNameAddress(st, discover(a.cfg, t, time.Now()), name, time.Now(), liveSocket)
}

// sharedNameAddress is where a message by name goes while running CLIs other
// than the roster agent's of that name carry it, as a hand-over's
// follow-up and the session it replaces both do until that one ends: the
// agent's own CLI's socket, so the follow-up gets it. "" leaves the name as
// written: no agent carries it, or no other CLI does. An agent whose CLI
// does not run is refused, as the message would reach the other session.
func sharedNameAddress(st *state.State, sessions []*claude.Session, name string, now time.Time, sock func(pid int) string) (string, error) {
	i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return strings.EqualFold(ag.Name, name) })
	if i < 0 {
		return "", nil
	}
	ag := st.Agents[i]
	own, live := claude.Live(sessions, ag.Party)
	var others []string
	for _, s := range sessions {
		if strings.EqualFold(s.Address(), name) && s != own {
			others = append(others, s.ID)
		}
	}
	switch {
	case len(others) == 0:
		return "", nil
	case !live:
		return "", refused("%s. By name the message would reach session %s, which is not the agent's: beekeeper agents wake %q <message> reaches it",
			noCLI(ag, now), strings.Join(others, ", "), ag.Name)
	}
	path := sock(own.PID)
	if path == "" {
		return "", refused("the running CLIs of sessions %s and %s are named %q and the agent's CLI %d has no peer socket: send to its session id %s",
			strings.Join(others, ", "), own.ID, name, own.PID, cmp.Or(ag.HostSession, ag.Session))
	}
	return socketAddress + path, nil
}

// absentPeer is the PreToolUse hook's lookup for a SendMessage by name: what
// says that the roster agent of that name (or session id) runs no CLI and
// whether its import is pending, "" when a CLI carries the name or no agent
// does.
func (a *app) absentPeer(name string) string {
	if a.loadConfig() != nil {
		return ""
	}
	t, err := plat.Machine.Processes()
	if err != nil {
		return ""
	}
	store, err := state.Open(a.cfg.StateDir)
	if err != nil {
		return ""
	}
	st, err := store.Read()
	if err != nil {
		return ""
	}
	return absentAgent(st, discover(a.cfg, t, time.Now()), name, time.Now())
}

// absentAgent is absentPeer's answer from the state and the running
// sessions.
func absentAgent(st *state.State, sessions []*claude.Session, name string, now time.Time) string {
	for _, s := range sessions {
		if strings.EqualFold(s.Name, name) || strings.EqualFold(s.Address(), name) || s.ID == name {
			return ""
		}
	}
	i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return strings.EqualFold(ag.Name, name) || ag.Session == name })
	if i < 0 {
		return ""
	}
	return noCLI(st.Agents[i], now) + ". A message by name reaches it once a CLI of it runs"
}
