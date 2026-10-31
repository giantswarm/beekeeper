package cmd

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/state"
)

// codexRunCmdName is the hidden command a codex agent's user unit runs to
// drive `codex exec` / `codex exec resume` one turn at a time from its
// persistent FIFO inbox.
const codexRunCmdName = "codex-run"

// codexBriefWait bounds the wait for a started codex session's stream log to
// show its thread_id and first turn.started event; tests shorten it.
var codexBriefWait = 60 * time.Second

// codexUnit is the transient user unit a codex agent runs in.
func codexUnit(id string) string { return "beekeeper-codex-" + id[:min(8, len(id))] }

func (a *app) codexRunCmd() *cobra.Command {
	c := &cobra.Command{
		Use:    codexRunCmdName + " <inbox> <log> <bin> [model]",
		Short:  "Run a codex session one turn at a time on its persistent inbox",
		Hidden: true,
		Args:   cobra.RangeArgs(3, 4),
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			var model string
			if len(args) >= 4 {
				model = args[3]
			}
			return codex.Run(cmd.Context(), args[0], args[1], args[2], model, os.Stderr)
		},
	}
	c.Flags().SetInterspersed(false)
	return c
}

// startCodexAgent starts sp as a Codex CLI agent: `beekeeper codex-run` in a
// transient systemd user unit with an isolated CODEX_HOME, its FIFO inbox in
// the state directory, and sp.brief (worker rules + brief) queued as its first
// turn.
func (a *app) startCodexAgent(ctx context.Context, sp agentStart) error {
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	if err := a.refuseStopped(st, "starting "+sp.name); err != nil {
		return err
	}
	if !plat.Launcher.Available() {
		return refused("this machine has no unit launcher (systemd --user): a codex agent runs in a transient user unit")
	}
	dir, err := a.agentDir(sp.dir)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return usageErr("--dir %s: not a directory", dir)
	}
	model, err := a.codexModel(sp.model)
	if err != nil {
		return err
	}
	bin, err := exec.LookPath(codex.Comm)
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	var by state.Party
	if sp.by != nil {
		by = *sp.by
	} else if by, err = a.caller(); err != nil {
		return err
	}
	sessions, _, err := a.sessions()
	if err != nil {
		return err
	}
	live := func(p state.Party) bool {
		if sp.replaces != nil && p.Is(*sp.replaces) {
			return false
		}
		_, ok := claude.Live(sessions, p)
		return ok
	}

	codexHome := codex.HomeDir(a.cfg.StateDir)
	sh := a.cfg.Agents.Shell
	unalias := append(slices.Clone(sh.Unalias), a.cfg.Secret.UnlockCommands...)
	prelude := guard.Prelude(unalias, sh.Globs == config.GlobsLiteral, sh.Path, nil, "")
	if err := codex.EnsureHome(codexHome, a.cfg.Codex.AuthFile, a.cfg.Codex.ConfigFile, self, prelude); err != nil {
		return err
	}

	id := sp.id
	if id == "" {
		id = uuid.NewString()
	}
	sp.brief = projectPrompt(dir, sp.brief)
	sp.brief = fillScratchpad(sp.brief, dir, id)
	inbox := codex.InboxPath(a.cfg.StateDir, id)
	logPath := codex.LogPath(a.cfg.StateDir, id)

	s := state.Start{
		Party:   state.Party{HostSession: codex.HostPrefix + id, Name: sp.name},
		Mode:    state.ModeBypass,
		Dir:     dir,
		By:      by,
		At:      a.now.UTC(),
		Harness: codex.Harness,
		Model:   model,
	}
	var reg registration
	var handed []string
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		var err error
		reg, err = recordStart(st, s, sp.task, sp.serves, live)
		if err != nil {
			return nil, err
		}
		evs := []state.Event{event(by, "agents.start", "%s: codex agent codex_%s in %s, bypass, on %s, busy with %q", sp.name, id, dir, model, reg.task)}
		if sp.replaces != nil {
			moveRecord(st, *sp.replaces, s.Party)
			if handed, err = handClaims(st, lease.Dir(a.cfg.LeaseDir), *sp.replaces, s.Party); err != nil {
				return nil, err
			}
			if len(handed) > 0 {
				evs = append(evs, event(by, "lease.handover", "%s: %s from %s to codex_%s", sp.name, strings.Join(handed, ", "), cmp.Or(sp.replaces.Session, sp.replaces.HostSession), id))
			}
		}
		return evs, nil
	})
	if err != nil {
		return err
	}
	if err := codex.MakeInbox(inbox); err != nil {
		return err
	}

	unit := codexUnit(id)
	runnerArgv := []string{self, codexRunCmdName, inbox, logPath, bin, model}
	if err := launchCodex(
		unit,
		dir,
		a.explicitConfig(),
		runnerArgv,
		codex.EnvAgent+"="+id,
		codex.EnvName+"="+sp.name,
		"CODEX_HOME="+codexHome,
		"ZDOTDIR="+codex.ZshDir(codexHome),
	); err != nil {
		_, _ = codex.RemoveInbox(inbox)
		return fmt.Errorf("starting %s: %w (the start stays recorded; beekeeper agents remove %q takes it off the roster)", sp.name, err, sp.name)
	}
	if err := sendCodexWhenReady(ctx, inbox, sp.brief, inboxWait); err != nil {
		_ = plat.Launcher.Stop(ctx, unit)
		_, _ = codex.RemoveInbox(inbox)
		return a.undelivered(by, s.Party, fmt.Errorf("the brief did not reach its inbox: %w (journalctl --user -u %s)", err, unit))
	}
	if _, err := awaitCodexBrief(ctx, logPath, func() bool { return unitEnded(ctx, unit) }, codexBriefWait); err != nil {
		_ = plat.Launcher.Stop(ctx, unit)
		_, _ = codex.RemoveInbox(inbox)
		return a.undelivered(by, s.Party, fmt.Errorf("%w (journalctl --user -u %s)", err, unit))
	}
	_, err = fmt.Fprintf(a.out, "started %s: codex agent codex_%s, bypass, on %s, in %s, busy with %q\n"+
		"turn: %s (%s)\n",
		sp.name, id, model, dir, reg.task, unit, strings.Join(codex.ExecArgv(codex.Comm, model), " "))
	for _, r := range handed {
		if err == nil {
			_, err = fmt.Fprintf(a.out, "handed over lease %s to %s\n", r, sp.name)
		}
	}
	return err
}

// launchCodex starts a codex-run transient user service with KeepChildren: false
// (KillMode=control-group) so stopping the unit terminates and waits for the
// runner and any active codex child turn process tree.
func launchCodex(unit, dir, config string, argv []string, env ...string) error {
	return startUnit(platform.Unit{
		Name:          unit,
		Dir:           dir,
		Argv:          argv,
		KeepChildren:  false,
		TermIsSuccess: true,
		StopTimeout:   turnStopWait,
	}, config, env...)
}

func (a *app) codexModel(flag string) (string, error) {
	m := cmp.Or(strings.TrimSpace(flag), strings.TrimSpace(a.cfg.Codex.Model))
	if m == "" {
		return "", usageErr("a codex agent needs its model: --model, or codex.model in the config")
	}
	return m, nil
}

func (a *app) dryRunCodexStart(sp agentStart, gaps []string) error {
	dir, err := a.agentDir(sp.dir)
	if err != nil {
		return err
	}
	sp.brief = projectPrompt(dir, sp.brief)
	model := cmp.Or(sp.model, a.cfg.Codex.Model, "<model>")
	argv := codex.ExecArgv(codex.Comm, model)
	if _, err := fmt.Fprintf(a.out, "dry run: %s would start in %s as a codex agent on %s, bypass, no desktop import\nstart: %s\n",
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

var sendCodexWhenReady = func(ctx context.Context, inbox, msg string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := codex.Send(inbox, msg)
		if err == nil {
			return nil
		}
		if !errors.Is(err, codex.ErrNotRunning) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// awaitCodexBrief waits up to wait for the stream log at logPath to show the
// started session's thread.started and turn.started events, confirming the
// first turn began in its own thread.
func awaitCodexBrief(ctx context.Context, logPath string, ended func() bool, wait time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		threadID, started, err := readCodexBriefStream(logPath)
		if err != nil {
			return threadID, err
		}
		if threadID != "" && started {
			return threadID, nil
		}
		if ended != nil && ended() {
			threadID, started, err = readCodexBriefStream(logPath)
			if err != nil {
				return threadID, err
			}
			if threadID != "" && started {
				return threadID, nil
			}
			return threadID, errors.New("codex ended before its first turn started")
		}
		select {
		case <-ctx.Done():
			return threadID, fmt.Errorf("the brief did not start a codex turn within %s", wait)
		case <-tick.C:
		}
	}
}

func readCodexBriefStream(logPath string) (threadID string, started bool, err error) {
	f, oerr := os.Open(logPath) //nolint:gosec // beekeeper's own stream log
	if oerr != nil {
		return "", false, nil
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var ev struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Message  string `json:"message"`
			Error    *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch {
		case ev.Type == "thread.started":
			if ev.ThreadID != "" {
				threadID = ev.ThreadID
			}
		case ev.Type == "turn.started" || ev.Type == "item.started" || ev.Type == "item.completed" || ev.Type == "turn.completed":
			started = true
		case ev.Type == "turn.failed" || strings.EqualFold(ev.Type, "ERROR"):
			msg := strings.TrimSpace(ev.Message)
			if ev.Error != nil && strings.TrimSpace(ev.Error.Message) != "" {
				msg = strings.TrimSpace(ev.Error.Message)
			}
			if msg == "" {
				msg = "codex reported an error before starting the turn"
			}
			return threadID, false, errors.New(msg)
		}
	}
	return threadID, started, nil
}

// endCodex stops the unit of the codex agent started under id, which left the
// roster, and removes its inbox, log and thread files, and says what it did.
func (a *app) endCodex(ctx context.Context, id string) string {
	var done []string
	if unit := codexUnit(id); plat.Launcher.State(ctx, unit) == holdActive {
		if err := plat.Launcher.Stop(ctx, unit); err != nil {
			return fmt.Sprintf("its codex unit %s runs on: %v", unit, err)
		}
		done = append(done, "stopped its codex unit "+unit)
	}
	removed, err := codex.RemoveInbox(codex.InboxPath(a.cfg.StateDir, id))
	switch {
	case err != nil:
		done = append(done, fmt.Sprintf("its codex inbox stays: %v", err))
	case removed:
		done = append(done, "removed its codex inbox")
	}
	if len(done) == 0 {
		return "a codex agent has no desktop session"
	}
	return strings.Join(done, ", ")
}
