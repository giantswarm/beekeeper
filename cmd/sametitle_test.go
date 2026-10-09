package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	sharedTitle = "Supervisor run 82"
	holderHost  = "local_9"
	firstRun    = "Supervisor run 1"
)

// noSocket is a machine where no CLI takes peer messages on a socket.
func noSocket(int) string { return "" }

// liveMachine runs a Claude Code CLI per entry of titles (PID: title), each
// with its record in dir, the sessions directory: Discover lists one live
// session s<PID> per CLI under its title.
type liveMachine struct {
	platform.Machine
	titles map[int]string
}

func (m liveMachine) Processes() (*proc.Table, error) {
	t := &proc.Table{ByPID: map[int]*proc.Process{}}
	for pid := range m.titles {
		t.ByPID[pid] = &proc.Process{PID: pid, Comm: claudeComm, Args: []string{claudeComm}, Start: time.Now().Add(-time.Hour), StartTicks: int64(pid) * 100}
	}
	return t, nil
}

// runLive has a's machine run a CLI per entry of titles.
func runLive(t *testing.T, a *app, titles map[int]string) {
	t.Helper()
	if err := os.MkdirAll(a.cfg.Claude.SessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for pid, title := range titles {
		rec := fmt.Sprintf(`{"pid":%d,"sessionId":"s%d","name":%q,"procStart":"%d","kind":"interactive"}`, pid, pid, title, pid*100)
		writeFile(t, filepath.Join(a.cfg.Claude.SessionsDir, fmt.Sprintf("%d.json", pid)), rec)
	}
	plat.Machine = liveMachine{Machine: plat.Machine, titles: titles}
}

// Two live sessions carrying one title (ignoring case and spaces) are one
// finding naming both; a title one session carries is none.
func TestSameTitles(t *testing.T) {
	sessions := []*claude.Session{
		{PID: 9, ID: "b", Name: "supervisor  RUN 82"},
		{PID: 3, ID: "a", Name: sharedTitle},
		{PID: 5, ID: "c", Name: agentFour},
		{PID: 6, ID: "d", Name: ""},
		{PID: 7, ID: "e", Name: ""},
	}
	ds := sameTitles(sessions)
	if len(ds) != 1 || ds[0].title != sharedTitle || ds[0].sessions[0].PID != 3 || ds[0].sessions[1].PID != 9 {
		t.Fatalf("findings %+v", ds)
	}
	if s := ds[0].String(); !strings.Contains(s, `2 live sessions are titled "Supervisor run 82" (PID 3 session a, PID 9 session b)`) {
		t.Errorf("line %q", s)
	}
}

// A title another live session carries is refused with that session named;
// my own session carrying it, or nobody, is no refusal.
func TestTitleTaken(t *testing.T) {
	me := state.Party{Session: "a", Name: sharedTitle}
	sessions := []*claude.Session{{PID: 3, ID: "a", Name: sharedTitle}}
	if err := titleTaken(sessions, sharedTitle, me); err != nil {
		t.Errorf("my own title: %v", err)
	}
	sessions = append(sessions, &claude.Session{PID: 9, ID: "b", Name: "supervisor run 82"})
	err := titleTaken(sessions, sharedTitle, me)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), `the title "Supervisor run 82" is taken: the live session b (PID 9)`) {
		t.Errorf("taken: %v", err)
	}
	if err := titleTaken(sessions, "Supervisor run 83", me); err != nil {
		t.Errorf("a free title: %v", err)
	}
}

// With two live sessions of one title, a message to the supervisor reaches
// the holder beekeeper records, by its CLI's own socket; with no socket the
// name stays ambiguous and is refused.
func TestTheRoleReachesItsHolderPastASharedTitle(t *testing.T) {
	holder := &state.Supervisor{Party: state.Party{Session: "s9", HostSession: holderHost, Name: sharedTitle}}
	sessions := []*claude.Session{{PID: 3, ID: "s3", Name: sharedTitle}, {PID: 9, ID: "s9", HostID: holderHost, Name: sharedTitle}}
	sock := func(pid int) string { return fmt.Sprintf("/run/user/1000/cc-socks/%d.sock", pid) }
	to, err := roleAddress(holder, supervisorRole, sessions, sock)
	if err != nil || to != "uds:/run/user/1000/cc-socks/9.sock" {
		t.Fatalf("to %q, %v; want the holder's socket", to, err)
	}
	if _, err := roleAddress(holder, supervisorRole, sessions, noSocket); err == nil {
		t.Error("no socket: an ambiguous name is refused")
	}
	if to, err := roleAddress(holder, supervisorRole, sessions[1:], sock); err != nil || to != sharedTitle {
		t.Errorf("a unique title stays the address: %q, %v", to, err)
	}
}

// The watch says a shared title once, naming both sessions, and its end
// once one session is left with it.
func TestTheWatchSaysASharedTitle(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	w.now = time.Now()
	both := []*claude.Session{{PID: 3, ID: "s3", Name: sharedTitle}, {PID: 9, ID: "s9", Name: sharedTitle}}
	w.sameTitles(both)
	w.sameTitles(both)
	if s := out.String(); strings.Count(s, "SAME TITLE") != 1 || !strings.Contains(s, "PID 3 session s3, PID 9 session s9") {
		t.Errorf("output:\n%s", s)
	}
	w.sameTitles(both[1:])
	if !strings.Contains(out.String(), "ENDED SAME TITLE") {
		t.Errorf("no end once one session is left:\n%s", out)
	}
}

// The doctor reports a title two live sessions carry.
func TestTheDoctorReportsASharedTitle(t *testing.T) {
	a, _ := stubApp(t)
	runLive(t, a, map[int]string{3: sharedTitle, 9: sharedTitle, 5: agentFour})
	rep, err := a.doctor(context.Background(), doctorRun{by: state.Party{Name: agentOne}, dryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.sameTitles) != 1 || rep.sameTitles[0].title != sharedTitle {
		t.Fatalf("findings %+v", rep.sameTitles)
	}
}

// A relay never starts a successor under a run name another live session
// carries: it is refused with the reason, and no relay opens.
func TestARelayRefusesATakenTitle(t *testing.T) {
	a, _ := stubApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) { *st = *handOverState(); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	a.cfg.Supervisor.RelayTTL.Duration = 15 * time.Minute
	runLive(t, a, map[int]string{4: supB.Name})
	_, _, err := a.startSuccessor(context.Background(), supervisorRole, supA, supA, t.TempDir())
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), `the title "Supervisor run 12" is taken: the live session s4 (PID 4)`) {
		t.Fatalf("relay: %v", err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if r := supervisorRole.get(st); r.Relay != nil || !r.Holder.Is(supA) {
		t.Errorf("the refused relay changed the role: %+v", r)
	}
}

// A start never titles its session with a run name another live session
// carries: it is refused with the reason, and the role stays where it was.
func TestAStartRefusesATakenTitle(t *testing.T) {
	a, _ := stubApp(t)
	a.as = ""
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) { st.Supervisor = nil; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s7")
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_NAME", "Agent eleven")
	runLive(t, a, map[int]string{7: "Agent eleven", 4: firstRun})
	err := a.runStart(context.Background(), supervisorRole, false)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), `the title "Supervisor run 1" is taken: the live session s4 (PID 4)`) {
		t.Fatalf("start: %v", err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Supervisor != nil {
		t.Errorf("the refused start took the role: %+v", st.Supervisor)
	}
}
