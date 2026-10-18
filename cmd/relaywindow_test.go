package cmd

import (
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A message to "the supervisor" during a relay reaches a run that takes it:
// the outgoing holder until the successor's start, the outgoing holder still
// while the successor's CLI takes no message after it (its headless first
// turn ended, its desktop CLI not up), then the successor. Never an older
// relieved run, and never a bare run name nothing answers.
func TestSupervisorAliasThroughTheRelayWindow(t *testing.T) {
	at := time.Date(2026, 10, 18, 1, 7, 0, 0, time.UTC)
	window := 15 * time.Minute
	older := state.Party{Session: "sup-221", HostSession: "local_sup-221", Name: "Supervisor run 221"}
	out := state.Party{Session: "sup-222", HostSession: "local_sup-222", Name: "Supervisor run 222"}
	succ := state.Party{Session: "sup-223", HostSession: "local_sup-223", Name: "Supervisor run 223"}

	st := &state.State{}
	supervisorRole.set(st, state.Role{
		Holder:   &state.Supervisor{Party: out, Since: at.Add(-2 * time.Hour)},
		Relieved: []state.Relief{{Party: older, By: out, At: at.Add(-2 * time.Hour), Taken: at.Add(-2 * time.Hour)}},
		Run:      222,
	})
	if _, _, err := relayRole(st, out, succ, at, window); err != nil {
		t.Fatal(err)
	}

	olderCLI := &claude.Session{ID: older.Session, HostID: older.HostSession, Name: older.Name, PID: 221}
	outCLI := &claude.Session{ID: out.Session, HostID: out.HostSession, Name: out.Name, PID: 222}
	succCLI := &claude.Session{ID: succ.Session, Name: succ.Name, PID: 223}
	sockets := map[int]string{221: "/run/221.sock", 222: "/run/222.sock", 223: "/run/223.sock"}
	sock := func(pid int) string { return sockets[pid] }
	noSock223 := func(pid int) string {
		if pid == 223 {
			return ""
		}
		return sockets[pid]
	}
	check := func(step string, now time.Time, sessions []*claude.Session, sock func(int) string, want string) {
		t.Helper()
		got, err := relayAddress(supervisorRole.get(st), supervisorRole, sessions, sock, now, window)
		if err != nil || got != want {
			t.Errorf("%s: %q, %v; want %q", step, got, err, want)
		}
	}

	// The relay is open, the successor's headless first turn runs.
	check("relay open", at.Add(5*time.Second), []*claude.Session{olderCLI, outCLI, succCLI}, sock, out.Name)

	// The start runs in the headless first turn, whose caller knows no
	// desktop id: the holder keeps the relay's.
	headless := succ
	headless.HostSession = ""
	taken := at.Add(10 * time.Second)
	if _, _, err := startRole(st, headless, true, false, taken); err != nil {
		t.Fatal(err)
	}
	if h := supervisorRole.get(st).Holder; h.HostSession != succ.HostSession {
		t.Fatalf("holder's desktop id %q, want the relay's %q", h.HostSession, succ.HostSession)
	}

	check("start done, headless turn still up", taken.Add(time.Second), []*claude.Session{olderCLI, outCLI, succCLI}, sock, succ.Name)
	check("headless turn ended, desktop CLI not up", taken.Add(20*time.Second), []*claude.Session{olderCLI, outCLI}, sock, out.Name)
	check("desktop CLI up, no socket yet", taken.Add(25*time.Second), []*claude.Session{olderCLI, outCLI, succCLI}, noSock223, out.Name)
	check("desktop CLI takes messages", taken.Add(30*time.Second), []*claude.Session{olderCLI, outCLI, succCLI}, sock, succ.Name)

	// The outgoing holder gone too: the successor's desktop id, which the
	// desktop starts, never the older relieved run that still runs.
	check("outgoing gone", taken.Add(20*time.Second), []*claude.Session{olderCLI}, sock, succ.HostSession)
	// Past the window a relieved run gets nothing.
	check("window over", taken.Add(window+time.Second), []*claude.Session{olderCLI, outCLI}, sock, succ.HostSession)
}
