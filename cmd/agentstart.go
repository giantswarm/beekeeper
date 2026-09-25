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
	// transcriptWait bounds the wait for a started session's transcript,
	// which the import reads.
	transcriptWait = time.Minute
	// maxBrief keeps the brief within one command-line argument.
	maxBrief = 100 << 10
)

func (a *app) agentStartCmd() *cobra.Command {
	var model, dir string
	c := &cobra.Command{
		Use:   "start <name> <brief file>",
		Short: "Start an agent session in bypass from the command line and import it into the desktop",
		Long: `start starts a Claude Code session without a click: "claude -p" in
bypassPermissions under a session id beekeeper chooses, with the brief file
as its first prompt, in a transient user unit (beekeeper-agent-<id>, its
output in journalctl --user -u <unit>) that the caller's session, scope or
terminal do not take down. Before the session exists, beekeeper records its
id and mode as one of its starts and registers it on the roster under
<name>, busy with the brief's first line (or with the open task of a
stopped session's entry under that name, which it takes over). Once the
transcript is on disk it imports the session into Claude Desktop
(claude://resume?session=<id>): it shows in the sidebar as local_<id> and
takes messages there.

Its first turn runs the brief from the command line in bypass. The desktop
runs every later turn in acceptEdits (its import always drops bypass), so
requests no allow rule covers would stop at a card: beekeeper hook
permissionrequest answers them, for beekeeper's starts only. Send it
nothing while its first turn runs (beekeeper agents shows it live).`,
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
			if dir, err = filepath.Abs(dir); err != nil {
				return err
			}
			if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
				return usageErr("--dir %s: not a directory", dir)
			}
			bin, err := exec.LookPath("claude")
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
			live := func(p state.Party) bool {
				_, ok := claude.Live(sessions, p)
				return ok
			}
			id := uuid.NewString()
			s := state.Start{Party: state.Party{Session: id, HostSession: "local_" + id, Name: name}, Mode: state.ModeBypass, Dir: dir, By: by, At: a.now.UTC()}
			var reg registration
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				var err error
				reg, err = recordStart(st, s, briefTask(brief), live)
				if err != nil {
					return nil, err
				}
				return []state.Event{event(by, "agents.start", "%s: session %s in %s, bypassPermissions, busy with %q", name, id, dir, reg.task)}, nil
			})
			if err != nil {
				return err
			}
			unit := "beekeeper-agent-" + id[:8]
			if err := launch(unit, dir, agentArgv(bin, id, name, model, brief)); err != nil {
				return fmt.Errorf("starting %s: %w (the start stays recorded; beekeeper agents remove %q takes it off the roster)", name, err, name)
			}
			if err := a.awaitTranscript(cmd.Context(), id, unit); err != nil {
				return err
			}
			t, err := proc.Read()
			if err != nil {
				return err
			}
			if err := openDesktop(cmd.Context(), resumeURL(id), !desktopStart(t).IsZero()); err != nil {
				return fmt.Errorf("importing %s into the desktop: %w", id, err)
			}
			_, err = fmt.Fprintf(a.out, "started %s: session %s, desktop local_%s, bypassPermissions, in %s, busy with %q\n"+
				"its first turn runs from the command line (journalctl --user -u %s); later turns are desktop turns in acceptEdits\n",
				name, id, id, dir, reg.task, unit)
			return err
		},
	}
	c.Flags().StringVar(&model, "model", "", "the session's model (default: Claude Code's)")
	c.Flags().StringVar(&dir, "dir", ".", "the session's working directory")
	return c
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
	if reg.task == "" {
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
// environment, not the caller's session variables, and outlives the caller.
// KillMode=process leaves what the turn started running when it ends, as a
// terminal would.
func launch(unit, dir string, argv []string) error {
	args := append([]string{"--user", "--collect", "--quiet", "--unit=" + unit, "-p", "KillMode=process",
		"--working-directory=" + dir, "--"}, argv...)
	out, err := exec.Command("systemd-run", args...).CombinedOutput() //nolint:gosec // starting the session is the purpose
	if err != nil {
		return fmt.Errorf("systemd-run: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// awaitTranscript waits up to transcriptWait for the session's transcript,
// and fails early once its unit has ended without one.
func (a *app) awaitTranscript(ctx context.Context, id, unit string) error {
	ctx, cancel := context.WithTimeout(ctx, transcriptWait)
	defer cancel()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	written := func() bool {
		m, _ := filepath.Glob(filepath.Join(a.cfg.Claude.ProjectsDir, "*", id+".jsonl"))
		return len(m) > 0
	}
	for {
		if written() {
			return nil
		}
		out, _ := exec.CommandContext(ctx, "systemctl", "--user", "show", "-p", "ActiveState", "--value", unit).Output() //nolint:gosec // the unit beekeeper named
		if s := strings.TrimSpace(string(out)); (s == "inactive" || s == "failed") && !written() {
			return fmt.Errorf("session %s ended without a transcript: journalctl --user -u %s", id, unit)
		}
		select {
		case <-ctx.Done():
			return errors.Join(fmt.Errorf("no transcript for session %s after %s: journalctl --user -u %s", id, transcriptWait, unit), ctx.Err())
		case <-tick.C:
		}
	}
}
