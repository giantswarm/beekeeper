package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/state"
)

// stubApp is an app on the platform with every part not available, with a
// supervisor recorded and one lease held by a session.
func stubApp(t *testing.T) (*app, *bytes.Buffer) {
	t.Helper()
	orig := plat
	plat = platform.Stub()
	t.Cleanup(func() { plat = orig })
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := "stateDir: " + dir + "\nleaseDir: " + filepath.Join(dir, "leases") + "\n" +
		"memcap: {slotDir: " + filepath.Join(dir, "slots") + "}\n" +
		"claude: {projectsDir: " + filepath.Join(dir, "projects") + ", sessionsDir: " + filepath.Join(dir, "sessions") + "}\n"
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(c.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Supervisor = &state.Supervisor{Party: state.Party{Name: agentOne, Session: "s1"}, Since: time.Now().Add(-time.Hour)}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.Dir(c.LeaseDir).Claim("agentlab-1", lease.Holder{Holder: agentOne, Session: "s1"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	return &app{cfg: c, store: store, now: time.Now(), out: &out, as: agentOne}, &out
}

// Every snapshot section a missing platform part feeds prints one "not
// available" line; the portable ones are filled.
func TestSnapshotSaysEachMissingSection(t *testing.T) {
	a, out := stubApp(t)
	s, err := a.takeSnapshot(context.Background(), a.now.Add(-time.Hour), false, false)
	if err != nil {
		t.Fatalf("takeSnapshot on the stub: %v", err)
	}
	a.printSnapshot(s)
	for _, sec := range []string{secLoad, secPressure, secMemory, secScope, secSessions, secOOM} {
		if n := strings.Count(out.String(), platform.Unavailable(sec)); n != 1 {
			t.Errorf("%q printed %d times, want once:\n%s", platform.Unavailable(sec), n, out)
		}
	}
	for _, want := range []string{"build slots: ", "leases: agentlab-1: " + agentOne + " (" + holderUnknown + ")"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("snapshot lacks %q:\n%s", want, out)
		}
	}
	for _, wrong := range []string{"RAM available", "sessions: 0 CLIs", "OOM kills since", "desktop scope: none found", "(" + holderGone + ")"} {
		if strings.Contains(out.String(), wrong) {
			t.Errorf("snapshot says %q of a section it cannot read:\n%s", wrong, out)
		}
	}
}

// status names the recorded supervisor and says its liveness is unknown.
func TestStatusWithoutSessions(t *testing.T) {
	a, _ := stubApp(t)
	v, err := a.status()
	if err != nil {
		t.Fatalf("status on the stub: %v", err)
	}
	if got, want := v.bar(), "beekeeper\t?"+agentOne+"\t1\t0\t0"; got != want {
		t.Errorf("status --bar = %q, want %q", got, want)
	}
	if got := v.line(); !strings.Contains(got, platform.Unavailable(secSessions)) {
		t.Errorf("status = %q, want it to say %q", got, platform.Unavailable(secSessions))
	}
}

// The watch says each missing section once over many polls and raises no
// condition for it.
func TestWatchSaysMissingSectionsOnce(t *testing.T) {
	a, _ := stubApp(t)
	out := &syncBuffer{}
	a.out = out
	w := a.newWatcher(true, false)
	w.lastBudget = time.Now()
	ctx := context.Background()
	if err := w.run(ctx, true); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		w.sample(ctx)
		w.poll(ctx)
	}
	for _, sec := range []string{secLoad, secPressure, secMemory, secScope, secSessions} {
		if n := strings.Count(out.String(), platform.Unavailable(sec)); n != 1 {
			t.Errorf("%q said %d times, want once:\n%s", platform.Unavailable(sec), n, out)
		}
	}
	for _, alert := range []string{"cannot read", "no Claude Desktop scope", "LOW RAM", "HIGH LOAD", "MEMORY PRESSURE", "SESSIONS"} {
		if strings.Contains(out.String(), alert) {
			t.Errorf("the watch raised %q for a part it does not have:\n%s", alert, out)
		}
	}
	// The conditions a platform part feeds; tmpfs and disk are portable.
	for _, key := range []string{"proc", "journal", "noscope", "avail", "swap", "swapoff", "oomd", "psi", "load", "loadrising", "cpupsi", "slowed", "scopeanon"} {
		if c, ok := w.active[key]; ok {
			t.Errorf("open condition %q (%s) for a part the stub does not have", key, c.Label)
		}
	}
}

// A command that needs a missing part refuses: exit 3 and the reason.
func TestMissingPartRefuses(t *testing.T) {
	a, _ := stubApp(t)
	_, _, err := a.sessions()
	if !platform.Missing(err) || Code(err) != ExitRefused {
		t.Fatalf("sessions on the stub = %v (exit %d), want not available, exit %d", err, Code(err), ExitRefused)
	}
	if got, want := err.Error(), "Machine.Processes: not available on "+platform.Name(); got != want {
		t.Errorf("the refusal says %q, want %q", got, want)
	}
	if err := a.uiCmd().RunE(nil, nil); !platform.Missing(err) || Code(err) != ExitRefused {
		t.Errorf("ui on the stub = %v (exit %d), want not available, exit %d", err, Code(err), ExitRefused)
	}
	if err := plat.Launcher.Start(platform.Unit{Name: "x"}); Code(err) != ExitRefused {
		t.Errorf("an agent's unit start on the stub exits %d, want %d", Code(err), ExitRefused)
	}
}
