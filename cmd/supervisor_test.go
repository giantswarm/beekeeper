package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
)

const browser = "browser"

// The role holders of the relay tests (the guide's role: the supervisor
// holds none), and two agents.
var (
	supA   = state.Party{Session: "sA", HostSession: "local_A", Name: "Guide run 11"}
	supB   = state.Party{Session: "sB", HostSession: "local_B", Name: "Guide run 12"}
	agentC = state.Party{Session: "sC", Name: "Agent three"}
	agentD = state.Party{Session: "sD", Name: "Agent five"}
)

// handOverState is a desk whose guide is supA, with two grants queued.
func handOverState() *state.State {
	return &state.State{
		Guide: &state.Role{Holder: &state.Supervisor{Party: supA, Since: relayNow.Add(-8 * time.Hour)}},
		Grants: []state.Grant{
			{Resource: browser, To: four, By: loopParty, At: relayNow.Add(-time.Minute)},
			{Resource: browser, To: agentD, By: loopParty, At: relayNow},
		},
	}
}

func TestRelayMovesTheRole(t *testing.T) {
	st := handOverState()
	var log []state.Event
	if _, _, err := guideRole.relay(st, supB, agentC, relayNow, 15*time.Minute); Code(err) != ExitRefused {
		t.Fatalf("a session that does not guide relayed: %v", err)
	}
	msg, evs, err := guideRole.relay(st, supA, supB, relayNow, 15*time.Minute)
	if err != nil || !strings.Contains(msg, `relayed to "Guide run 12"`) {
		t.Fatalf("relay: %q, %v", msg, err)
	}
	log = append(log, evs...)
	if _, _, err := guideRole.start(st, agentC, true, false, relayNow); Code(err) != ExitRefused || !strings.Contains(err.Error(), `its relay names "Guide run 12", not you`) {
		t.Fatalf("an unnamed start was not refused: %v", err)
	}
	if relievedIn(st.GuideRole(), supA) != nil {
		t.Fatal("relieved before the successor started")
	}
	msg, evs, err = guideRole.start(st, supB, true, false, relayNow.Add(time.Minute))
	if err != nil || !strings.Contains(msg, `relieving "Guide run 11"`) || strings.Contains(msg, "grant") {
		t.Fatalf("start: %q, %v", msg, err)
	}
	log = append(log, evs...)
	if !st.Guide.Holder.Is(supB) {
		t.Fatalf("guide is %q", st.Guide.Holder.Name)
	}
	for _, g := range st.Grants {
		if g.By != loopParty {
			t.Errorf("the guide's relay moved the grant of %s to %q", g.Resource, g.To.Name)
		}
	}
	if relievedIn(st.GuideRole(), supA) == nil || relievedIn(st.GuideRole(), supB) != nil {
		t.Fatal("status does not tell the outgoing guide it has been relieved")
	}
	if len(log) != 2 || log[0].Verb != "guide.relay" || log[1].Verb != "guide.start" {
		t.Fatalf("the event log lacks the two steps: %+v", log)
	}
	lines, _ := guideRole.fireRelay(st, relayNow.Add(time.Minute))
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "GUIDE RELAY TAKEN:") {
		t.Fatalf("taken: %q", lines)
	}
	if lines, _ := guideRole.fireRelay(st, relayNow.Add(2*time.Minute)); len(lines) != 0 {
		t.Fatalf("taken reported twice: %q", lines)
	}
}

func TestRelayExpiresAndCancels(t *testing.T) {
	st := handOverState()
	if _, _, err := guideRole.relay(st, supA, supB, relayNow, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	later := relayNow.Add(16 * time.Minute)
	if lines, _ := guideRole.fireRelay(st, relayNow.Add(time.Minute)); len(lines) != 0 {
		t.Fatalf("open relay reported: %q", lines)
	}
	if _, _, err := guideRole.start(st, supB, true, false, later); Code(err) != ExitRefused || !strings.Contains(err.Error(), "its relay to you expired") {
		t.Fatalf("an expired relay was taken: %v", err)
	}
	if !st.Guide.Holder.Is(supA) {
		t.Fatal("the outgoing guide lost the role to an expired relay")
	}
	lines, evs := guideRole.fireRelay(st, later)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "GUIDE RELAY EXPIRED:") || len(evs) != 1 {
		t.Fatalf("expired: %q", lines)
	}
	if lines, _ := guideRole.fireRelay(st, later); len(lines) != 0 {
		t.Fatalf("expiry reported twice: %q", lines)
	}
	if _, _, err := guideRole.relay(st, supA, supB, later, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	msg, evs, err := guideRole.cancelRelay(st, supA, later)
	if err != nil || st.Guide.Relay != nil || len(evs) != 1 || !strings.Contains(msg, "cancelled") {
		t.Fatalf("cancel: %q, %v", msg, err)
	}
	if _, _, err := guideRole.start(st, supB, true, false, later); Code(err) != ExitRefused {
		t.Fatalf("a cancelled relay was taken: %v", err)
	}
	if _, _, err := guideRole.start(st, supB, false, false, later); err != nil || !st.Guide.Holder.Is(supB) {
		t.Fatalf("a start over a gone guide: %v", err)
	}
}

// supervisorTranscript is a real session transcript's window, its content
// stripped to the usage lines: its last request read 163018 tokens.
var supervisorTranscript = filepath.Join("..", "internal", "claude", "testdata", "transcript", "window.jsonl")

func TestRelievedSurvivesTheSuccessorsRelays(t *testing.T) {
	st := handOverState()
	relieved := func(p state.Party) *state.Relief { return relievedIn(st.GuideRole(), p) }
	step := func(at time.Duration, what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s at +%s: %v", what, at, err)
		}
		if r := relieved(supA); r == nil || !r.By.Is(supB) {
			t.Fatalf("after %s, A's status does not say B relieved it: %+v", what, r)
		}
	}
	relay := func(from, to state.Party, at time.Duration) error {
		_, _, err := guideRole.relay(st, from, to, relayNow.Add(at), 15*time.Minute)
		return err
	}
	start := func(who state.Party, at time.Duration) error {
		_, _, err := guideRole.start(st, who, true, false, relayNow.Add(at))
		return err
	}
	if err := relay(supA, supB, 0); err != nil || relieved(supA) != nil {
		t.Fatalf("relieved by an open relay: %v", err)
	}
	step(time.Minute, "B's start", start(supB, time.Minute))
	step(2*time.Minute, "B's relay to C", relay(supB, agentC, 2*time.Minute))
	_, _, err := guideRole.cancelRelay(st, supB, relayNow.Add(3*time.Minute))
	step(3*time.Minute, "B's cancel", err)
	step(4*time.Minute, "B's relay to C again", relay(supB, agentC, 4*time.Minute))
	step(5*time.Minute, "C's start", start(agentC, 5*time.Minute))
	if r := relieved(supB); r == nil || !r.By.Is(agentC) {
		t.Fatalf("B is not relieved by C: %+v", r)
	}
	if relieved(agentC) != nil {
		t.Fatal("the guide reads as relieved")
	}
	// A takes the role back through C's relay: guiding again ends its relief.
	if err := relay(agentC, supA, 6*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := start(supA, 7*time.Minute); err != nil || relieved(supA) != nil {
		t.Fatalf("A guides again and still reads as relieved: %v", err)
	}
	if len(st.Guide.Relieved) != 2 {
		t.Fatalf("one relief per party: %+v", st.Guide.Relieved)
	}
	// A relief nobody asked about for reliefTTL goes with the next start.
	if err := relay(supA, supB, reliefTTL+time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := start(supB, reliefTTL+time.Hour+time.Minute); err != nil {
		t.Fatal(err)
	}
	if len(st.Guide.Relieved) != 1 || !st.Guide.Relieved[0].Party.Is(supA) {
		t.Fatalf("stale reliefs stay: %+v", st.Guide.Relieved)
	}
}

// A holder's CLI back under the same session within the grace is a
// restart, logged once; past the grace it is gone, and a new term has a
// grace of its own.
func TestRestartGraceThroughACLIRestart(t *testing.T) {
	const grace = time.Minute
	st := handOverState()
	running := func(pid int) []*claude.Session {
		return []*claude.Session{{PID: pid, ID: supA.Session, HostID: supA.HostSession, Name: supA.Name}}
	}
	read := func(at time.Duration, sessions []*claude.Session) supervision {
		return readHolder(st.GuideRole(), sessions, relayNow.Add(at), grace)
	}
	if sv := read(0, nil); !sv.restarting() {
		t.Fatal("a CLI never seen in this term has no grace")
	}
	if changed, evs := guideRole.observeCLI(st, running(100), relayNow); !changed || len(evs) != 0 || st.Guide.CLI.PID != 100 {
		t.Fatalf("first sighting: %v %+v %+v", changed, evs, st.Guide.CLI)
	}
	if changed, _ := guideRole.observeCLI(st, running(100), relayNow.Add(time.Second)); changed {
		t.Fatal("an unchanged CLI changes the state")
	}
	if changed, _ := guideRole.observeCLI(st, nil, relayNow.Add(10*time.Second)); !changed || st.Guide.CLI.Gone.IsZero() {
		t.Fatal("the CLI's absence is not recorded")
	}
	if sv := read(69*time.Second, nil); !sv.restarting() {
		t.Fatal("gone within the grace")
	}
	changed, evs := guideRole.observeCLI(st, running(200), relayNow.Add(16*time.Second))
	if !changed || len(evs) != 1 || evs[0].Verb != "guide.restart" || !strings.Contains(evs[0].Detail, "CLI 100 back as 200 (gone for 6s)") {
		t.Fatalf("restart: %v %+v", changed, evs)
	}
	guideRole.observeCLI(st, nil, relayNow.Add(time.Hour))
	if sv := read(time.Hour+grace-time.Second, nil); !sv.restarting() {
		t.Fatalf("within the grace after the crash: %+v", sv)
	}
	if sv := read(time.Hour+grace, nil); !sv.down() || !sv.gone.Equal(relayNow.Add(time.Hour)) {
		t.Fatalf("down: %+v", sv)
	}
	if _, _, err := guideRole.start(st, supB, false, false, relayNow.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if sv := read(2*time.Hour, nil); !sv.restarting() || !sv.gone.Equal(relayNow.Add(2*time.Hour)) {
		t.Fatalf("the previous term's CLI record counts for the new guide: %+v", sv)
	}
}

func TestRoleViewShowsTheContext(t *testing.T) {
	a := &app{cfg: &config.Config{Guide: config.Guide{Role: config.Role{RelayAt: 400_000}}}, now: relayNow}
	r := state.Role{Holder: &state.Supervisor{Party: supA, Since: relayNow.Add(-time.Hour)}}
	sessions := []*claude.Session{{ID: supA.Session, HostID: supA.HostSession, Name: supA.Name, Transcript: supervisorTranscript}}
	v := a.viewRole(guideRole, r, sessions, supervision{live: true})
	if got := v.contextText(); got != ", 163k tokens of context (relay at 400k)" {
		t.Fatalf("context: %q", got)
	}
	if got := a.viewRole(guideRole, r, nil, supervision{}).contextText(); got != "" {
		t.Fatalf("context of a gone session: %q", got)
	}
	if a.viewRole(guideRole, state.Role{}, sessions, supervision{}) != nil {
		t.Fatal("a view of no holder")
	}
}

func TestARenamedHolderIsKnownByItsCurrentTitle(t *testing.T) {
	const renamedTitle = "Guide run 18"
	st := handOverState()
	renamed := []*claude.Session{{PID: 100, ID: supA.Session, HostID: supA.HostSession, Name: renamedTitle}}
	guideRole.observeCLI(st, []*claude.Session{{PID: 100, ID: supA.Session, HostID: supA.HostSession, Name: supA.Name}}, relayNow)
	changed, evs := guideRole.observeCLI(st, renamed, relayNow.Add(time.Second))
	if !changed || st.Guide.Holder.Name != renamedTitle || st.Guide.CLI.Supervisor.Name != renamedTitle {
		t.Fatalf("the rename is not recorded: %v %+v %+v", changed, st.Guide.Holder, st.Guide.CLI)
	}
	if len(evs) != 1 || evs[0].Verb != "guide.rename" || !strings.Contains(evs[0].Detail, `is now "`+renamedTitle+`"`) {
		t.Fatalf("the rename is not logged once: %+v", evs)
	}
	if changed, evs := guideRole.observeCLI(st, renamed, relayNow.Add(2*time.Second)); changed || len(evs) != 0 {
		t.Fatalf("an unchanged title changes the state: %v %+v", changed, evs)
	}
	if renameHolder(st.Guide.Holder, []*claude.Session{{PID: 100, ID: supA.Session}}) || st.Guide.Holder.Name != renamedTitle {
		t.Fatalf("an untitled session erased the name: %+v", st.Guide.Holder)
	}
}

// supervisorApp is an app on a fresh state whose supervisor command runs
// with out as its output.
func supervisorApp(t *testing.T) (*app, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	a := quietApp(t, &out)
	return a, &out
}

// The supervisor is the supervisor loop: status reads the loop's PID marker,
// exit 3 while no loop runs, and the command has no session subcommands.
func TestSupervisorStatusReadsTheLoop(t *testing.T) {
	a, out := supervisorApp(t)
	if err := a.supervisorStatus(); Code(err) != ExitRefused || !strings.Contains(err.Error(), "the supervisor loop does not run") {
		t.Fatalf("no loop: %v", err)
	}
	if err := a.store.WriteFile(loopPIDFile, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := a.supervisorStatus(); err != nil || !strings.Contains(out.String(), "the supervisor loop runs (PID ") {
		t.Fatalf("a running loop: %v, %q", err, out)
	}
	if err := a.store.WriteFile(loopPIDFile, 0); err != nil {
		t.Fatal(err)
	}
	if err := a.supervisorStatus(); Code(err) != ExitRefused {
		t.Fatalf("a stopped loop: %v", err)
	}
	for _, c := range a.supervisorCmd().Commands() {
		switch c.Name() {
		case "status", "loop":
		default:
			t.Errorf("the supervisor command has %q: no session holds the role", c.Name())
		}
	}
}

// fakeLoopService records the standby's restarts of the loop's service.
type fakeLoopService struct {
	state   string
	starts  int
	refusal error
}

func (f *fakeLoopService) service() *loopService {
	return &loopService{
		state: func(context.Context) string { return f.state },
		start: func(context.Context) error {
			f.starts++
			return f.refusal
		},
	}
}

// While the supervisor loop is down the standby watch restarts its service,
// at most once a minute, says SUPERVISOR LOOP DOWN once past the grace and
// notifies, and never starts or relays a session: the supervisor session is
// gone. A loop back is one line; a stop restarts nothing.
func TestStandbyRestartsTheLoopAndStartsNoSession(t *testing.T) {
	dir := t.TempDir()
	w, d, out := notifyingWatch(t, dir, true)
	svc := &fakeLoopService{state: "dead"}
	w.stand.loopUnit = svc.service()
	successors := 0
	w.stand.succeed = func(context.Context, role, state.Party) (state.Party, error) {
		successors++
		return state.Party{}, errors.New("no successor")
	}
	poll := func(at time.Duration) {
		w.now = relayNow.Add(at)
		w.pending(context.Background(), nil)
	}
	downs := func() (n int) {
		for _, m := range d.sent {
			if m.Summary == "beekeeper: the supervisor loop is down" {
				n++
			}
		}
		return n
	}
	poll(0)
	if svc.starts != 1 || !strings.Contains(out.String(), "SUPERVISOR LOOP RESTARTED") {
		t.Fatalf("the first poll did not restart the loop: %d starts\n%s", svc.starts, out)
	}
	poll(30 * time.Second)
	if svc.starts != 1 {
		t.Fatalf("restarted again within a minute: %d", svc.starts)
	}
	if strings.Contains(out.String(), "SUPERVISOR LOOP DOWN") || downs() != 0 {
		t.Fatalf("down within the grace:\n%s", out)
	}
	svc.state = "activating"
	poll(90 * time.Second)
	if svc.starts != 1 {
		t.Fatalf("restarted a starting service: %d", svc.starts)
	}
	svc.state = "dead"
	poll(2*time.Minute + time.Second)
	poll(3 * time.Minute)
	if svc.starts != 2 || strings.Count(out.String(), "SUPERVISOR LOOP DOWN") != 1 || downs() != 1 {
		t.Fatalf("down: %d starts, %d notifications\n%s", svc.starts, downs(), out)
	}
	if successors != 0 {
		t.Fatalf("the standby started %d supervisor sessions", successors)
	}
	// The loop runs again: one BACK line.
	if err := w.store.WriteFile(loopPIDFile, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	poll(4 * time.Minute)
	poll(5 * time.Minute)
	if strings.Count(out.String(), "SUPERVISOR LOOP BACK") != 1 {
		t.Fatalf("back:\n%s", out)
	}
	// A stop holds the loop: nothing restarts it.
	if err := w.store.WriteFile(loopPIDFile, 0); err != nil {
		t.Fatal(err)
	}
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Stop = &state.Stop{Reason: "maintenance", At: relayNow}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	starts := svc.starts
	poll(10 * time.Minute)
	poll(20 * time.Minute)
	if svc.starts != starts || successors != 0 {
		t.Fatalf("a stopped desk restarted the loop (%d) or started a session (%d)", svc.starts-starts, successors)
	}
	if downs() != 1 {
		t.Fatalf("a stopped desk notified the loop down: %d", downs())
	}
}

// A message to "the supervisor" is refused with where a worker reports,
// waits and asks instead: no session holds the role.
func TestAMessageToTheSupervisorIsRefused(t *testing.T) {
	a, _ := supervisorApp(t)
	if _, err := a.roleTarget(guard.RoleSupervisor); !errors.Is(err, errSupervisorMessage) {
		t.Fatalf("a message to the supervisor: %v", err)
	}
}
