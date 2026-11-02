//go:build linux && !nosystemd

package cmd

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/state"
)

// Use a real PID-namespace init and brokered scope. The sandbox's initial
// shell stays alive while its scoped command leaves a child behind.
func TestWatchStopsSandboxOrphan(t *testing.T) {
	if !plat.Launcher.Available() {
		t.Skip("no user systemd")
	}
	if _, err := exec.LookPath(sandboxInitCommand); err != nil {
		t.Skip("no bubblewrap")
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	spool := sandbox.SpoolDir(dir)
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		served <- sandbox.Serve(ctx, spool, "/proc", 5*time.Millisecond,
			brokered(brokeredCap(plat.Capper, platform.Cap{}), nil))
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("scratch broker: %v", err)
		}
	})

	unit := fmt.Sprintf("memcap-test-%d-%06d", os.Getpid(), time.Now().Nanosecond()%1000000)
	slice := guard.Options{Test: true}.SlotSlice(9)
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = plat.Launcher.Stop(stopCtx, unit+".scope")
	})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	capper := sandbox.Capper{Dir: spool, Exe: exe}
	scoped, err := capper.Command(unit, platform.Cap{Slice: slice, Max: "64M", Swap: "0"},
		[]string{"sh", "-c", "sleep 300 >/dev/null 2>&1 &"})
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--unshare-pid", "--die-with-parent", "--ro-bind", "/", "/", "--bind", spool, spool, "--dev", "/dev", "sh", "-c",
		`"$@" && echo ready; read -r finish`, "sh"}
	cmd := exec.CommandContext(ctx, sandboxInitCommand, append(args, scoped.Args...)...) //nolint:gosec // this test's binary, scratch spool and scope inside bubblewrap
	cmd.Env = append(os.Environ(), "BEEKEEPER_TEST_MAIN=1",
		"BEEKEEPER_CONFIG="+emptyTestConfig(t, filepath.Join(dir, "none.yaml")))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = in.Write([]byte("done\n")); _ = in.Close(); _ = cmd.Wait() })
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("sandbox startup: %q, %v: %s", line, err, &stderr)
	}
	scope := plat.Machine.MemcapScope(unit + ".scope")
	if scope == nil {
		t.Fatal("the background child did not keep its scope alive")
	}
	pids := plat.Machine.ScopePIDs(scope.Path)
	table, err := proc.Read()
	if err != nil || len(pids) != 1 {
		t.Fatalf("scope processes %v, table error %v", pids, err)
	}
	child := table.ByPID[pids[0]]
	if child == nil || table.ByPID[child.PPID] == nil || table.ByPID[child.PPID].Comm != sandboxInitCommand {
		t.Fatalf("child %v was not adopted by bubblewrap", child)
	}
	if table.Orphaned(child) {
		t.Fatal("generic orphan detection unexpectedly recognizes the sandbox init")
	}
	if _, err := capper.Reap(unit); err != nil || len(plat.Machine.ScopePIDs(scope.Path)) != 1 {
		t.Fatalf("sandbox Reap changed the reproduction: %v", err)
	}
	t.Logf("scoped child %d adopted by bwrap %d; sandbox Reap left it running", child.PID, child.PPID)

	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMCAP_STATE", "")
	t.Setenv("MEMCAP_SLOTS", "")
	cfg := &config.Config{StateDir: dir, Memcap: config.Memcap{SlotDir: dir, Slots: 1}}
	var output bytes.Buffer
	a := &app{cfg: cfg, store: store, out: &output}
	saved := plat.Machine
	plat.Machine = &slotMachine{Machine: saved,
		slices: map[string][]string{guard.Options{SlotDir: dir}.SlotSlice(1): {unit + ".scope"}},
		pids:   map[string][]int{unit + ".scope": pids}}
	t.Cleanup(func() { plat.Machine = saved })
	w := a.newWatcher(false, false)
	now := time.Now()
	w.slotOrphans(now, table)
	w.slotOrphans(now.Add(2*slotOrphanGrace), table)
	if strings.Contains(output.String(), "STOPPED") || len(saved.ScopePIDs(scope.Path)) != 1 {
		t.Fatal("a reporting-only watch stopped the sandbox child")
	}
	w.slotsOn = true
	w.slotOrphans(now.Add(2*slotOrphanGrace), table)
	if !strings.Contains(output.String(), "STOPPED orphaned slot scope "+unit+".scope") {
		t.Fatalf("watch did not stop the sandbox orphan: %s", &output)
	}
	if left := saved.ScopePIDs(scope.Path); len(left) != 0 {
		t.Fatalf("processes remain after stopping the scope: %v", left)
	}
	t.Log("watch stopped the real sandbox scope after its grace period")
}
