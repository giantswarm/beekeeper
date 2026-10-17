package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/takeover"
)

// rosterOnly is a roster name with spaces and parentheses that no running
// session carries as its desktop title.
const rosterOnly = "SCAN beekeeper 975 (opus, builds)"

// rosterNameApp is an app whose roster names the running session s4 (desktop
// title "Agent four") rosterOnly, and a registered agent s6 whose CLI does
// not run; it returns the running sessions, s4 with a transcript.
func rosterNameApp(t *testing.T) (*app, []*claude.Session) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{
			{Party: state.Party{Session: "s4", HostSession: hostFour, Name: rosterOnly}},
			{Party: state.Party{Session: "s6", Name: "PARKED beekeeper 976 (opus, builds)"}},
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(dir, "s4.jsonl")
	line := `{"type":"assistant","timestamp":"2026-10-17T09:00:00Z","message":{"content":[{"type":"text","text":"scanning 975"}]}}` + "\n"
	if err := os.WriteFile(transcript, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	sessions := []*claude.Session{
		{PID: 40, ID: "s4", HostID: hostFour, Name: agentFour, Transcript: transcript},
		{PID: 50, ID: "s5", Name: agentFive},
	}
	return &app{store: store, now: relayNow, cfg: &config.Config{StateDir: dir}}, sessions
}

// sessions tail and the screen's tail read the transcript of the session a
// roster name names; a registered agent whose CLI does not run is refused.
func TestSessionTailResolvesARosterName(t *testing.T) {
	a, sessions := rosterNameApp(t)
	for _, tools := range []bool{false, true} {
		turns, err := a.sessionTail(sessions, rosterOnly, 3, tools, relayNow)
		if err != nil || len(turns) != 1 || turns[0].Text != "scanning 975" {
			t.Errorf("tail of %q (tools %v) = %+v, %v", rosterOnly, tools, turns, err)
		}
	}
	if _, err := a.sessionTail(sessions, "PARKED beekeeper 976 (opus, builds)", 3, false, relayNow); err == nil || !strings.Contains(err.Error(), "no CLI of") {
		t.Errorf("tail of an agent whose CLI does not run: %v", err)
	}
}

// alerts own --by takes the running session a roster name names.
func TestAlertOwnerByResolvesARosterName(t *testing.T) {
	a, sessions := rosterNameApp(t)
	p, err := a.ownerNamed(sessions, rosterOnly)
	if err != nil || p.Session != "s4" {
		t.Errorf("--by %q = %+v, %v; want session s4", rosterOnly, p, err)
	}
	if _, err := a.ownerNamed(sessions, "nobody (here)"); err == nil {
		t.Error("--by a name nothing carries resolved")
	}
}

// The screen's Send reaches the session a roster name names by its desktop
// title, the name its CLI answers to.
func TestUISendResolvesARosterName(t *testing.T) {
	a, sessions := rosterNameApp(t)
	sent := fakeRelay(t, false)
	if _, err := a.sendNamed(context.Background(), sessions, rosterOnly, "hello", relayNow); err != nil {
		t.Fatalf("send to %q: %v", rosterOnly, err)
	}
	if got := fmt.Sprint(relayed(t, sent)); got != "["+agentFour+"]" {
		t.Errorf("relayed to %s, want [%s]", got, agentFour)
	}
}

// The screen's TakeOver flags the session a roster name names.
func TestUITakeOverResolvesARosterName(t *testing.T) {
	a, sessions := rosterNameApp(t)
	c := &collector{a: a}
	if err := c.takeOver(sessions, rosterOnly, relayNow); err != nil {
		t.Fatalf("take over %q: %v", rosterOnly, err)
	}
	if _, ok := takeover.Taken(c.takeoverDir(), "s4", func(int) bool { return true }); !ok {
		t.Errorf("take over of %q flagged no s4", rosterOnly)
	}
}
