package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// waker wakes the sessions of the twin tests; twinName and twinHost name the
// session of TestTheWatchSaysATwinCLI.
var waker = state.Party{Name: "waker"}

const (
	twinName = "Twin worker"
	twinHost = "local_s1"
)

// wakeTwinSetup is an app whose machine runs the desktop's CLI (PID 7) of
// session id once warm is set, a socket file for it, a fake claude on PATH,
// and a recording launcher; sent collects the sends to a running CLI.
func wakeTwinSetup(t *testing.T, id string, warm bool) (*app, *noteLauncher, *[]string) {
	t.Helper()
	a, _ := stubApp(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // a fake claude
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	run := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", run)
	writeFile(t, peerSocket(run, 7), "")
	w := &atomic.Bool{}
	w.Store(warm)
	plat.Machine = warmMachine{Machine: plat.Machine, id: id, warm: w}
	l := &noteLauncher{}
	plat.Launcher = l
	var sent []string
	saved := cliSend
	t.Cleanup(func() { cliSend = saved })
	cliSend = func(_ context.Context, _ *app, to, msg string) error {
		sent = append(sent, to+" "+msg)
		return nil
	}
	return a, l, &sent
}

// A wake whose session's desktop CLI started after the wake looked sends
// the message to that CLI: no headless turn starts beside it.
func TestAWakeGoesToTheDesktopCLIThatRuns(t *testing.T) {
	const id = "twin-1"
	a, l, sent := wakeTwinSetup(t, id, true)
	w := wakeTarget{name: "Twin", id: id, dir: t.TempDir(), mode: state.ModeBypass}
	if err := a.resumeTurn(t.Context(), waker, w, "go on"); err != nil {
		t.Fatal(err)
	}
	if len(l.units) != 0 {
		t.Errorf("started %d headless units beside the desktop CLI", len(l.units))
	}
	if want := "uds:" + peerSocket(os.Getenv("XDG_RUNTIME_DIR"), 7) + " go on"; len(*sent) != 1 || (*sent)[0] != want {
		t.Errorf("sent %q, want %q", *sent, want)
	}
}

// With no desktop CLI the wake resumes the session headless, as before.
func TestAWakeWithoutADesktopCLIRunsHeadless(t *testing.T) {
	const id = "twin-2"
	a, l, sent := wakeTwinSetup(t, id, false)
	w := wakeTarget{name: "Twin", id: id, dir: t.TempDir(), mode: state.ModeBypass}
	if err := a.resumeTurn(t.Context(), waker, w, "go on"); err != nil {
		t.Fatal(err)
	}
	if len(l.units) != 1 || len(*sent) != 0 {
		t.Errorf("units %d, sends %q: want one headless unit, no send", len(l.units), *sent)
	}
}

// A desktop send, which starts the desktop's CLI, waits for the session's
// headless turn to end and is refused while it still runs.
func TestADesktopSendWaitsForTheHeadlessTurn(t *testing.T) {
	a, _ := stubApp(t)
	plat.Machine = turnMachine{Machine: plat.Machine}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	_, err := a.sendThroughDesktop(ctx, "local_"+firstTurn, "msg", func() bool { return false })
	if err == nil || !strings.Contains(err.Error(), "headless first turn still runs") {
		t.Errorf("a desktop send beside a headless turn: %v", err)
	}
	if err := awaitNoHeadless(t.Context(), onScreen, time.Second); err != nil {
		t.Errorf("a session with only its desktop CLI: %v", err)
	}
}

// The watch says a session that runs two CLIs once, naming both, and its end
// once one is left. Discover lists the session once; the process table holds
// both CLIs. A CLI younger than twinGrace (a restart's overlap) counts not
// yet.
func TestTheWatchSaysATwinCLI(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	w.now = time.Now()
	old := w.now.Add(-time.Hour)
	sessions := []*claude.Session{{PID: 11, ID: "s1", HostID: twinHost, Name: twinName}, {PID: 12, ID: "o", Name: "someone else"}}
	head := &proc.Process{PID: 10, Comm: claudeComm, Start: old, Args: []string{claudeComm, "-p", resumeFlag, "s1", "--", "msg"}}
	desk := &proc.Process{PID: 11, Comm: claudeComm, Start: old, Args: []string{claudeComm, "--resume=s1"}}
	other := &proc.Process{PID: 12, Comm: claudeComm, Start: old, Args: []string{claudeComm, resumeFlag, "o"}}
	table := func(ps ...*proc.Process) *proc.Table {
		tb := &proc.Table{ByPID: map[int]*proc.Process{}}
		for _, p := range ps {
			tb.ByPID[p.PID] = p
		}
		return tb
	}
	young := *head
	young.Start = w.now.Add(-time.Second)
	w.twins(sessions, table(&young, desk, other))
	if strings.Contains(out.String(), "TWIN CLI") {
		t.Errorf("a restart's overlap counts as a twin:\n%s", out)
	}
	w.twins(sessions, table(head, desk, other))
	w.twins(sessions, table(head, desk, other))
	if s := out.String(); strings.Count(s, "TWIN CLI") != 1 ||
		!strings.Contains(s, `"Twin worker" runs 2 CLIs on session s1 (PID 10 in `) || !strings.Contains(s, "PID 11 in ") {
		t.Errorf("output:\n%s", s)
	}
	w.twins(sessions, table(desk, other))
	if !strings.Contains(out.String(), "ENDED TWIN CLI") {
		t.Errorf("no end once one CLI is left:\n%s", out)
	}
}
