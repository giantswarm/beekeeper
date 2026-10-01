package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
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
		Use:   "wake <agent> <message>",
		Short: "Message an agent, starting its CLI headless when none runs: no desktop cap",
		Long: `wake delivers a message to a registered agent without Claude Desktop's
route, whose cap pauses a session's messages to local_ ids after ten sends
since its person last typed in it.

An omp agent (agents start --harness omp) gets the message in its inbox,
at its next tool round or as its next turn; one whose process ended is
refused: nothing resumes it.

A session whose CLI runs gets the message by name, as SendMessage by name
does: it queues and runs at the session's next tool call or as its next turn.
A session with no running CLI (after a reboot, a crash, the desktop's idle
drop) is resumed from the command line: "claude -p --resume <id>" with the
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
}

// wakeAgent delivers msg from by to the registered agent q (wake).
func (a *app) wakeAgent(ctx context.Context, by state.Party, q, msg, mode string) error {
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	i, err := findAgent(st, q)
	if err != nil {
		return err
	}
	ag := st.Agents[i]
	if id, ok := strings.CutPrefix(ag.HostSession, omp.HostPrefix); ok {
		return a.wakeOmp(by, ag, id, msg)
	}
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
		_ = a.store.Log(event(by, "agents.wake", "%s: sent by name to its running CLI %d", ag.Name, s.PID))
		_, err = fmt.Fprintf(a.out, "wake: %s runs (CLI %d): sent by name, it runs the message at its next tool call or as its next turn\n", ag.Name, s.PID)
		return err
	}
	if u := wakeRunning(ctx, w.id); u != "" {
		return refused("%s: its wake turn %s is starting and not yet reachable by name: send again in a minute", ag.Name, u)
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
		stopPost = []string{self, agentsName, reopenName, w.host}
	}
	if err := launch(unit, w.dir, a.explicitConfig(), stopPost, wakeArgv(bin, w, msg)); err != nil {
		return fmt.Errorf("waking %s: %w", ag.Name, err)
	}
	_ = a.store.Log(event(by, "agents.wake", "%s: resumed session %s headless in %s, %s (%s)", ag.Name, w.id, w.dir, w.mode, unit))
	_, err = fmt.Fprintf(a.out, "wake: %s had no running CLI: resumed session %s headless in %s, %s, with the message as its turn (journalctl --user -u %s)\n",
		ag.Name, w.id, w.dir, w.mode, unit)
	if err == nil && w.host != "" {
		_, err = fmt.Fprintf(a.out, "once the turn ends, %s is shown in the desktop for a moment, which warms its desktop CLI; the moment waits while the desktop's window has the focus\n", w.host)
	}
	return err
}

// wakeOmp writes msg to the inbox of the omp agent ag, started under id.
func (a *app) wakeOmp(by state.Party, ag state.Agent, id, msg string) error {
	if err := a.sendOmp(ag.Name, id, msg); err != nil {
		return err
	}
	_ = a.store.Log(event(by, "agents.wake", "%s: written to its omp inbox", ag.Name))
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
	for _, id := range []string{w.id, ag.Session} {
		if s, ok := st.BypassStart(id); ok {
			w.mode = s.Mode
			if w.dir == "" {
				w.dir = s.Dir
			}
			break
		}
	}
	if w.id == "" {
		return wakeTarget{}, refused("%s has no session id to resume", ag.Name)
	}
	m, _ := filepath.Glob(filepath.Join(cfg.Claude.ProjectsDir, "*", w.id+".jsonl"))
	if len(m) == 0 {
		return wakeTarget{}, refused("%s: session %s has no transcript under %s to resume", ag.Name, w.id, cfg.Claude.ProjectsDir)
	}
	if w.dir == "" {
		w.dir, _ = claude.TranscriptCwd(m[0])
	}
	if w.dir == "" {
		return wakeTarget{}, refused("%s: no directory is recorded for session %s", ag.Name, w.id)
	}
	if w.mode == "" {
		w.mode = guard.ModeAcceptEdits
	}
	return w, nil
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
		if strings.EqualFold(o.Name, s.Name) {
			pids = append(pids, fmt.Sprint(o.PID))
		}
	}
	if len(pids) > 1 {
		return "", refused("%d running CLIs are named %q (PIDs %s): a message by name reaches neither for sure; stop the one that should not run",
			len(pids), s.Name, strings.Join(pids, ", "))
	}
	return s.Name, nil
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

// wakeArgv is a wake's command line: one headless turn resuming session id
// in its mode and on its model, the message as the turn; flags go before the
// message.
func wakeArgv(bin string, w wakeTarget, msg string) []string {
	argv := []string{bin, "-p", resumeFlag, w.id, "--permission-mode", w.mode}
	if w.name != "" {
		argv = append(argv, "-n", w.name)
	}
	if w.model != "" {
		argv = append(argv, modelFlag, w.model)
	}
	return append(argv, "--", msg)
}

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

// printsTurn reports whether p is a headless claude turn (-p, --print).
func printsTurn(p *proc.Process) bool {
	return p.Comm == claudeComm && (slices.Contains(p.Args, "-p") || slices.Contains(p.Args, "--print"))
}

// roleTarget is the PreToolUse hook's lookup for a message to a role (the
// supervisor, the guide): roleAddress of its holder now. A relay moves the
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
	var sessions []*claude.Session
	if t, err := plat.Machine.Processes(); err == nil {
		sessions = claude.Discover(a.cfg, t, time.Now())
	}
	return roleAddress(rl.get(st).Holder, rl, sessions)
}

// roleAddress is where a message to rl's holder goes: the name its running
// CLI takes messages under, else its desktop session id, which the desktop
// starts, else its name.
func roleAddress(holder *state.Supervisor, rl role, sessions []*claude.Session) (string, error) {
	if holder == nil {
		return "", fmt.Errorf("no session holds the %s's role (beekeeper %s status): the message has nobody to go to", rl.name, rl.name)
	}
	if s, ok := claude.Live(sessions, holder.Party); ok {
		return uniqueName(sessions, s)
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
	id := strings.TrimPrefix(host, "local_")
	if r, ok := claude.ReadRecord(a.cfg, host); ok && r.CLISessionID != "" {
		id = r.CLISessionID
	}
	s, ok := wakeLive(sessions, state.Party{HostSession: host}, id)
	if !ok {
		if s, ok = wakeLive(sessions, state.Party{}, strings.TrimPrefix(host, "local_")); !ok {
			return "", nil
		}
	}
	name, err := uniqueName(sessions, s)
	if err != nil {
		return "", fmt.Errorf("%w. A desktop send to %s would start yet another copy", err, host)
	}
	return name, nil
}
