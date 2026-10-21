package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/state"
)

// stopLauncher has no unit of any session running and records the stops.
type stopLauncher struct {
	platform.Launcher
	units   []string
	stopped []string
}

func (l *stopLauncher) Running(context.Context, bool, ...string) []string { return l.units }

func (l *stopLauncher) Stop(_ context.Context, unit string) error {
	l.stopped = append(l.stopped, unit)
	return nil
}

// useEndRevived records the CLIs the watch stops instead of signalling them.
func useEndRevived(t *testing.T) *[]int {
	t.Helper()
	var ended []int
	orig := endRevived
	endRevived = func(_ context.Context, s *claude.Session) (int, error) {
		ended = append(ended, s.PID)
		return 1, nil
	}
	t.Cleanup(func() { endRevived = orig })
	return &ended
}

// A CLI of a handed-over session the desktop revived beside its follow-up
// under the shared name is stopped by the watch, with its reopen units: one
// session answers to the name again, no SAME TITLE is said, and the doctor
// owes its desktop session the archive. The follow-up and a person's own
// session of the same title run on.
func TestTheWatchStopsARevivedPredecessor(t *testing.T) {
	const name = "BK 37984 serve on gazelle"
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	w.now = time.Now()
	l := &stopLauncher{units: []string{"beekeeper-reopen-9b9ca177-1"}}
	useLauncher(t, l)
	ended := useEndRevived(t)

	old := state.Party{Session: "9b9ca177-old", HostSession: "local_9b9ca177-old", Name: name}
	next := state.Party{Session: "b8a904ab-new", HostSession: "local_b8a904ab-new", Name: name}
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = []state.Start{{Party: old, At: w.now.Add(-3 * time.Hour)}, {Party: next, At: w.now.Add(-time.Minute)}}
		st.Agents = []state.Agent{{Party: next, Task: "serve"}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	oldCLI := &claude.Session{PID: 1580801, ID: old.Session, HostID: old.HostSession, Name: name}
	nextCLI := &claude.Session{PID: 2, ID: next.Session, HostID: next.HostSession, Name: name}
	person := &claude.Session{PID: 3, ID: "own-notes", Name: "notes"}

	left := w.stopRevived(t.Context(), []*claude.Session{oldCLI, nextCLI, person})
	if len(*ended) != 1 || (*ended)[0] != oldCLI.PID {
		t.Fatalf("stopped %v, want only the revived CLI %d", *ended, oldCLI.PID)
	}
	if len(left) != 2 || left[0] != nextCLI || left[1] != person {
		t.Errorf("left %v, want the follow-up and the person's session", left)
	}
	if d := sameTitles(left); len(d) != 0 {
		t.Errorf("a shared title is left: %v", d)
	}
	if len(l.stopped) == 0 {
		t.Error("its reopen units run on: the desktop would warm it again")
	}
	if s := out.String(); !strings.Contains(s, `REVIVED "BK 37984 serve on gazelle": session 9b9ca177-old's CLI 1580801`) || !strings.Contains(s, "session b8a904ab-new") {
		t.Errorf("output:\n%s", s)
	}
	st, err := w.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Archives) != 1 || st.Archives[0].Host != old.HostSession {
		t.Errorf("archives %+v, want the predecessor's desktop session owed", st.Archives)
	}
	evs, err := w.store.Events(0, func(e state.Event) bool { return e.Verb == "agents.revived" })
	if err != nil || len(evs) != 1 {
		t.Errorf("agents.revived events %v, %v", evs, err)
	}

	// Gone, it stays gone; the follow-up is never stopped.
	w.stopRevived(t.Context(), []*claude.Session{nextCLI})
	if len(*ended) != 1 {
		t.Errorf("stopped %v after the predecessor was gone", *ended)
	}
	// Revived once more under a new CLI, it is stopped again.
	again := *oldCLI
	again.PID = 1580999
	w.stopRevived(t.Context(), []*claude.Session{&again, nextCLI})
	if len(*ended) != 2 || (*ended)[1] != again.PID {
		t.Errorf("stopped %v, want the second revival %d too", *ended, again.PID)
	}
}

// The CLI of a handed-over session that the doctor warmed to archive its
// own desktop session runs on for archiveWarmGrace: the watch neither stops
// it as revived nor says SAME TITLE beside its follow-up, so its archive
// call runs. Past the grace it is stopped like any revived CLI.
func TestTheWatchLeavesACLIWarmedForItsArchive(t *testing.T) {
	const name = "BS 2867 provider connect prompt"
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	w.now = time.Now()
	useLauncher(t, &stopLauncher{})
	ended := useEndRevived(t)

	old := state.Party{Session: "6d5d9d3c-old", HostSession: "local_6d5d9d3c-old", Name: name}
	next := state.Party{Session: "a1b2c3d4-new", HostSession: "local_a1b2c3d4-new", Name: name}
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = []state.Start{{Party: old, At: w.now.Add(-3 * time.Hour)}, {Party: next, At: w.now.Add(-time.Minute)}}
		st.Agents = []state.Agent{{Party: next, Task: "connect prompt"}}
		oweArchive(st, old, "handed over to session "+next.Session, w.now)
		st.Archives[0].Warmed = w.now.Add(-time.Minute)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	oldCLI := &claude.Session{PID: 4711, ID: old.Session, HostID: old.HostSession, Name: name}
	nextCLI := &claude.Session{PID: 2, ID: next.Session, HostID: next.HostSession, Name: name}

	left := w.stopRevived(t.Context(), []*claude.Session{oldCLI, nextCLI})
	if len(*ended) != 0 {
		t.Fatalf("stopped %v, a CLI warmed for its archive", *ended)
	}
	if len(left) != 1 || left[0] != nextCLI {
		t.Errorf("left %v, want only the follow-up answering to the name", left)
	}
	if d := sameTitles(left); len(d) != 0 {
		t.Errorf("a shared title is left: %v", d)
	}
	if s := out.String(); strings.Contains(s, "REVIVED") {
		t.Errorf("output:\n%s", s)
	}

	w.now = w.now.Add(archiveWarmGrace)
	w.stopRevived(t.Context(), []*claude.Session{oldCLI, nextCLI})
	if len(*ended) != 1 || (*ended)[0] != oldCLI.PID {
		t.Errorf("stopped %v past the grace, want the warmed CLI %d", *ended, oldCLI.PID)
	}
}

// A relieved supervisor run whose CLI the desktop keeps or revives is
// stopped once reliefGrace has passed since its successor took the relay
// and it runs no tool command; the holder never is.
func TestTheWatchStopsARelievedRunPastItsGrace(t *testing.T) {
	w, _, _ := notifyingWatch(t, t.TempDir(), false)
	useLauncher(t, &stopLauncher{})
	ended := useEndRevived(t)
	taken := time.Now()
	out := state.Party{Session: "sup-81", HostSession: "local_sup-81", Name: "Supervisor run 81"}
	succ := state.Party{Session: "sup-82", HostSession: "local_sup-82", Name: "Supervisor run 82"}
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = []state.Start{{Party: out, At: taken.Add(-time.Hour)}, {Party: succ, At: taken.Add(-time.Minute)}}
		supervisorRole.set(st, state.Role{
			Holder:   &state.Supervisor{Party: succ, Since: taken},
			Relay:    &state.Relay{From: out, To: succ, At: taken.Add(-time.Minute), Expires: taken.Add(time.Hour), Taken: taken},
			Relieved: []state.Relief{{Party: out, By: succ, At: taken.Add(-time.Minute), Taken: taken}},
		})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	outCLI := &claude.Session{PID: 81, ID: out.Session, HostID: out.HostSession, Name: out.Name}
	succCLI := &claude.Session{PID: 82, ID: succ.Session, HostID: succ.HostSession, Name: succ.Name}

	w.now = taken.Add(reliefGrace / 2)
	w.stopRevived(t.Context(), []*claude.Session{outCLI, succCLI})
	if len(*ended) != 0 {
		t.Fatalf("stopped %v within the relief's grace", *ended)
	}
	w.now = taken.Add(reliefGrace + time.Second)
	busy := *outCLI
	busy.Commands = []claude.Command{{PID: 811}}
	w.stopRevived(t.Context(), []*claude.Session{&busy, succCLI})
	if len(*ended) != 0 {
		t.Fatalf("stopped %v while it runs a tool command", *ended)
	}
	w.stopRevived(t.Context(), []*claude.Session{outCLI, succCLI})
	if len(*ended) != 1 || (*ended)[0] != outCLI.PID {
		t.Errorf("stopped %v, want only the relieved run %d", *ended, outCLI.PID)
	}
}

// Once the warmed CLI of a handed-over session archived its desktop session,
// the doctor owes it nothing and the warm's mark is gone: the watch stops the
// CLI as done, logged as agents.archive, never REVIVED, and owes no archive.
func TestTheWatchStopsAnArchivedSessionsCLIQuietly(t *testing.T) {
	const name = "WS 38113 backstage workspaces"
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	w.now = time.Now()
	useLauncher(t, &stopLauncher{})
	ended := useEndRevived(t)
	old := state.Party{Session: "16140721-old", HostSession: "local_16140721-old", Name: name}
	next := state.Party{Session: "52b403dd-new", HostSession: "local_52b403dd-new", Name: name}
	orig := desktopArchived
	t.Cleanup(func() { desktopArchived = orig })
	desktopArchived = func(_ *config.Config, host string) bool { return host == old.HostSession }
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = []state.Start{{Party: old, At: w.now.Add(-3 * time.Hour)}, {Party: next, At: w.now.Add(-time.Minute)}}
		st.Agents = []state.Agent{{Party: next, Task: "workspaces"}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	oldCLI := &claude.Session{PID: 377302, ID: old.Session, HostID: old.HostSession, Name: name}
	nextCLI := &claude.Session{PID: 2, ID: next.Session, HostID: next.HostSession, Name: name}

	left := w.stopRevived(t.Context(), []*claude.Session{oldCLI, nextCLI})
	if len(*ended) != 1 || (*ended)[0] != oldCLI.PID {
		t.Fatalf("stopped %v, want the archived session's CLI %d", *ended, oldCLI.PID)
	}
	if len(left) != 1 || left[0] != nextCLI {
		t.Errorf("left %v, want only the follow-up", left)
	}
	if s := out.String(); strings.Contains(s, "REVIVED") {
		t.Errorf("output:\n%s", s)
	}
	st, err := w.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Archives) != 0 {
		t.Errorf("archives %+v, want none owed for an archived session", st.Archives)
	}
	revived, _ := w.store.Events(0, func(e state.Event) bool { return e.Verb == "agents.revived" })
	archive, _ := w.store.Events(0, func(e state.Event) bool { return e.Verb == "agents.archive" })
	if len(revived) != 0 || len(archive) != 1 || !strings.Contains(archive[0].Detail, "is archived") {
		t.Errorf("agents.revived %v, agents.archive %v", revived, archive)
	}
}
