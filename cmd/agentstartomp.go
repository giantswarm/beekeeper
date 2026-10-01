package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/state"
)

// inboxWait bounds the wait for a started omp agent to open its inbox.
const inboxWait = 30 * time.Second

// ompUnit is the transient user unit an omp agent runs in.
func ompUnit(id string) string { return "beekeeper-omp-" + id[:8] }

// startOmpAgent records and registers an omp agent, starts it in a
// transient user unit with its stdin on its inbox and sends it the brief.
func (a *app) startOmpAgent(ctx context.Context, sp agentStart) error {
	dir, err := filepath.Abs(sp.dir)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return usageErr("--dir %s: not a directory", dir)
	}
	bin, err := exec.LookPath(omp.Comm)
	if err != nil {
		return err
	}
	by, err := a.caller()
	if err != nil {
		return err
	}
	sessions, _, err := a.sessions()
	if err != nil {
		return err
	}
	id := uuid.NewString()
	inbox := omp.InboxPath(a.cfg.StateDir, id)
	if err := omp.MakeInbox(inbox); err != nil {
		return err
	}
	model := sp.model
	if model == "" {
		model = a.cfg.Omp.Model
	}
	s := state.Start{Party: state.Party{HostSession: omp.HostPrefix + id, Name: sp.name}, Mode: state.ModeBypass, Dir: dir, By: by, At: a.now.UTC(), Harness: omp.Harness}
	var reg registration
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		var err error
		reg, err = recordStart(st, s, sp.task, func(p state.Party) bool { _, ok := claude.Live(sessions, p); return ok })
		if err != nil {
			return nil, err
		}
		return []state.Event{event(by, "agents.start", "%s: omp agent %s in %s, yolo, busy with %q", sp.name, id, dir, reg.task)}, nil
	})
	if err != nil {
		return err
	}
	unit := ompUnit(id)
	if err := launch(unit, dir, a.explicitConfig(), nil, omp.ShellArgv(inbox, ompArgv(bin, model)...),
		omp.EnvAgent+"="+id, omp.EnvName+"="+sp.name); err != nil {
		return fmt.Errorf("starting %s: %w (the start stays recorded; beekeeper agents remove %q takes it off the roster)", sp.name, err, sp.name)
	}
	if err := sendWhenOpen(ctx, inbox, sp.brief, inboxWait); err != nil {
		return fmt.Errorf("%s: the brief did not reach it: %w (journalctl --user -u %s)", sp.name, err, unit)
	}
	modelText := model
	if modelText == "" {
		modelText = "omp's default model"
	}
	_, err = fmt.Fprintf(a.out, "started %s: omp agent omp_%s, yolo, on %s, in %s, busy with %q\n"+
		"its brief is its first message; agents wake %q <message> reaches it at its next tool round (journalctl --user -u %s)\n",
		sp.name, id, modelText, dir, reg.task, sp.name, unit)
	return err
}

// ompArgv is an omp agent's command line: the rpc protocol, no approvals,
// extensions headless.
func ompArgv(bin, model string) []string {
	argv := []string{bin, "--mode", "rpc", "--no-ui", "--approval-mode", "yolo"}
	if model != "" {
		argv = append(argv, "--model", model)
	}
	return argv
}

// sendWhenOpen sends msg to the inbox once the agent opened it, waiting up
// to wait for that.
func sendWhenOpen(ctx context.Context, inbox, msg string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := omp.Send(inbox, msg)
		if !errors.Is(err, omp.ErrNotRunning) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(awayPoll):
		}
	}
}
