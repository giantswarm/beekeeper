package guard

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/platform"
)

func TestParseClass(t *testing.T) {
	for in, want := range map[string]string{"": ClassBuild, "build": ClassBuild, "benchmark": ClassBenchmark, "lab": ClassLab} {
		if got, err := ParseClass(in); err != nil || got != want {
			t.Errorf("ParseClass(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseClass("long"); err == nil || !strings.Contains(err.Error(), "build, benchmark, lab") {
		t.Errorf("ParseClass(long): %v, want the classes named", err)
	}
}

func TestDefaultClass(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{testShell, "-c", "cd ~/lab && agentlab platform"}, ClassLab},
		{[]string{"agentlab", "agents-test"}, ClassLab},
		{[]string{testShell, "-c", "go test ./... && agentlab platform"}, ClassBuild},
		{[]string{testShell, "-c", "agentlab --help"}, ClassBuild},
		{[]string{testShell, "-c", "python3 benchmark_preset.py"}, ClassBuild},
		{[]string{testShell, "-c", "grep agentlab README.md"}, ClassBuild},
		{[]string{"go", "build", "./..."}, ClassBuild},
	} {
		if got := DefaultClass(c.argv); got != c.want {
			t.Errorf("DefaultClass(%q) = %q, want %q", c.argv, got, c.want)
		}
	}
}

func TestBuildsNothing(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want bool
	}{
		{[]string{"true"}, true},
		{[]string{"/usr/bin/false"}, true},
		{[]string{"echo", "done"}, true},
		{[]string{testShell, "-c", "printf '%s' x"}, true},
		{[]string{"go", "version"}, true},
		{[]string{testShell, "-c", "make help"}, true},
		{[]string{"kubectl", "--help"}, true},
		{[]string{"go", "test", "./..."}, false},
		{[]string{testShell, "-c", "make"}, false},
		{[]string{testShell, "-c", "true && go test ./..."}, false},
		{[]string{testShell, "-c", "echo $(go build ./...)"}, false},
		{[]string{testShell, "-c", "echo x > out"}, false},
		{[]string{"./build.sh"}, false},
		{[]string{testShell, "-c", "  "}, false},
	} {
		if got := BuildsNothing(c.argv); got != c.want {
			t.Errorf("BuildsNothing(%q) = %v, want %v", c.argv, got, c.want)
		}
	}
}

// A benchmark or a lab proof takes no build slot: with every slot held it
// runs at once, in memcap.slice under its own cap, and caps no slot's slice.
func TestRunOfASlotlessClassTakesNoSlot(t *testing.T) {
	for _, class := range []string{ClassBenchmark, ClassLab} {
		f := &fakeCapper{}
		saved := plat.Capper
		plat.Capper = f
		dir := t.TempDir()
		holdSlot(t, dir, 1, "other")
		t.Setenv("CLAUDE_CODE_SESSION_ID", "bench")
		var stderr bytes.Buffer
		o := Options{Max: "1M", Swap: "0", SlotDir: dir, Slots: 1, Stderr: &stderr, Class: class}
		rc := Run(o, trueCmd)
		plat.Capper = saved
		if rc != 0 {
			t.Fatalf("%s with every slot held: exit %d, want 0:\n%s", class, rc, stderr.String())
		}
		if len(f.capped) != 0 || !slices.Equal(f.ran, []string{MemcapSlice}) {
			t.Errorf("%s: capped %v, ran in %v: want no slot capped, run in %s", class, f.capped, f.ran, MemcapSlice)
		}
		if !strings.Contains(stderr.String(), "a "+class+" run takes no build slot") {
			t.Errorf("%s: stderr does not say it takes no slot:\n%s", class, stderr.String())
		}
	}
}

// lowMem is the machine with too little memory available for any cap.
type lowMem struct{ platform.Machine }

func (lowMem) Mem() (machine.Mem, error) { return machine.Mem{AvailableMiB: 1}, nil }

func TestRunOfASlotlessClassWaitsForItsMemory(t *testing.T) {
	f := &fakeCapper{}
	savedC, savedM := plat.Capper, plat.Machine
	plat.Capper, plat.Machine = f, lowMem{Machine: savedM}
	t.Cleanup(func() { plat.Capper, plat.Machine = savedC, savedM })
	var stderr bytes.Buffer
	o := Options{Max: "8G", Swap: "0", SlotDir: t.TempDir(), Slots: 1, Stderr: &stderr, Class: ClassBenchmark}
	if rc := Run(o, trueCmd); rc != ExitBusy {
		t.Fatalf("exit %d, want %d", rc, ExitBusy)
	}
	if !strings.Contains(stderr.String(), "gave up after 0s waiting for memory: 1 MiB available, the benchmark run's cap needs 8192") ||
		!strings.Contains(stderr.String(), "not run: true") || len(f.ran) != 0 {
		t.Errorf("ran %v, stderr:\n%s", f.ran, stderr.String())
	}
}

// Exit 75's lines name each slot's holder and since when, not its raw
// record.
func TestBusyRunNamesTheHolderAndSince(t *testing.T) {
	f := &fakeCapper{}
	saved := plat.Capper
	plat.Capper = f
	t.Cleanup(func() { plat.Capper = saved })
	dir := t.TempDir()
	holdSlot(t, dir, 1, "other")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "late")
	var stderr bytes.Buffer
	o := Options{Max: "1M", Swap: "0", SlotDir: dir, Slots: 1, Stderr: &stderr}
	if rc := Run(o, trueCmd); rc != ExitBusy {
		t.Fatalf("exit %d, want %d", rc, ExitBusy)
	}
	if !strings.Contains(stderr.String(), "  slot 1: other (pid 1) since 14:00Z (") ||
		!strings.Contains(stderr.String(), "): agentlab backstage-test\n") || strings.Contains(stderr.String(), `"session"`) {
		t.Errorf("the busy exit does not name the holder and since when:\n%s", stderr.String())
	}
}

func TestHolderText(t *testing.T) {
	now := time.Date(2026, 10, 9, 23, 20, 0, 0, time.UTC)
	for raw, want := range map[string]string{
		`{"pid":7,"session":"s1","name":"Agent four","cmd":"python3 benchmark_preset.py","since":"2026-10-09T20:27:00Z"}`: "Agent four (pid 7) since 20:27Z (2h53m0s): python3 benchmark_preset.py",
		`{"pid":7,"session":"s1","cmd":"go test ./..."}`:                                                                  "s1 (pid 7): go test ./...",
		`{"pid":7,"cmd":"make"}`: "pid 7: make",
		`{broken`:                "{broken",
	} {
		if got := HolderText(raw, now); got != want {
			t.Errorf("HolderText(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestHolderRecordsTheSessionName(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_NAME", "Agent four")
	o := Options{SlotDir: t.TempDir(), Max: "4G"}
	writeHolder(o, 1, trueCmd)
	raw, err := os.ReadFile(filepath.Join(o.SlotDir, "1.holder")) //nolint:gosec // the test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	if h, ok := ParseHolder(string(raw)); !ok || h.Name != "Agent four" {
		t.Errorf("holder %+v from %s: want the session's name", h, raw)
	}
}
