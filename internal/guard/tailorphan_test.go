package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Capture must not wait for an orphan: a command that leaves a background
// child behind (a test's envtest kube-apiserver) inherits the run's output
// pipes, and a Wait that also waits for the pipes' EOF would outlive the
// command by the child's lifetime — holding the slot, the tail and the end
// event. scope answers when the process exits; the pipe copy drains after
// the scope's reap.
func TestScopeReturnsBeforeItsOrphanEnds(t *testing.T) {
	f := &fakeCapper{}
	savedC, savedM := plat.Capper, plat.Machine
	plat.Capper, plat.Machine = f, noKills{Machine: savedM}
	t.Cleanup(func() { plat.Capper, plat.Machine = savedC, savedM })
	o := Options{Max: "64M", Swap: "0", SlotDir: t.TempDir(), Slots: 1, Wait: time.Second,
		Tail: NewTail(1024), TailDir: t.TempDir()}
	start := time.Now()
	rc, drain := scope(f, "memcap-test-scope-1", o, 1, []string{"sh", "-c", "sleep 2 & exit 1"}, nil, func(string, ...any) {})
	el := time.Since(start)
	if rc != 1 {
		t.Errorf("exit %d, want the command's 1", rc)
	}
	if el > time.Second {
		t.Errorf("scope returned after %s: the capture waited for the two-second orphan", el)
	}
	drain()
}

// The whole path: an orphaned child in the run's scope is stopped by the
// reap, the output copy ends with it, and the run keeps its masked tail
// and its end event — all of it long before a thirty-minute child would
// have ended.
func TestRunKeepsTheTailDespiteAnOrphan(t *testing.T) {
	dir := t.TempDir()
	f := &scopeCapper{pids: filepath.Join(t.TempDir(), "left"), lock: filepath.Join(dir, "1.lock")}
	savedC, savedM := plat.Capper, plat.Machine
	plat.Capper, plat.Machine = f, noKills{Machine: savedM}
	t.Cleanup(func() { plat.Capper, plat.Machine = savedC, savedM })
	var stderr strings.Builder
	var ends []string
	o := Options{Max: "1M", Swap: "0", SlotDir: dir, Slots: 1, Wait: time.Second, Stderr: &stderr,
		Record: func(verb, detail string) {
			if verb == VerbEnd {
				ends = append(ends, detail)
			}
		}}
	o.Tail = NewTail(1024)
	o.TailDir = filepath.Join(dir, "tails")
	start := time.Now()
	rc := Run(o, []string{"sh", "-c", `sleep 1800 & echo $! > "$0"; echo '--- FAIL: TestOrphan (0.02s)'; exit 1`, f.pids})
	el := time.Since(start)
	if rc != 1 {
		t.Fatalf("exit %d, want 1:\n%s", rc, stderr.String())
	}
	if el > 15*time.Second {
		t.Errorf("the run waited %s on its orphan:\n%s", el, stderr.String())
	}
	if len(f.reaped) != 1 {
		t.Errorf("reaped %v: want the scope stopped once", f.reaped)
	}
	tails, err := os.ReadDir(o.TailDir)
	if err != nil || len(tails) != 1 {
		t.Fatalf("tails %v (%v), want the failed run's one", tails, err)
	}
	raw, err := os.ReadFile(filepath.Join(o.TailDir, tails[0].Name())) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "--- FAIL: TestOrphan") {
		t.Errorf("the tail missed the output written before the orphan outlived the command:\n%s", raw)
	}
	if len(ends) != 1 || !strings.Contains(ends[0], "exit 1 after") {
		t.Errorf("end event %q", ends)
	}
}
