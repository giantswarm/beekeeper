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
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

// codexRunCmdName is the hidden command a codex agent's user unit runs to
// drive `codex exec` / `codex exec resume` one turn at a time from its
// persistent FIFO inbox.
const codexRunCmdName = "codex-run"

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
	if !plat.Launcher.Available() {
		return refused("this machine has no unit launcher (systemd --user): a codex agent runs in a transient user unit")
	}
	dir, err := a.agentDir(sp.dir)
	if err != nil {
		return err
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
	sp.brief = fillScratchpad(sp.brief, dir, id)
	inbox := codex.InboxPath(a.cfg.StateDir, id)
	logPath := codex.LogPath(a.cfg.StateDir, id)
	if err := codex.MakeInbox(inbox); err != nil {
		return err
	}

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
		_, _ = codex.RemoveInbox(inbox)
		return err
	}

	unit := codexUnit(id)
	runnerArgv := []string{self, codexRunCmdName, inbox, logPath, bin, model}
	if err := launch(
		unit,
		dir,
		a.explicitConfig(),
		nil,
		runnerArgv,
		codex.EnvAgent+"="+id,
		codex.EnvName+"="+sp.name,
		"CODEX_HOME="+codexHome,
		"ZDOTDIR="+codex.ZshDir(codexHome),
	); err != nil {
		_, _ = codex.RemoveInbox(inbox)
		return a.undelivered(by, s.Party, err)
	}
	if err := sendCodexWhenReady(ctx, inbox, sp.brief, inboxWait); err != nil {
		return a.undelivered(by, s.Party, err)
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
