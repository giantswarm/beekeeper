//go:build perf && linux

package perfsuite

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/state"
)

// runnerUnitsIssue is the open issue that has parked and off-roster sessions
// keep their runner units.
const runnerUnitsIssue = "https://github.com/giantswarm/beekeeper/issues/3675"

// footprint budgets per session: processes and MiB of its runner unit.
var footprintBudgets = map[string]struct{ procs, mem limit }{
	"working":    {procs: limit{Max: 3}, mem: limit{Max: 48}},
	"idle":       {procs: limit{Max: 1}, mem: limit{Max: 24}},
	"parked":     {procs: limit{Max: 0, Known: runnerUnitsIssue}, mem: limit{Max: 0, Known: runnerUnitsIssue}},
	"off roster": {procs: limit{Max: 0, Known: runnerUnitsIssue}, mem: limit{Max: 0, Known: runnerUnitsIssue}},
}

// TestSessionFootprint needs a systemd user manager (the nightly job); with
// BEEKEEPER_PERF_SYSTEMD=1 its absence fails instead of skipping. Each
// session's runner is the real codex runner in the unit beekeeper names for
// it, its harness the fake codex; the supervisor loop then runs for a few
// passes and each unit is measured.
func TestSessionFootprint(t *testing.T) {
	requirePerf(t)
	systemdRun, err := exec.LookPath("systemd-run")
	if err == nil {
		err = exec.CommandContext(t.Context(), "systemctl", "--user", "show-environment").Run() //nolint:gosec // fixed command
	}
	if err != nil {
		if os.Getenv("BEEKEEPER_PERF_SYSTEMD") == "1" {
			t.Fatalf("no systemd user manager: %v", err)
		}
		t.Skip("no systemd user manager: the nightly job measures the footprint")
	}
	o := defaultDesk()
	o.systemd = true
	d := newDesk(t, o)
	now := time.Now().UTC()
	type session struct {
		kind, id, unit, inbox string
	}
	var sessions []session
	for _, kind := range []string{"working", "idle", "parked", "off roster"} {
		id := uuid.NewString()
		sessions = append(sessions, session{kind: kind, id: id, unit: "beekeeper-codex-" + id[:8], inbox: codex.InboxPath(d.stateDir, id)})
	}
	if err := d.store.Update(func(st *state.State) ([]state.Event, error) {
		for _, s := range sessions {
			if s.kind == "off roster" {
				continue
			}
			p := state.Party{HostSession: codex.HostPrefix + s.id, Name: "perf codex " + s.kind, Person: fixturePerson, Host: "fixture"}
			ag := state.Agent{Party: p, Registered: now.Add(-time.Hour), ActivityAt: now}
			switch s.kind {
			case "working":
				ag.Task, ag.AssignedAt = "Work https://github.com/"+fixtureRepo+"/issues/900 to the finish", now
			case "idle":
				ag.IdleSince = now.Add(-time.Minute)
			case "parked":
				ag.Task = "Work https://github.com/" + fixtureRepo + "/issues/901 to the finish"
				ag.Park = &state.Park{By: p, At: now.Add(-time.Minute), On: fixtureRepo + "#1901", Waits: "desk review"}
			}
			st.Agents = append(st.Agents, ag)
			st.Starts = append(st.Starts, state.Start{Party: p, Mode: state.ModeBypass, Dir: d.dir, By: state.Party{Name: "perf guide"}, At: now.Add(-time.Hour), Harness: "codex", Model: "fixture-model", Task: ag.Task})
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		if err := codex.MakeInbox(s.inbox); err != nil {
			t.Fatal(err)
		}
		args := []string{"--user", "--quiet", "--collect", "--unit", s.unit, "--working-directory", d.dir}
		for _, kv := range slices.Concat(d.env, []string{"PERF_CODEX_SLEEP=30m", codex.EnvAgent + "=" + s.id}) {
			args = append(args, "--setenv", kv)
		}
		args = append(args, "--", d.bin, "codex-run", s.inbox, codex.LogPath(d.stateDir, s.id), filepath.Join(d.dir, "bin", "codex"), "fixture-model")
		if out, err := exec.CommandContext(t.Context(), systemdRun, args...).CombinedOutput(); err != nil { //nolint:gosec // fixture unit
			t.Fatalf("starting %s: %v: %s", s.unit, err, out)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = exec.CommandContext(ctx, "systemctl", "--user", "stop", s.unit).Run() //nolint:gosec // fixture unit
		})
	}
	// The working session takes a turn: its runner starts the harness.
	deadline := time.Now().Add(30 * time.Second)
	for err = codex.Send(sessions[0].inbox, "fixture turn"); err != nil && time.Now().Before(deadline); err = codex.Send(sessions[0].inbox, "fixture turn") {
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("working session's turn: %v", err)
	}

	l := d.startLoop(t)
	time.Sleep(6*o.loopInterval + 15*time.Second)
	_ = l.usage(t)
	for _, s := range sessions {
		procs, mem := unitFootprint(t, s.unit)
		b := footprintBudgets[s.kind]
		rep.check(t, s.kind+" session", "procs", float64(procs), b.procs)
		rep.check(t, s.kind+" session", "mem", float64(mem)/mib, b.mem)
	}
}

// unitFootprint is the processes and memory of a user unit's cgroup; a unit
// that is gone has none.
func unitFootprint(t *testing.T, unit string) (procs int, mem int64) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "systemctl", "--user", "show", "-p", "ControlGroup", "--value", unit+".service").Output() //nolint:gosec // fixture unit
	cg := strings.TrimSpace(string(out))
	if err != nil || cg == "" {
		return 0, 0
	}
	root := filepath.Join("/sys/fs/cgroup", cg)
	if raw, err := os.ReadFile(filepath.Join(root, "memory.current")); err == nil { //nolint:gosec // cgroup file
		mem, _ = strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	}
	_ = filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || e.Name() != "cgroup.procs" {
			return nil //nolint:nilerr // a cgroup that went away has no processes
		}
		raw, rerr := os.ReadFile(path) //nolint:gosec // cgroup file
		if rerr == nil {
			procs += len(strings.Fields(string(raw)))
		}
		return nil
	})
	t.Logf("%s: %d processes, %s", unit, procs, fmt.Sprintf("%.1f MiB", float64(mem)/mib))
	return procs, mem
}
