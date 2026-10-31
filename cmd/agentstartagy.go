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
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// agyBriefWait bounds the wait for a started agy session's stream log to
// show its conversation_id and first user_input step; tests shorten it.
var agyBriefWait = 60 * time.Second

// agyRemoteDBWait bounds the wait for agy to write <convID>.db before
// restarting the remote daemon; tests shorten it.
var agyRemoteDBWait = 2 * time.Second

// startAgyAgent validates --model against agy models, records and registers
// the agent, starts it in a transient user unit with beekeeper agy-run
// driving agy from its FIFO inbox, sends it the worker rules and brief, and
// confirms the brief arrived in its own conversation.
func (a *app) startAgyAgent(ctx context.Context, sp agentStart) error {
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	if err := a.refuseStopped(st, "starting "+sp.name); err != nil {
		return err
	}
	dir, err := a.agentDir(sp.dir)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return usageErr("--dir %s: not a directory", dir)
	}
	bin, err := exec.LookPath(agy.Comm)
	if err != nil {
		return err
	}
	model, err := agyModel(sp.model, func() ([]string, error) { return agyModels(ctx, bin) })
	if err != nil {
		return err
	}
	self, err := os.Executable()
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
	sp.brief = "Working directory: " + dir + "\n\n" + fillScratchpad(sp.brief, dir, id)
	sp.brief = projectPrompt(dir, sp.brief)
	inbox := agy.InboxPath(a.cfg.StateDir, id)
	stream := agy.StreamPath(a.cfg.StateDir, id)
	s := state.Start{Party: state.Party{HostSession: agy.HostPrefix + id, Name: sp.name}, Mode: state.ModeBypass, Dir: dir, By: by, At: a.now.UTC(), Harness: agy.Harness, Model: model}
	var reg registration
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		var err error
		reg, err = recordStart(st, s, sp.task, sp.serves, func(p state.Party) bool { _, ok := claude.Live(sessions, p); return ok })
		if err != nil {
			return nil, err
		}
		return []state.Event{event(by, "agents.start", "%s: agy agent %s on %s in %s, dangerously-skip-permissions, busy with %q", sp.name, id, model, dir, reg.task)}, nil
	})
	if err != nil {
		return err
	}
	if err := agy.MakeInbox(inbox); err != nil {
		return err
	}
	unit := agyUnit(id)
	projID := agy.ProjectForDir(a.cfg.Agy.ProjectsDir, dir)
	argv := agyRunArgv(self, inbox, stream, agyArgv(bin, model, a.cfg.Agy.RemoteControl, projID))
	if err := launch(unit, dir, a.explicitConfig(), nil, argv,
		agy.EnvAgent+"="+id, agy.EnvName+"="+sp.name); err != nil {
		_, _ = agy.RemoveInbox(inbox)
		return fmt.Errorf("starting %s: %w (the start stays recorded; beekeeper agents remove %q takes it off the roster)", sp.name, err, sp.name)
	}
	if err := sendAgyWhenOpen(ctx, inbox, sp.brief, inboxWait); err != nil {
		_ = plat.Launcher.Stop(ctx, unit)
		_, _ = agy.RemoveInbox(inbox)
		return a.undelivered(by, s.Party, fmt.Errorf("the brief did not reach its inbox: %w (journalctl --user -u %s)", err, unit))
	}
	convID, err := awaitAgyBrief(ctx, stream, func() bool { return unitEnded(ctx, unit) }, agyBriefWait)
	if err != nil {
		_ = plat.Launcher.Stop(ctx, unit)
		_, _ = agy.RemoveInbox(inbox)
		return a.undelivered(by, s.Party, fmt.Errorf("%w (journalctl --user -u %s)", err, unit))
	}
	var remoteLine string
	if a.cfg.Agy.RemoteControl && a.cfg.Agy.RemoteDaemon != "" {
		remoteLine = a.notifyAgyRemoteDaemon(ctx, id, convID)
	}
	if _, err = fmt.Fprintf(a.out, "started %s: agy agent agy_%s (conversation %s), dangerously-skip-permissions, on %s, in %s, busy with %q\n"+
		"its brief is in its conversation; agents wake %q <message> reaches it before its next model call or as its next turn (journalctl --user -u %s)\n",
		sp.name, id, convID, model, dir, reg.task, sp.name, unit); err != nil {
		return err
	}
	if remoteLine != "" {
		_, err = fmt.Fprintln(a.out, remoteLine)
	}
	return err
}

// dryRunAgyStart prints how startAgyAgent would start sp and starts nothing.
func (a *app) dryRunAgyStart(sp agentStart, gaps []string) error {
	dir, err := a.agentDir(sp.dir)
	if err != nil {
		return err
	}
	sp.brief = projectPrompt(dir, sp.brief)
	model := cmp.Or(strings.TrimSpace(sp.model), "<model>")
	projID := agy.ProjectForDir(a.cfg.Agy.ProjectsDir, dir)
	argv := agyArgv(agy.Comm, model, a.cfg.Agy.RemoteControl, projID)
	if _, err := fmt.Fprintf(a.out, "dry run: %s would start in %s as an agy agent on %s, dangerously-skip-permissions, no desktop import\nstart: %s\n",
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

// agyModel validates the model an agy agent starts on: --model is required
// (no harness default) and must be one agy models lists.
func agyModel(flag string, listed func() ([]string, error)) (string, error) {
	model := strings.TrimSpace(flag)
	if model == "" {
		return "", usageErr("an agy agent needs its model: --model (agy models lists them)")
	}
	models, err := listed()
	if err != nil {
		return "", fmt.Errorf("listing agy's models: %w", err)
	}
	if !slices.Contains(models, model) {
		return "", usageErr("agy lists no model %q: name one as agy models lists it", model)
	}
	return model, nil
}

// agyModels returns the model selectors `agy models` lists.
func agyModels(ctx context.Context, bin string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, inboxWait)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "models").Output() //nolint:gosec // agy from PATH
	if err != nil {
		return nil, err
	}
	return agy.ParseModelsOutput(string(out)), nil
}

// agyArgv is an agy agent's command line: stream-json in and out, on its
// model, with --dangerously-skip-permissions, optional --remote-control, and
// --project <id> when known or --new-project otherwise.
func agyArgv(bin, model string, remoteControl bool, projectID string) []string {
	argv := []string{
		bin,
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		modelFlag, model,
		"--dangerously-skip-permissions",
	}
	if remoteControl {
		argv = append(argv, "--remote-control")
	}
	if projectID != "" {
		argv = append(argv, "--project", projectID)
	} else {
		argv = append(argv, "--new-project")
	}
	return argv
}

// agyRunArgv wraps agyArgs in beekeeper agy-run on inbox and stream.
func agyRunArgv(self, inbox, stream string, agyArgs []string) []string {
	return append([]string{self, "agy-run", inbox, stream}, agyArgs...)
}

// sendAgyWhenOpen queues msg in inbox once beekeeper agy-run opened it.
func sendAgyWhenOpen(ctx context.Context, inbox, msg string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := agy.Send(inbox, msg)
		if !errors.Is(err, agy.ErrNotRunning) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// awaitAgyBrief waits up to wait for the stream log at streamPath to show the
// started session's init event (conversation_id) and its first user_input
// step_update, confirming the brief arrived in its own conversation.
func awaitAgyBrief(ctx context.Context, streamPath string, ended func() bool, wait time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		convID, accepted, err := readAgyBriefStream(streamPath)
		if err != nil {
			return convID, err
		}
		if convID != "" && accepted {
			return convID, nil
		}
		if ended != nil && ended() {
			convID, accepted, err = readAgyBriefStream(streamPath)
			if err != nil {
				return convID, err
			}
			if convID != "" && accepted {
				return convID, nil
			}
			return convID, errors.New("agy ended before the brief entered its conversation")
		}
		select {
		case <-ctx.Done():
			return convID, fmt.Errorf("the brief did not enter the agy conversation within %s", wait)
		case <-tick.C:
		}
	}
}

func readAgyBriefStream(streamPath string) (convID string, accepted bool, err error) {
	f, oerr := os.Open(streamPath) //nolint:gosec // beekeeper's own stream log
	if oerr != nil {
		return "", false, nil
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var ev struct {
			Type           string `json:"type"`
			Event          string `json:"event"`
			ConversationID string `json:"conversation_id"`
			StepType       string `json:"step_type"`
			Status         string `json:"status"`
			IsError        bool   `json:"is_error"`
			Error          string `json:"error"`
			StepUpdate     *struct {
				ConversationID string `json:"conversation_id"`
				StepType       string `json:"step_type"`
			} `json:"step_update"`
			Result *struct {
				ConversationID string `json:"conversation_id"`
				Status         string `json:"status"`
				Error          string `json:"error"`
			} `json:"result"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		kind := cmp.Or(ev.Event, ev.Type)
		switch kind {
		case "init":
			if ev.ConversationID != "" {
				convID = ev.ConversationID
			}
		case "step_update":
			st := ev.StepType
			if ev.StepUpdate != nil {
				if convID == "" && ev.StepUpdate.ConversationID != "" {
					convID = ev.StepUpdate.ConversationID
				}
				st = cmp.Or(st, ev.StepUpdate.StepType)
			}
			if st == "user_input" {
				accepted = true
			}
		case "result":
			status := ev.Status
			errMsg := ev.Error
			if ev.Result != nil {
				status = cmp.Or(status, ev.Result.Status)
				errMsg = cmp.Or(errMsg, ev.Result.Error)
			}
			if ev.IsError || strings.HasPrefix(strings.ToLower(status), "err") {
				msg := strings.TrimSpace(errMsg)
				if msg == "" {
					msg = "agy reported a result error before accepting the brief"
				}
				return convID, false, errors.New(msg)
			}
		}
	}
	return convID, accepted, nil
}

// restartAgyRemoteDaemon runs `systemctl --user try-restart <unit>`; a seam
// for tests.
var restartAgyRemoteDaemon = func(ctx context.Context, unit string) error {
	return exec.CommandContext(ctx, "systemctl", "--user", "try-restart", unit).Run() //nolint:gosec // configured unit name
}

// notifyAgyRemoteDaemon waits briefly for <ConversationsDir>/<convID>.db,
// checks that no other running agy session is in a busy turn, and restarts
// the configured remote daemon unit so Antigravity's remote view picks up the
// new conversation, returning a status line describing what it did.
func (a *app) notifyAgyRemoteDaemon(ctx context.Context, startID, convID string) string {
	daemon := a.cfg.Agy.RemoteDaemon
	if convID != "" && a.cfg.Agy.ConversationsDir != "" {
		db := filepath.Join(a.cfg.Agy.ConversationsDir, convID+".db")
		deadline := time.Now().Add(agyRemoteDBWait)
		found := false
		for {
			if _, err := os.Stat(db); err == nil {
				found = true
				break
			}
			if !time.Now().Before(deadline) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !found {
			return fmt.Sprintf("remote daemon %s not restarted: %s.db did not appear in %s", daemon, convID, a.cfg.Agy.ConversationsDir)
		}
	}
	if sessions, _, err := a.sessions(); err == nil {
		var busy []string
		for _, s := range sessions {
			if s.Harness != agy.Harness || s.State != agy.StateBusy {
				continue
			}
			if (convID != "" && s.ID == convID) || (startID != "" && s.HostID == agy.HostPrefix+startID) {
				continue
			}
			label := cmp.Or(s.Name, s.ID, s.HostID)
			busy = append(busy, label)
		}
		if len(busy) > 0 {
			return fmt.Sprintf("remote daemon %s not restarted: agy session %s is busy mid-turn", daemon, strings.Join(busy, ", "))
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := restartAgyRemoteDaemon(cctx, daemon); err != nil {
		return fmt.Sprintf("remote daemon %s restart failed: %v", daemon, err)
	}
	return fmt.Sprintf("restarted remote daemon %s for conversation %s", daemon, convID)
}

// endAgy stops the unit of the agy agent started under id, which left the
// roster, and removes its inbox and state files, and says what it did.
func (a *app) endAgy(ctx context.Context, id string) string {
	var done []string
	if unit := agyUnit(id); plat.Launcher.State(ctx, unit) == holdActive {
		if err := plat.Launcher.Stop(ctx, unit); err != nil {
			return fmt.Sprintf("its agy unit %s runs on: %v", unit, err)
		}
		done = append(done, "stopped its agy unit "+unit)
	}
	removed, err := agy.RemoveInbox(agy.InboxPath(a.cfg.StateDir, id))
	switch {
	case err != nil:
		done = append(done, fmt.Sprintf("its agy inbox stays: %v", err))
	case removed:
		done = append(done, "removed its agy inbox")
	}
	if len(done) == 0 {
		return "an agy agent has no desktop session"
	}
	return strings.Join(done, ", ")
}
