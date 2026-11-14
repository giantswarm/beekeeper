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
	"github.com/giantswarm/beekeeper/internal/watch"
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
//
// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestTheWatchStopsARevivedPredecessor(t *testing.T) {
	const name = "BK 37984 serve on gazelle"
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	watchApp(w.Host).now = time.Now()
	l := &stopLauncher{units: []string{"beekeeper-reopen-9b9ca177-1"}}
	useLauncher(t, l)
	ended := useEndRevived(t)

	old := state.Party{Session: "9b9ca177-old", HostSession: "local_9b9ca177-old", Name: name}
	next := state.Party{Session: "b8a904ab-new", HostSession: "local_b8a904ab-new", Name: name}
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = []state.Start{{Party: old, At: watchApp(w.Host).now.Add(-3 * time.Hour)}, {Party: next, At: watchApp(w.Host).now.Add(-time.Minute)}}
		st.Agents = []state.Agent{{Party: next, Task: "serve"}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	oldCLI := &claude.Session{PID: 1580801, ID: old.Session, HostID: old.HostSession, Name: name}
	nextCLI := &claude.Session{PID: 2, ID: next.Session, HostID: next.HostSession, Name: name}
	person := &claude.Session{PID: 3, ID: "own-notes", Name: keyNotes}

	left := w.StopRevived(t.Context(), []*claude.Session{oldCLI, nextCLI, person})
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
	st, err := watchApp(w.Host).store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Archives) != 1 || st.Archives[0].Host != old.HostSession {
		t.Errorf("archives %+v, want the predecessor's desktop session owed", st.Archives)
	}
	evs, err := watchApp(w.Host).store.Events(0, func(e state.Event) bool { return e.Verb == "agents.revived" })
	if err != nil || len(evs) != 1 {
		t.Errorf("agents.revived events %v, %v", evs, err)
	}

	// Gone, it stays gone; the follow-up is never stopped.
	w.StopRevived(t.Context(), []*claude.Session{nextCLI})
	if len(*ended) != 1 {
		t.Errorf("stopped %v after the predecessor was gone", *ended)
	}
	// Revived once more under a new CLI, it is stopped again.
	again := *oldCLI
	again.PID = 1580999
	w.StopRevived(t.Context(), []*claude.Session{&again, nextCLI})
	if len(*ended) != 2 || (*ended)[1] != again.PID {
		t.Errorf("stopped %v, want the second revival %d too", *ended, again.PID)
	}
}

// The CLI of a handed-over session that the doctor warmed to archive its
// own desktop session runs on for watch.ArchiveWarmGrace: the watch neither stops
// it as revived nor says SAME TITLE beside its follow-up, so its archive
// call runs. Past the grace it is stopped like any revived CLI.
//
// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestTheWatchLeavesACLIWarmedForItsArchive(t *testing.T) {
	const name = "BS 2867 provider connect prompt"
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	watchApp(w.Host).now = time.Now()
	useLauncher(t, &stopLauncher{})
	ended := useEndRevived(t)

	old := state.Party{Session: "6d5d9d3c-old", HostSession: "local_6d5d9d3c-old", Name: name}
	next := state.Party{Session: "a1b2c3d4-new", HostSession: "local_a1b2c3d4-new", Name: name}
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = []state.Start{{Party: old, At: watchApp(w.Host).now.Add(-3 * time.Hour)}, {Party: next, At: watchApp(w.Host).now.Add(-time.Minute)}}
		st.Agents = []state.Agent{{Party: next, Task: "connect prompt"}}
		oweArchive(st, old, "handed over to session "+next.Session, watchApp(w.Host).now)
		st.Archives[0].Warmed = watchApp(w.Host).now.Add(-time.Minute)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	oldCLI := &claude.Session{PID: 4711, ID: old.Session, HostID: old.HostSession, Name: name}
	nextCLI := &claude.Session{PID: 2, ID: next.Session, HostID: next.HostSession, Name: name}

	left := w.StopRevived(t.Context(), []*claude.Session{oldCLI, nextCLI})
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

	watchApp(w.Host).now = watchApp(w.Host).now.Add(watch.ArchiveWarmGrace)
	w.StopRevived(t.Context(), []*claude.Session{oldCLI, nextCLI})
	if len(*ended) != 1 || (*ended)[0] != oldCLI.PID {
		t.Errorf("stopped %v past the grace, want the warmed CLI %d", *ended, oldCLI.PID)
	}
}

// A relieved run whose CLI the desktop or the person brings back runs on,
// right after the relay and long after it: the person decides when its
// session goes (https://github.com/giantswarm/beekeeper/issues/2195).
//
// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestTheWatchLeavesARelievedRunRunning(t *testing.T) {
	w, _, _ := notifyingWatch(t, t.TempDir(), false)
	useLauncher(t, &stopLauncher{})
	ended := useEndRevived(t)
	taken := time.Now()
	out := state.Party{Session: "sup-81", HostSession: "local_sup-81", Name: "Guide run 81"}
	succ := state.Party{Session: "sup-82", HostSession: "local_sup-82", Name: "Guide run 82"}
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = []state.Start{{Party: out, At: taken.Add(-time.Hour), Role: guideName}, {Party: succ, At: taken.Add(-time.Minute), Role: guideName}}
		guideRole.Set(st, state.Role{
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
	for _, at := range []time.Duration{4 * time.Second, 2 * time.Minute, 48 * time.Hour} {
		watchApp(w.Host).now = taken.Add(at)
		left := w.StopRevived(t.Context(), []*claude.Session{outCLI, succCLI})
		if len(*ended) != 0 || len(left) != 2 {
			t.Fatalf("%s after the relay: stopped %v, left %d CLIs", at, *ended, len(left))
		}
	}
	st, err := watchApp(w.Host).store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Archives) != 0 {
		t.Errorf("owes archives %+v, want none", st.Archives)
	}
}

// Once the warmed CLI of a handed-over session archived its desktop session,
// the doctor owes it nothing and the warm's mark is gone: the watch stops the
// CLI as done, logged as agents.archive, never REVIVED, and owes no archive.
//
// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestTheWatchStopsAnArchivedSessionsCLIQuietly(t *testing.T) {
	const name = "WS 38113 backstage workspaces"
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	watchApp(w.Host).now = time.Now()
	useLauncher(t, &stopLauncher{})
	ended := useEndRevived(t)
	old := state.Party{Session: "16140721-old", HostSession: "local_16140721-old", Name: name}
	next := state.Party{Session: "52b403dd-new", HostSession: "local_52b403dd-new", Name: name}
	orig := watch.DesktopArchived
	t.Cleanup(func() { watch.DesktopArchived = orig })
	watch.DesktopArchived = func(_ *config.Config, host string) bool { return host == old.HostSession }
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = []state.Start{{Party: old, At: watchApp(w.Host).now.Add(-3 * time.Hour)}, {Party: next, At: watchApp(w.Host).now.Add(-time.Minute)}}
		st.Agents = []state.Agent{{Party: next, Task: "workspaces"}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	oldCLI := &claude.Session{PID: 377302, ID: old.Session, HostID: old.HostSession, Name: name}
	nextCLI := &claude.Session{PID: 2, ID: next.Session, HostID: next.HostSession, Name: name}

	left := w.StopRevived(t.Context(), []*claude.Session{oldCLI, nextCLI})
	if len(*ended) != 1 || (*ended)[0] != oldCLI.PID {
		t.Fatalf("stopped %v, want the archived session's CLI %d", *ended, oldCLI.PID)
	}
	if len(left) != 1 || left[0] != nextCLI {
		t.Errorf("left %v, want only the follow-up", left)
	}
	if s := out.String(); strings.Contains(s, "REVIVED") {
		t.Errorf("output:\n%s", s)
	}
	st, err := watchApp(w.Host).store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Archives) != 0 {
		t.Errorf("archives %+v, want none owed for an archived session", st.Archives)
	}
	revived, _ := watchApp(w.Host).store.Events(0, func(e state.Event) bool { return e.Verb == "agents.revived" })
	archive, _ := watchApp(w.Host).store.Events(0, func(e state.Event) bool { return e.Verb == "agents.archive" })
	if len(revived) != 0 || len(archive) != 1 || !strings.Contains(archive[0].Detail, "is archived") {
		t.Errorf("agents.revived %v, agents.archive %v", revived, archive)
	}
}
