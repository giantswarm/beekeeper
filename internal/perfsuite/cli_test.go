//go:build perf && linux

package perfsuite

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// cliBudget is what one call of a command may cost on the desk of record:
// the median of its runs for time, the most of them for the rest.
type cliBudget struct {
	wall, cpu time.Duration
	readMiB   float64
	rssMiB    float64
	children  int
	exits     []int // exit codes a call on this desk may end with
	args      []string
}

// cliRuns is the number of calls per command: the first one cold, after a
// fresh desk, the others warm.
const cliRuns = 3

// cliBudgets are the commands sessions run most. Exit 3 is a refusal (no
// room: the fixture's capacity.ceiling is 0) and 1 a failure a fake caused;
// both still pay the reads and decisions the budget is about.
var cliBudgets = map[string]cliBudget{
	"agents":                 {args: []string{"--as", "perf guide", "agents", "--full"}, exits: []int{0}},
	"agents start --dry-run": {args: []string{"--as", "perf guide", "agents", "start", "perf dry run", "BRIEF", "--dry-run", "--harness", "codex", "--model", "fixture-model", "--dir", "CHECKOUT"}, exits: []int{0, 1, 3}},
	"agents wake":            {args: []string{"--as", "perf guide", "agents", "wake", "perf worker 1", "fixture wake"}, exits: []int{0, 1, 3}},
	"capacity":               {args: []string{"capacity", "--json"}, exits: []int{0, 3}},
	"supervisor status":      {args: []string{"supervisor", "status"}, exits: []int{0}},
	"guide work":             {args: []string{"--as", "perf guide", "guide", "work"}, exits: []int{0}},
	"board show":             {args: []string{"board", "show", fixtureRepo + "#2"}, exits: []int{0, 1}},
	"note list":              {args: []string{"note", "list"}, exits: []int{0}},
	"budget":                 {args: []string{"budget", "--json"}, exits: []int{0}},
}

// Every command's budget, measured on a two-core CI machine when the suite
// was written with headroom for the machine's noise: a call reads the state
// and the event log once at most (6 + 12 MiB), takes less than a second
// of CPU and starts at most a few processes.
func init() {
	for name, b := range cliBudgets {
		b.wall, b.cpu = 2*time.Second, 1500*time.Millisecond
		b.readMiB, b.rssMiB, b.children = 32, 256, 8
		cliBudgets[name] = b
	}
}

func TestCLIBudgets(t *testing.T) {
	requirePerf(t)
	d := newDesk(t, defaultDesk())
	brief := filepath.Join(d.dir, "brief.md")
	if err := os.WriteFile(brief, []byte("Work https://github.com/"+fixtureRepo+"/issues/7 to the finish.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(cliBudgets))
	for name := range cliBudgets {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		b := cliBudgets[name]
		t.Run(name, func(t *testing.T) {
			args := slices.Clone(b.args)
			for i, a := range args {
				switch a {
				case "BRIEF":
					args[i] = brief
				case "CHECKOUT":
					args[i] = d.checkout
				}
			}
			var runs []sample
			for range cliRuns {
				s := d.run(t.Context(), args...)
				if !slices.Contains(b.exits, s.Exit) {
					t.Fatalf("exit %d (expected one of %v):\n%s", s.Exit, b.exits, s.Output)
				}
				runs = append(runs, s)
			}
			t.Logf("exit %d, output:\n%s", runs[0].Exit, runs[0].Output)
			cliCheck(t, name, runs, b)
		})
	}
}

func cliCheck(t *testing.T, name string, runs []sample, b cliBudget) {
	t.Helper()
	median := func(f func(sample) float64) float64 {
		v := make([]float64, 0, len(runs))
		for _, s := range runs {
			v = append(v, f(s))
		}
		slices.Sort(v)
		return v[len(v)/2]
	}
	most := func(f func(sample) float64) float64 {
		var m float64
		for _, s := range runs {
			m = max(m, f(s))
		}
		return m
	}
	rep.check(t, name, "wall", median(func(s sample) float64 { return s.Wall.Seconds() }), limit{Max: b.wall.Seconds()})
	rep.check(t, name, "cpu", median(func(s sample) float64 { return s.CPU.Seconds() }), limit{Max: b.cpu.Seconds()})
	rep.check(t, name, "read", most(func(s sample) float64 { return float64(s.Read) / mib }), limit{Max: b.readMiB})
	rep.check(t, name, "write", most(func(s sample) float64 { return float64(s.Write) / mib }), measured)
	rep.check(t, name, "rss", most(func(s sample) float64 { return float64(s.RSS) / mib }), limit{Max: b.rssMiB})
	rep.check(t, name, "children", most(func(s sample) float64 { return float64(s.Children) }), limit{Max: float64(b.children)})
}
