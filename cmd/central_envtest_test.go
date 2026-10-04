package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/central/centraltest"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	verbHold  = "hold"
	piaEmail  = "pia@example.com"
	agentTwo  = "Agent two"
	stateIdle = "idle"
)

// person is one person's machine: its configuration with the central
// instance behind the test muster, and its state.
type person struct {
	t     *testing.T
	cfg   string
	state string
	agent string
}

func newPerson(t *testing.T, m *centraltest.Muster, token, email, team, host, agent string) *person {
	t.Helper()
	bin, _ := centraltest.Binary(t, m.URL, token)
	dir := t.TempDir()
	p := &person{t: t, cfg: filepath.Join(dir, "config.yaml"), state: filepath.Join(dir, "state"), agent: agent}
	cfg := fmt.Sprintf(`stateDir: %s
leaseDir: %s
resources: [kind-1]
identity: {person: %s, team: %s, host: %s}
central: {context: lab, muster: %s}
lanes: [{name: %s, installation: gazelle, repositories: [%s]}]
`, p.state, filepath.Join(dir, "leases"), email, team, host, bin, portalLane, backstage)
	if err := os.WriteFile(p.cfg, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// run runs beekeeper as the person's agent and returns its output and
// exit code.
func (p *person) run(args ...string) (string, int) {
	p.t.Helper()
	var out bytes.Buffer
	a := &app{cfgPath: p.cfg, as: p.agent, out: &out}
	if err := a.load(); err != nil {
		p.t.Fatal(err)
	}
	cmd := map[string]func() *cobra.Command{"lease": a.leaseCmd, verbHold: a.holdCmd, lanesName: a.lanesCmd}[args[0]]()
	cmd.SetArgs(args[1:])
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	if err := cmd.Execute(); err != nil {
		out.WriteString(err.Error())
		return out.String(), Code(err)
	}
	return out.String(), 0
}

// watcher is the person's watch, its lines into out.
func (p *person) watcher(out *bytes.Buffer) *watcher {
	p.t.Helper()
	a := &app{cfgPath: p.cfg, out: out}
	if err := a.load(); err != nil {
		p.t.Fatal(err)
	}
	return &watcher{app: a, last: map[string]time.Time{}}
}

// Two people's machines share an installation through the central instance:
// a lease one claims is refused to the other with its holder, the machine's
// own resources stay local, each machine's agents are on the central roster,
// and an unreachable central instance refuses the central verbs with their
// own exit code and is one watch line.
func TestCentralEnvtest(t *testing.T) {
	e := newServeEnv(t)
	m := centraltest.New(t, e.url, "beekeeper")
	anaToken := e.token(t, anaEmail, teamGroup)
	ana := newPerson(t, m, anaToken, anaEmail, ourTeam, "ana-laptop", anaAgent)
	pia := newPerson(t, m, e.token(t, piaEmail, "giantswarm:team-planeteers"), piaEmail, piaTeam, "pia-laptop", "pia-agent")

	if out, code := ana.run("lease", "claim", graveler, "-p", "e2e for #123"); code != 0 || !strings.Contains(out, "claimed "+graveler) {
		t.Fatalf("ana's claim: exit %d: %s", code, out)
	}
	out, code := pia.run("lease", "claim", graveler, "-p", "mine")
	if code != ExitRefused || !strings.Contains(out, fmt.Sprintf("%s is held by %q", graveler, anaEmail+"/"+anaAgent)) || !strings.Contains(out, "e2e for #123") {
		t.Errorf("pia's claim: exit %d, want %d naming ana's lease: %s", code, ExitRefused, out)
	}
	if out, code := pia.run("lease", "status", graveler); code != ExitRefused || !strings.Contains(out, anaAgent) {
		t.Errorf("pia's status: exit %d: %s", code, out)
	}
	if out, code := pia.run("lease", "list"); code != 0 || !strings.Contains(out, "central (muster context lab)") ||
		!strings.Contains(out, graveler) || !strings.Contains(out, anaAgent) || !strings.Contains(out, "free: "+glean) {
		t.Errorf("pia's list: exit %d: %s", code, out)
	}
	if out, code := pia.run("lease", "release", graveler); code != ExitRefused || !strings.Contains(out, "not by you") {
		t.Errorf("pia's release of ana's lease: exit %d: %s", code, out)
	}
	if out, code := ana.run("lease", "release", graveler); code != 0 || !strings.Contains(out, "released "+graveler) {
		t.Errorf("ana's release: exit %d: %s", code, out)
	}
	if out, code := pia.run("lease", "claim", graveler, "-p", "mine"); code != 0 {
		t.Errorf("pia's claim after the release: exit %d: %s", code, out)
	}
	if out, code := pia.run("lease", "claim", "nowhere", "-p", "x"); code != ExitUsage {
		t.Errorf("an unknown name: exit %d, want %d: %s", code, ExitUsage, out)
	}

	// The machine's agents are on the central roster, on their transitions.
	anaStore, err := state.Open(ana.state)
	if err != nil {
		t.Fatal(err)
	}
	setAgents := func(agents ...state.Agent) {
		t.Helper()
		if err := anaStore.Update(func(st *state.State) ([]state.Event, error) { st.Agents = agents; return nil, nil }); err != nil {
			t.Fatal(err)
		}
	}
	reader := e.as(t, anaToken)
	roster := func() map[string]map[string]any {
		t.Helper()
		_, got, _ := reader.call("list_agents", map[string]any{"scope": scopeTeam})
		out := map[string]map[string]any{}
		agents, _ := got[agentsName].([]any)
		for _, a := range agents {
			a := a.(map[string]any)
			out[a["address"].(string)] = a
		}
		return out
	}
	var lines bytes.Buffer
	w := ana.watcher(&lines)
	setAgents(state.Agent{Party: state.Party{Name: agentOne}, Task: "giantswarm/beekeeper#157", Registered: time.Now()},
		state.Agent{Party: state.Party{Name: agentTwo}, Registered: time.Now()})
	w.syncRoster(context.Background())
	got := roster()
	if a := got["local:ana-laptop/"+agentOne]; a == nil || a["state"] != "busy" || a["task"] != "giantswarm/beekeeper#157" || a["person"] != anaEmail {
		t.Errorf("Agent one on the roster: %v", got)
	}
	if a := got["local:ana-laptop/"+agentTwo]; a == nil || a["state"] != stateIdle {
		t.Errorf("Agent two on the roster: %v", got)
	}
	before := e.events(t)
	w.syncRoster(context.Background())
	if n := e.events(t) - before; n != 0 {
		t.Errorf("a sync without a transition wrote %d events", n)
	}
	setAgents(state.Agent{Party: state.Party{Name: agentOne}, Done: true, Registered: time.Now()})
	w.syncRoster(context.Background())
	got = roster()
	if a := got["local:ana-laptop/"+agentOne]; a == nil || a["state"] != "ended" {
		t.Errorf("Agent one done: %v", got)
	}
	if _, ok := got["local:ana-laptop/"+agentTwo]; ok {
		t.Errorf("Agent two is gone from the machine, still on the roster: %v", got)
	}

	// Unreachable: central verbs refuse with ExitCentral, local ones work,
	// and the watch says so once and its end once.
	m.Down(true)
	if out, code := pia.run("lease", "release", graveler); code != ExitCentral || !strings.Contains(out, "the central instance (muster context lab) is unreachable") {
		t.Errorf("release while unreachable: exit %d, want %d: %s", code, ExitCentral, out)
	}
	if out, code := pia.run("lease", "claim", "kind-1", "-p", "local"); code != 0 {
		t.Errorf("a local claim while unreachable: exit %d: %s", code, out)
	}
	if out, code := pia.run("lease", "list"); code != ExitCentral || !strings.Contains(out, "kind-1") {
		t.Errorf("list while unreachable: exit %d, want the machine's leases and %d: %s", code, ExitCentral, out)
	}
	setAgents(state.Agent{Party: state.Party{Name: "Agent three"}, Registered: time.Now()})
	w.syncRoster(context.Background())
	w.syncRoster(context.Background())
	if n := strings.Count(lines.String(), "CENTRAL UNREACHABLE lab: "); n != 1 {
		t.Errorf("%d CENTRAL UNREACHABLE lines, want 1:\n%s", n, lines.String())
	}
	m.Down(false)
	w.syncRoster(context.Background())
	if !strings.Contains(lines.String(), "ENDED CENTRAL UNREACHABLE lab") {
		t.Errorf("no ENDED line once the central instance answers:\n%s", lines.String())
	}
	if a := roster()["local:ana-laptop/Agent three"]; a == nil {
		t.Errorf("Agent three, registered while unreachable, is not published once it answers")
	}
}

// gate is the person's gate on backstage#pr, its central calls only.
func (p *person) gate(pr int) *gateRun {
	p.t.Helper()
	a := &app{cfgPath: p.cfg, as: p.agent, out: &bytes.Buffer{}}
	if err := a.load(); err != nil {
		p.t.Fatal(err)
	}
	me, err := a.caller()
	if err != nil {
		p.t.Fatal(err)
	}
	g := &gateRun{app: a, ctx: context.Background(), repo: backstage, pr: pr, lane: a.cfg.LaneOf(backstage), me: me}
	g.central = a.cfg.CentralLane(g.lane)
	return g
}

// Two people's merges into one central lane roll one after the other: the
// second's turn comes only once the first merged and its release rolled; a
// central hold refuses the gate; a place nobody asks for any longer holds
// up nobody; an unreachable central instance refuses with ExitCentral.
func TestCentralLanesEnvtest(t *testing.T) {
	e := newServeEnv(t)
	m := centraltest.New(t, e.url, "beekeeper")
	ana := newPerson(t, m, e.token(t, anaEmail, teamGroup), anaEmail, ourTeam, "ana-laptop", anaAgent)
	pia := newPerson(t, m, e.token(t, piaEmail, "giantswarm:team-planeteers"), piaEmail, piaTeam, "pia-laptop", "pia-agent")

	first, second := ana.gate(1), pia.gate(2)
	if !first.central {
		t.Fatal("the lane on gazelle, a central installation, is not central")
	}
	turn := func(g *gateRun) string {
		t.Helper()
		g.centralAsked = time.Time{}
		why, err := g.centralTurn()
		if err != nil {
			t.Fatalf("%s: %v", g.key(), err)
		}
		return why
	}
	if why := turn(first); why != "" {
		t.Errorf("the first merge waits: %s", why)
	}
	if why := turn(second); !strings.Contains(why, "position 2 in central lane "+portalLane+" behind "+backstage+"#1") {
		t.Errorf("the second merge: %q", why)
	}
	if why, err := first.centralStart(); why != "" || err != nil {
		t.Fatalf("the first start: %q, %v", why, err)
	}
	if why := turn(second); !strings.Contains(why, "behind "+backstage+"#1") {
		t.Errorf("the second behind a running merge: %q", why)
	}
	if why, err := second.centralStart(); why == "" || err != nil {
		t.Errorf("the second started beside a running merge: %q, %v", why, err)
	}
	first.centralRecord(true, "")
	if why := turn(second); why == "" {
		t.Error("the second's turn came while the first settles")
	}
	ana.gate(1).leaveCentral(context.Background(), []state.Merge{{Repo: backstage, PR: 1, Lane: portalLane, By: first.me}}, "rolled")
	if why := turn(second); why != "" {
		t.Errorf("the second waits after the first rolled: %s", why)
	}
	second.centralLeave("test done")

	// A central hold refuses the gate and is listed and checked as central.
	if out, code := ana.run(verbHold, "set", "--lane", portalLane, "-r", "proving window"); code != 0 || !strings.Contains(out, "held lane:"+portalLane) {
		t.Fatalf("hold set: exit %d: %s", code, out)
	}
	if out, code := pia.run(verbHold, "check", backstage+"#2"); code != ExitRefused || !strings.Contains(out, "proving window") {
		t.Errorf("hold check on the other machine: exit %d: %s", code, out)
	}
	if out, code := pia.run(verbHold, "list"); code != 0 || !strings.Contains(out, "central (muster context lab)") || !strings.Contains(out, "proving window") {
		t.Errorf("hold list: exit %d: %s", code, out)
	}
	if _, err := pia.gate(2).centralTurn(); Code(err) != ExitGateRefused {
		t.Errorf("a held lane's gate: %v (exit %d), want %d", err, Code(err), ExitGateRefused)
	}
	if out, code := ana.run(verbHold, "lift", "--lane", portalLane); code != 0 {
		t.Fatalf("hold lift: exit %d: %s", code, out)
	}

	// A waiting place unseen for merge.queueTTL holds up nobody.
	if why := turn(first); why != "" {
		t.Fatalf("first again: %s", why)
	}
	e.srv.cfg.Merge.QueueTTL.Duration = time.Nanosecond
	if why := turn(second); why != "" {
		t.Errorf("a stale place holds up the second: %s", why)
	}

	m.Down(true)
	if _, err := pia.gate(3).centralTurn(); Code(err) != ExitCentral {
		t.Errorf("unreachable: %v (exit %d), want %d", err, Code(err), ExitCentral)
	}
}
