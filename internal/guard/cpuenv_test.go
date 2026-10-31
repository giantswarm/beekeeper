package guard

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSlotCores(t *testing.T) {
	for _, c := range []struct {
		quota string
		slots int
		want  int
	}{
		{"800%", 2, 4},
		{"1200%", 2, 6},
		{"400%", 3, 1},
		{"50%", 2, 1},
		{"1250%", 2, 6},
		{"", 2, 0},
		{"max", 2, 0},
		{"800%", 0, 0},
		{"x%", 2, 0},
		{"0%", 2, 0},
	} {
		if got := SlotCores(c.quota, c.slots); got != c.want {
			t.Errorf("SlotCores(%q, %d) = %d, want %d", c.quota, c.slots, got, c.want)
		}
	}
}

// A capped run's command gets its slot's share of the cores as its
// parallelism, unless the caller set one itself.
func TestRunBoundsTheCommandsParallelismToItsSlotsCores(t *testing.T) {
	f := &fakeCapper{}
	saved := plat.Capper
	plat.Capper = f
	t.Cleanup(func() { plat.Capper = saved })
	slots, out := t.TempDir(), filepath.Join(t.TempDir(), "env")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "fresh")
	t.Setenv("GOMAXPROCS", "")
	t.Setenv("CARGO_BUILD_JOBS", "3")
	o := Options{Max: "1M", Swap: "0", SlotDir: slots, Slots: 2, CPUQuota: "800%", Stderr: io.Discard}
	script := `printf '%s %s\n' "$GOMAXPROCS" "$CARGO_BUILD_JOBS" > "$1"`
	if rc := Run(o, []string{"sh", "-c", script, "sh", out}); rc != 0 {
		t.Fatalf("exit %d, want 0", rc)
	}
	raw, err := os.ReadFile(out) //nolint:gosec // the test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(raw)); got != "4 3" {
		t.Errorf("GOMAXPROCS and CARGO_BUILD_JOBS %q, want %q: the slot's 4 of 8 cores, the caller's own 3 kept", got, "4 3")
	}
}
