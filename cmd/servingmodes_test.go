package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/state"
)

// servingModesRawCfg is a desk with one self-hosted provider served in two
// modes: tiny on one node, full across two. fullModeNode is the node an
// exclusive constraint can claim; the tiny mode's ceiling stands while the
// constraint holds, so the desk keeps starting on the model during a drain.
const servingModesRawCfg = `resources: [node-a, node-b]
capacity:
  floor: 0
  ceiling: 0
  providers:
    claude:
      floor: 1
      ceiling: 2
    local-model:
      kind: self-hosted
      ceiling: 3
      serveModeCommand: [serve-mode]
      serveModeSwitchCommand: [serve-mode]
      serveModeModes:
        full: {nodes: [node-a, node-b], sessions: 3}
        tiny: {nodes: [node-a], sessions: 1}
        single: {nodes: [], sessions: 2}
  choices:
    - harness: claude
      model: claude-opus-4-7
    - harness: omp
      model: local-model/worker-model
      provider: local-model
      skipPace: true
omp:
  model: local-model/worker-model
`

// The serving modes and the node the serving-modes tests constrain.
const (
	servingModeFull = "full"
	servingModeTiny = "tiny"
	fullModeNode    = "node-b"
)

// fakeModeScript is the fake platform tool: called with no argument it
// prints the mode file, called with a mode it switches into that mode.
// Nothing here touches a real serving-mode command or the model this desk
// serves.
const fakeModeScript = `#!/bin/sh
if [ -z "$1" ]; then cat "$MODE_DIR/mode"; exit 0; fi
printf '%s\n' "$1" > "$MODE_DIR/mode"
echo "switched to $1"
`

// servingModesTest is one scenario: a desk with the config above, a fake
// mode reader and switcher sharing one mode file, and the event log of
// every command run against it.
type servingModesTest struct {
	t        *testing.T
	dir      string
	cfgPath  string
	cfg      *config.Config
	store    *state.FileStore
	modeFile string
	now      time.Time
	// The provider's fake platform tool: the environment its scenario
	// gives it.
	fakeEnv []string
}

func newServingModesTest(t *testing.T) *servingModesTest {
	t.Helper()
	dir, cfgPath := isolateUsageTestEnv(t)
	if err := os.WriteFile(cfgPath, []byte(servingModesRawCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	modeFile := filepath.Join(dir, "mode")
	if err := os.WriteFile(modeFile, []byte(servingModeTiny+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The fake platform tools live in the scratch dir, beside the mode file:
	// the config's own names are rewritten to them, so no command name ever
	// resolves on the machine.
	fake := filepath.Join(dir, "serve-mode")
	if err := os.WriteFile(fake, []byte(fakeModeScript), 0o700); err != nil { //nolint:gosec // the fake platform tool, an executable script
		t.Fatal(err)
	}
	p := cfg.Capacity.Providers[selfHostedProv]
	p.ServeModeCommand = []string{fake}
	p.ServeModeSwitch = []string{fake}
	cfg.Capacity.Providers[selfHostedProv] = p
	// The config file itself carries the fakes: every liveCapacity read of
	// every command instance sees them, as it would a real tool on PATH.
	if _, err := config.Set(cfgPath, []config.Setting{
		{Key: "capacity.providers.local-model.serveModeCommand", Value: `["` + fake + `"]`},
		{Key: "capacity.providers.local-model.serveModeSwitchCommand", Value: `["` + fake + `"]`},
	}); err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	s := &servingModesTest{t: t, dir: dir, cfgPath: cfgPath, cfg: cfg, store: store,
		modeFile: modeFile, now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	s.fakeEnv = []string{
		"MODE_DIR=" + dir,
		"MODE_FAIL_FULL=" + filepath.Join(dir, ".allow"),
		"MODE_FAIL_TINY=" + filepath.Join(dir, ".tries"),
	}
	return s
}

// app is a fresh app over the same state at the test's clock, as a fresh
// command is on the machine. The serving mode is read through the fake
// reader (the mode file in the scratch dir), not by running a command.
func (s *servingModesTest) app(out *bytes.Buffer) *app {
	a := &app{cfgPath: s.cfgPath, cfg: s.cfg, store: s.store, now: s.now, out: out, as: usageCaller}
	// The scenario's fakes live in the in-memory config: every liveCapacity
	// read falls back to it. The file named by configLive loads only when it
	// exists, so a scenario that rewrites the machine's config keeps being
	// seen by every command instance, as the machine's own file would.
	a.configLive = filepath.Join(s.dir, "no-such-config.yaml")
	// The fake platform tools live in the scratch dir; the env names the
	// mode file and the scenario's fail files.
	a.serveModeEnv = s.fakeEnv
	a.switchServeModeRun = func(ctx context.Context, command []string, mode string) (string, error) {
		return runServeModeSwitchCommand(ctx, config.Provider{ServeModeSwitch: command}, mode, s.dir, s.fakeEnv)
	}
	return a
}

// fresh drops the cached serving-mode reading so the next command reads the
// mode from the fake again.
func (s *servingModesTest) fresh() {
	s.t.Helper()
	if err := os.Remove(filepath.Join(s.cfg.StateDir, serveModeFile(selfHostedProv))); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.t.Fatal(err)
	}
}

// fakeSwitch installs a fake switcher of the scenario's own over the
// config's switch command; its mode file lies beside it in the scratch dir.
// fakeSwitch installs a fake switcher of the scenario's own over the
// config's switch command; its mode file lies beside it in the scratch dir.
// The reader is the default fake script: the switcher body needs a read
// branch, so its mode is printed from the mode file.
func (s *servingModesTest) fakeSwitch(body string) {
	s.t.Helper()
	// The fake switcher is the reader too: called with no argument it
	// prints the mode file, called with the wanted mode it switches.
	script := `#!/bin/sh
if [ -z "$1" ]; then cat "$MODE_DIR/mode"; exit 0; fi
` + body + `
`
	fake := s.cfg.Capacity.Providers[selfHostedProv].ServeModeSwitch[0]
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil { //nolint:gosec // the fake switcher, an executable script
		s.t.Fatal(err)
	}
}

func (s *servingModesTest) mode() string {
	s.t.Helper()
	raw, err := os.ReadFile(s.modeFile)
	if err != nil {
		s.t.Fatal(err)
	}
	return strings.TrimSpace(string(raw))
}

// setMode is a hand switch: the mode file changes under beekeeper, and the
// next read sees it.
func (s *servingModesTest) setMode(mode string) {
	s.t.Helper()
	if err := os.WriteFile(s.modeFile, []byte(mode+"\n"), 0o600); err != nil {
		s.t.Fatal(err)
	}
	s.fresh()
}

// lease runs a lease subcommand as its own app instance, its --as flag
// bound to that instance, so the claim and release carry the caller.
func (s *servingModesTest) lease(name string, args ...string) (string, error) {
	s.t.Helper()
	s.fresh()
	out := &bytes.Buffer{}
	a := s.app(out)
	a.as = name
	c := a.leaseCmd()
	c.PersistentPreRunE = nil
	usageArgs(c)
	c.SetArgs(args)
	c.SetOut(out)
	c.SetErr(out)
	c.SilenceUsage, c.SilenceErrors = true, true
	err := c.ExecuteContext(context.Background())
	if err != nil {
		return out.String() + "\nerr: " + err.Error(), err
	}
	return out.String(), nil
}

// claimNode requests the exclusive constraint of the full mode's node.
func (s *servingModesTest) claimNode(name string) (string, error) {
	return s.lease(name, "claim", fullModeNode, "--purpose", "a gpu job")
}

func (s *servingModesTest) releaseNode(name string) (string, error) {
	return s.lease(name, "release", fullModeNode)
}

// agents sets the roster's busy agents on the self-hosted provider.
func (s *servingModesTest) agents(names ...string) {
	s.t.Helper()
	if err := s.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = nil
		for _, n := range names {
			w := tasked(n)
			w.Harness, w.Model = omp.Harness, selfHostedTestModel
			st.Agents = append(st.Agents, w)
		}
		return nil, nil
	}); err != nil {
		s.t.Fatal(err)
	}
}

// capacity runs beekeeper capacity and returns its output.
func (s *servingModesTest) capacity() string {
	s.t.Helper()
	s.fresh()
	var out bytes.Buffer
	if err := s.app(&out).capacityCmd().Execute(); err != nil {
		s.t.Fatalf("capacity: %v\n%s", err, out.String())
	}
	return out.String()
}

// statusLine runs beekeeper status and returns its line.
func (s *servingModesTest) statusLine() string {
	s.t.Helper()
	s.fresh()
	var out bytes.Buffer
	if err := s.app(&out).statusCmd().Execute(); err != nil {
		s.t.Fatalf("status: %v\n%s", err, out.String())
	}
	return out.String()
}

func (s *servingModesTest) events(verbs ...string) []state.Event {
	s.t.Helper()
	evs, err := s.store.Events(0, func(e state.Event) bool {
		for _, v := range verbs {
			if e.Verb == v {
				return true
			}
		}
		return false
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return evs
}

func eventLines(evs []state.Event) string {
	var b strings.Builder
	for _, e := range evs {
		fmt.Fprintf(&b, "%s %s\n", e.Verb, e.Detail)
	}
	return b.String()
}

// TestServingModesConfig: each mode names the nodes it occupies and the
// sessions it sustains; the sessions feed the ceiling, and a mode with no
// nodes occupies none.
func TestServingModesConfig(t *testing.T) {
	dir, _ := isolateUsageTestEnv(t)
	cfgPath := filepath.Join(dir, "config.yaml")
	load := func(raw string) error {
		if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := config.Load(cfgPath)
		return err
	}
	base := `capacity:
  providers:
    local-model:
      kind: self-hosted
      serveModeCommand: [serve-mode]
      serveModeSwitchCommand: [serve-mode]
`
	if err := load(base + `      serveModeModes:
        full: {nodes: [node-a, node-b], sessions: 4}
        tiny: {sessions: 1}
`); err != nil {
		t.Fatalf("valid serveModeModes rejected: %v", err)
	}
	cases := []struct{ name, raw, want string }{
		{"sessions without a mode reader", `capacity:
  providers:
    local-model:
      kind: self-hosted
      serveModeModes:
        full: {sessions: 3}
`, "serveModeCommand"},
		{"a negative sessions", base + `      serveModeModes:
        full: {sessions: -1}
`, "negative"},
		{"an empty mode name", base + `      serveModeModes:
        "": {sessions: 1}
`, "empty mode name"},
		{"a mode entry that is no mapping", base + `      serveModeModes:
        full: 4
`, "unmarshal"},
		{"a serveModeCeilings entry the modes contradict", base + `      serveModeCeilings:
        full: 3
      serveModeModes:
        full: {sessions: 4}
`, "serveModeCeilings"},
		{"a switch command with no mode reader", `capacity:
  providers:
    local-model:
      kind: self-hosted
      serveModeSwitchCommand: [serve-mode]
`, "serveModeCommand"},
	}
	for _, c := range cases {
		if err := load(c.raw); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want it to name %q", c.name, err, c.want)
		}
	}
}

// TestServingModesConstraintRefusal: a mode occupying the claimed node never
// switches in while the constraint holds; the ceiling of the mode without
// the node stands meanwhile; a mode occupying no constrained node still
// switches in.
func TestServingModesConstraintRefusal(t *testing.T) {
	s := newServingModesTest(t)

	if out, err := s.claimNode("GpuJob"); err != nil {
		t.Fatalf("claim: %v\n%s", err, out)
	}
	if line := s.capacity(); !strings.Contains(line, "provider local-model: 0 busy of ceiling 1; serving mode "+servingModeTiny) {
		t.Fatalf("capacity while the constraint holds:\n%s", line)
	}

	// A request to switch into the full mode is refused; the mode stands.
	if err := s.app(&bytes.Buffer{}).switchServeMode(context.Background(), selfHostedProv, servingModeFull, 0); err == nil {
		t.Fatal("switching into the full mode was not refused while the constraint holds")
	}
	if m := s.mode(); m != servingModeTiny {
		t.Fatalf("the mode is %q after a refused switch, want %q", m, servingModeTiny)
	}
	if evs := s.events(verbServeRefused); len(evs) == 0 || !strings.Contains(evs[len(evs)-1].Detail, servingModeFull) {
		t.Fatalf("the refusal left no event:\n%s", eventLines(evs))
	}

	// A mode that occupies no constrained node still switches in.
	if err := s.app(&bytes.Buffer{}).switchServeMode(context.Background(), selfHostedProv, "single", 0); err != nil {
		t.Fatalf("switch into a mode with no nodes: %v", err)
	}
	if m := s.mode(); m != "single" {
		t.Fatalf("the mode is %q, want single", m)
	}
}

// TestServingModesDrainAndSwitch: a constraint requested while a mode that
// occupies the node runs lowers the ceiling to what the mode without the
// node sustains, drains (no new sessions; running ones finish or park),
// switches, and grants the constraint.
func TestServingModesDrainAndSwitch(t *testing.T) {
	s := newServingModesTest(t)
	s.setMode(servingModeFull)
	s.agents("BusyOne")

	// The full mode runs with one session on the model; the claim arrives.
	line, err := s.claimNode("GpuJob")
	if err != nil {
		t.Fatalf("claim during the full mode: %v\n%s", err, line)
	}
	if !strings.Contains(line, servingModeTiny) {
		t.Fatalf("the claim line says nothing of the switch to %s: %q", servingModeTiny, line)
	}
	if m := s.mode(); m != servingModeTiny {
		t.Fatalf("the mode is %q after the claim, want %q", m, servingModeTiny)
	}

	// The drain's ceiling stands: the full mode's session stayed, so the
	// tiny ceiling of 1 holds the provider full and no new session starts.
	if line := s.capacity(); !strings.Contains(line, "1 busy of ceiling 1") || !strings.Contains(line, "no start: 1 busy at the ceiling of 1") {
		t.Fatalf("capacity while the constraint holds:\n%s", line)
	}

	// The drain and the switch are in the event log, in order.
	evs := s.events(verbServeDrain, verbServeSwitch)
	if len(evs) < 2 || evs[0].Verb != verbServeDrain || evs[1].Verb != verbServeSwitch {
		t.Fatalf("the drain and switch are not in the event log:\n%s", eventLines(evs))
	}
	if !strings.Contains(eventLines(evs), servingModeFull+" -> "+servingModeTiny) {
		t.Fatalf("the switch event names no modes:\n%s", eventLines(evs))
	}

	// The constraint is granted: the lease is held by the requester.
	h, err := lease.Dir(s.cfg.LeaseDir).Get(fullModeNode)
	if err != nil || h == nil || h.Name != "GpuJob" {
		t.Fatalf("the constraint lease after the switch: %+v (%v)", h, err)
	}
}

// TestServingModesSwitchBackAndRaise: once the constraint is released and no
// other constraint needs the node, the mode switches back and the ceiling is
// raised again; a holder still standing on the node keeps the mode down.
func TestServingModesSwitchBackAndRaise(t *testing.T) {
	s := newServingModesTest(t)

	if out, err := s.claimNode("GpuJob"); err != nil {
		t.Fatalf("claim: %v\n%s", err, out)
	}
	if out, err := s.releaseNode("GpuJob"); err != nil {
		t.Fatalf("release: %v\n%s", err, out)
	}
	if m := s.mode(); m != servingModeFull {
		t.Fatalf("the mode is %q after the release, want %q", m, servingModeFull)
	}
	if line := s.capacity(); !strings.Contains(line, "provider local-model: 0 busy of ceiling 3; serving mode "+servingModeFull) {
		t.Fatalf("capacity after the switch back:\n%s", line)
	}
	evs := s.events(verbServeSwitch)
	if len(evs) == 0 || !strings.Contains(evs[len(evs)-1].Detail, servingModeTiny+" -> "+servingModeFull) {
		t.Fatalf("no switch-back event:\n%s", eventLines(evs))
	}

	// A claim of a node nobody holds runs the drain it asks for: the
	// model comes into the narrow mode, the ceiling of the mode without
	// the node, and the lease is granted.
	if out, err := s.claimNode("GpuJobA"); err != nil {
		t.Fatalf("claim: %v\n%s", err, out)
	}
	if m := s.mode(); m != servingModeTiny {
		t.Fatalf("the mode is %q after the claim, want %q", m, servingModeTiny)
	}
	// The holder's own retry needs no switch: the node is granted as it
	// stands, and a further claim waits behind the constraint already
	// asked for.
	if out, err := s.claimNode("GpuJobA"); err != nil {
		t.Fatalf("the holder's retry: %v\n%s", err, out)
	}
	if m := s.mode(); m != servingModeTiny {
		t.Fatalf("the mode is %q beside its holder, want %q", m, servingModeTiny)
	}
	if out, err := s.claimNode("GpuJobB"); err == nil || !strings.Contains(err.Error(), "waits behind") {
		t.Fatalf("a claim behind the holder of the node:\n%s\nerr: %v", out, err)
	}
	if m := s.mode(); m != servingModeTiny {
		t.Fatalf("the mode is %q while the holder stands, want %q", m, servingModeTiny)
	}
	// The release of the node: the node free, the model switches back.
	if out, err := s.releaseNode("GpuJobA"); err != nil {
		t.Fatalf("release: %v\n%s", err, out)
	}
	if m := s.mode(); m != servingModeFull {
		t.Fatalf("the mode is %q after the release, want %q", m, servingModeFull)
	}
	// A drain of the node stands while its sessions are still on the
	// model: the claim waits behind it, and the release retries the
	// switch the drain asked for — the node comes down and stays down
	// until the constraint falls.
	s.setMode(servingModeTiny)
	s.agents("BusyOne", "BusyTwo", "BusyThree", "BusyFour")
	if out, err := s.claimNode("GpuJob"); err != nil {
		t.Fatalf("the claim that starts the drain: %v\n%s", err, out)
	}
	_, _ = s.releaseNode("GpuJob")
	s.setMode(servingModeFull)
	if out, err := s.claimNode("GpuJobA"); err == nil || !strings.Contains(err.Error(), "drains") {
		t.Fatalf("a claim over the model's sessions:\n%s\nerr: %v", out, err)
	}
	if m := s.mode(); m != servingModeFull {
		t.Fatalf("the mode is %q while the drain waits, want %q", m, servingModeFull)
	}
	// The waiting claim of the same node waits too: the model drains to
	// the mode the constraint asked for, and no session starts meanwhile.
	if out, err := s.claimNode("GpuJobB"); err == nil || !strings.Contains(err.Error(), "drains") {
		t.Fatalf("a claim beside the drain:\n%s\nerr: %v", out, err)
	}
	// The sessions are gone: the release retries the drain's switch and
	// the model comes down, then comes back once the constraint falls.
	s.agents()
	if out, err := s.releaseNode("GpuJobA"); err != nil {
		t.Fatalf("release of the drain: %v\n%s", err, out)
	}
	if m := s.mode(); m != servingModeTiny {
		t.Fatalf("the mode is %q after the retried drain, want %q", m, servingModeTiny)
	}
	// The waiting claim takes the node and the model stays in the narrow
	// mode it needs.
	if out, err := s.claimNode("GpuJobB"); err != nil {
		t.Fatalf("the waiting claim: %v\n%s", err, out)
	}
	if m := s.mode(); m != servingModeTiny {
		t.Fatalf("the mode is %q while the further holder stands, want %q", m, servingModeTiny)
	}
	if out, err := s.releaseNode("GpuJobB"); err != nil {
		t.Fatalf("release of the further holder: %v\n%s", err, out)
	}
	if m := s.mode(); m != servingModeFull {
		t.Fatalf("the mode is %q after the last release, want %q", m, servingModeFull)
	}
}

// TestServingModesFailureLeavesMode: every failure leaves the mode as the
// command reports it — a failing switch keeps the old mode and is retried;
// a reader disagreeing with the recorded mode re-bases the state on what it
// reports, never on what was recorded.
func TestServingModesFailureLeavesMode(t *testing.T) {
	s := newServingModesTest(t)
	// The fake switcher: a switch into the tiny mode fails while the tries
	// file MODE_FAIL_TINY names stands below the count it asks for — the
	// standing constraint is its preflight. A failed try never writes the
	// mode file: the mode stays as it was.
	s.fakeSwitch(`case "$1" in
  tiny)
    if [ -n "$MODE_FAIL_TINY" ]; then
      n=0; [ -f "$MODE_FAIL_TINY" ] && n=$(cat "$MODE_FAIL_TINY")
      n=$((n + 1)); printf '%s\n' "$n" > "$MODE_FAIL_TINY"
      if [ "$n" -lt "${MODE_FAIL_TINY_N:-3}" ]; then echo "the node is in use" >&2; exit 1; fi
    fi ;;
esac
printf '%s\n' "$1" > "$MODE_DIR/mode"
echo "switched to $1"
`)

	// The switch into the tiny mode fails its preflight: the constraint is
	// not granted, the mode stands as the reader reports it.
	s.setMode(servingModeFull)
	if err := s.app(&bytes.Buffer{}).switchServeMode(context.Background(), selfHostedProv, servingModeTiny, 0); err == nil ||
		!strings.Contains(err.Error(), "the node is in use") {
		t.Fatalf("switch into the tiny mode: %v", err)
	}
	if m := s.mode(); m != servingModeFull {
		t.Fatalf("the mode is %q after the failed switch, want %q", m, servingModeFull)
	}
	evs := s.events(verbServeSwitch)
	if len(evs) == 0 || !strings.Contains(evs[len(evs)-1].Detail, servingModeFull+" -> "+servingModeTiny) ||
		!strings.Contains(evs[len(evs)-1].Detail, "the node is in use") {
		t.Fatalf("the failed switch is not in the event log:\n%s", eventLines(evs))
	}

	// The pending request retries: once the reason is gone, the switch runs.
	if err := os.WriteFile(filepath.Join(s.dir, ".tries"), []byte("3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if line, err := s.claimNode("GpuJob"); err != nil {
		t.Fatalf("claim after the reason passed: %v\n%s", err, line)
	}
	if m := s.mode(); m != servingModeTiny {
		t.Fatalf("the mode is %q after the claim, want %q", m, servingModeTiny)
	}

	// The release asks for the switch back, and it runs: the model comes
	// up, its ceiling with it. A switch back that fails keeps the mode as
	// reported and stays pending for the watch — the next case.
	if out, err := s.releaseNode("GpuJob"); err != nil {
		t.Fatalf("release: %v\n%s", err, out)
	}
	if m := s.mode(); m != servingModeFull {
		t.Fatalf("the mode is %q after the release, want %q", m, servingModeFull)
	}
	if line := s.capacity(); !strings.Contains(line, "provider local-model: 0 busy of ceiling 3; serving mode "+servingModeFull) {
		t.Fatalf("capacity after the switch back:\n%s", line)
	}

	// The switch into the tiny mode fails again: a second hand asks for
	// the narrow mode and the preflight still refuses — the mode stands
	// as the reader reports it, the request stays for the watch, and the
	// event log says so.
	if err := os.WriteFile(filepath.Join(s.dir, ".tries"), []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.app(&bytes.Buffer{}).switchServeMode(context.Background(), selfHostedProv, servingModeTiny, 0); err == nil ||
		!strings.Contains(err.Error(), "the node is in use") {
		t.Fatalf("second switch into the tiny mode: %v", err)
	}
	if m := s.mode(); m != servingModeFull {
		t.Fatalf("the mode is %q after the failed second switch, want %q", m, servingModeFull)
	}
	if evs := s.events(verbServeSwitch); len(evs) == 0 || !strings.Contains(evs[len(evs)-1].Detail, servingModeFull+" -> "+servingModeTiny) ||
		!strings.Contains(evs[len(evs)-1].Detail, "the node is in use") {
		t.Fatalf("the failed second switch is not in the event log:\n%s", eventLines(evs))
	}
	// The reason falls: the claim runs the switch the failed ones asked
	// for, and the node is granted in the narrow mode.
	if err := os.WriteFile(filepath.Join(s.dir, ".tries"), []byte("3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if line, err := s.claimNode("GpuJob"); err != nil {
		t.Fatalf("claim with the reason gone: %v\n%s", err, line)
	}
	if m := s.mode(); m != servingModeTiny {
		t.Fatalf("the mode is %q after the claim, want %q", m, servingModeTiny)
	}
	// The holder releases the node; the switch back runs and the ceiling
	// rises again.
	if out, err := s.releaseNode("GpuJob"); err != nil {
		t.Fatalf("release: %v\n%s", err, out)
	}
	// The watch keeps its eye on the model: a settled machine never
	// switches it again.
	for range 4 {
		w := s.app(&bytes.Buffer{}).newWatcher(false, false)
		if err := w.reconcileServeModes(context.Background()); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		s.fresh()
		if s.mode() == servingModeFull {
			break
		}
	}
	if m := s.mode(); m != servingModeFull {
		t.Fatalf("the retried switch back left the mode %q, want %q", m, servingModeFull)
	}
	if line := s.capacity(); !strings.Contains(line, "provider local-model: 0 busy of ceiling 3; serving mode "+servingModeFull) {
		t.Fatalf("the retried switch back did not raise the ceiling:\n%s", line)
	}
}

// TestServingModesSwitchOnceAndFromElsewhere: one switch at a time, never
// from a session running on the model, and the switch's own timeout.
func TestServingModesSwitchOnceAndFromElsewhere(t *testing.T) {
	s := newServingModesTest(t)

	// Never from a session on that model: this caller is the omp agent
	// beekeeper started on the self-hosted provider.
	s.agents("OnTheModel")
	a := s.app(&bytes.Buffer{})
	a.as = "OnTheModel"
	if err := a.switchServeMode(context.Background(), selfHostedProv, servingModeFull, 0); err == nil ||
		!strings.Contains(err.Error(), "never from a session") {
		t.Fatalf("a switch from the model's own session: %v", err)
	}
	if m := s.mode(); m != servingModeTiny {
		t.Fatalf("the mode is %q after a refused self-switch, want %q", m, servingModeTiny)
	}

	// One switch at a time: a switch asked while the machine's lock stands
	// refuses instead of starting a second one. The lock of a switch whose
	// process is gone is reaped, never left to hold the machine's switches
	// forever; the lock of a live process — even this very one, whose own
	// switch runs in a goroutine — refuses a second switch.
	s.fakeSwitch(`printf '%s\n' "$1" > "$MODE_DIR/mode"
`)
	lockPath := filepath.Join(s.cfg.StateDir, serveModeLockFile(selfHostedProv))
	if err := os.WriteFile(lockPath, []byte("999999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.app(&bytes.Buffer{}).switchServeMode(context.Background(), selfHostedProv, servingModeFull, 0); err != nil {
		t.Fatalf("the lock of a gone process was not reaped: %v", err)
	}
	if m := s.mode(); m != servingModeFull {
		t.Fatalf("the switch behind a stale lock left the mode %q, want %q", m, servingModeFull)
	}
	// The running switch's own process holds the lock; a second switch
	// asked beside it refuses with the lock named.
	inside := make(chan struct{})
	firstDone := make(chan struct{})
	s.fakeSwitch(`printf '%s\n' "$1" > "$MODE_DIR/mode"
sleep 2
`)
	go func() {
		a := s.app(&bytes.Buffer{})
		a.serveModeSwitchHeld = func(string) { close(inside) }
		_ = a.switchServeMode(context.Background(), selfHostedProv, servingModeTiny, 0)
		close(firstDone)
	}()
	<-inside
	//nolint:gosec // the test's own lock file
	if raw, rerr := os.ReadFile(lockPath); rerr != nil || strings.TrimSpace(string(raw)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("the running switch does not hold the machine's lock: %q %v", raw, rerr)
	}
	err := s.app(&bytes.Buffer{}).switchServeMode(context.Background(), selfHostedProv, servingModeFull, 0)
	if err == nil || !strings.Contains(err.Error(), "another serving-mode switch of") || !strings.Contains(err.Error(), "only one runs at a time") {
		t.Fatalf("a second switch ran beside the first: %v", err)
	}
	// The running switch finishes; its mode stands, and its lock is gone.
	<-firstDone
	s.fresh()
	if m := s.mode(); m != servingModeTiny {
		t.Fatalf("the running switch left the mode %q, want %q", m, servingModeTiny)
	}
	if _, lerr := os.Stat(lockPath); !errors.Is(lerr, os.ErrNotExist) {
		t.Fatalf("the finished switch left its machine lock: %v", lerr)
	}

	// Its own timeout: a switch that outlives it ends with the timeout
	// named; the mode is whatever the switcher left on disk. The switch
	// runs through the real command path: the timeout belongs to the
	// command's own context.
	s.fakeSwitch("sleep 30\n")
	done := make(chan error, 1)
	go func() {
		a := s.app(&bytes.Buffer{})
		// The real command path: the timeout belongs to the command's own
		// context. The switch command is the scratch dir's fake script.
		a.switchServeModeRun = func(ctx context.Context, command []string, mode string) (string, error) {
			return runServeModeSwitchCommand(ctx, config.Provider{ServeModeSwitch: command}, mode, s.dir, s.fakeEnv)
		}
		done <- a.switchServeMode(context.Background(), selfHostedProv, servingModeTiny, 200*time.Millisecond)
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("a switch past its timeout: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the switch ran past its timeout")
	}
}

func TestServingModesStatusAndWatch(t *testing.T) {
	s := newServingModesTest(t)

	if line := s.statusLine(); !strings.Contains(line, "serving mode "+servingModeTiny+" (1 session)") {
		t.Fatalf("status without a constraint:\n%s", line)
	}

	// The watch reads the mode and says nothing while it is settled.
	w := s.app(&bytes.Buffer{}).newWatcher(false, false)
	if err := w.reconcileServeModes(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if conds := openConditions(w); conds != "" {
		t.Fatalf("a settled reconcile opened conditions:\n%s", conds)
	}

	// A constraint requested while the full mode runs drains it: the drain
	// and the switch are events, the status names the constraint and the
	// mode that stands.
	s.setMode(servingModeFull)
	s.agents("BusyOne")
	if out, err := s.claimNode("GpuJob"); err != nil {
		t.Fatalf("claim during the full mode: %v\n%s", err, out)
	}
	if line := s.statusLine(); !strings.Contains(line, fullModeNode+" held (serving mode "+servingModeTiny+", 1 session)") {
		t.Fatalf("status while the constraint holds:\n%s", line)
	}
	if evs := s.events(verbServeDrain); len(evs) == 0 {
		t.Fatal("the drain left no event")
	}
	if !strings.Contains(eventLines(s.events(verbServeSwitch)), servingModeFull+" -> "+servingModeTiny) {
		t.Fatalf("the switch over the constraint is not in the event log:\n%s", eventLines(s.events(verbServeSwitch)))
	}
}

// openConditions returns the watch's open condition lines and clears them.
func openConditions(w *watcher) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var lines []string
	for k, c := range w.active {
		lines = append(lines, k+": "+c.Label)
		delete(w.active, k)
	}
	return strings.Join(lines, "\n")
}
