package free

import (
	"slices"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

func TestOrphansDoNotMatchTaskText(t *testing.T) {
	const node, claude = "node", "claude"
	r, out := newRun(t, Options{Apply: true, Only: []string{Orphans}, Orphan: 30 * time.Minute})
	for _, p := range []*proc.Process{
		{PID: 10, PPID: 2, Comm: claude, Args: []string{claude, "-p", "--session-id", liveSID, "--", "Investigate processChild.js and jest-worker"}},
		{PID: 11, PPID: 1, Comm: "python3", Args: []string{"python3", "-c", "print('claude --output-format stream-json')"}},
		{PID: 12, PPID: 1, Comm: node, Args: []string{node, "analysis.js", "--label", "jest-worker metrics"}},
		{PID: 20, PPID: 1, Comm: node, Args: []string{node, "/tools/jest-worker/build/workers/processChild.js"}},
		{PID: 21, PPID: 2, Comm: node, Args: []string{node, "--expose-gc", "/tools/jest-worker/build/index.js"}},
		{PID: 22, PPID: 1, Comm: claude, Args: []string{"/tools/claude", "--output-format", "stream-json", "--resume=" + deadSID}},
		{PID: 23, PPID: 1, Comm: node, Args: []string{node, "/worker tools/jest-worker/build/workers/processChild.js"}},
	} {
		p.Start = now.Add(-time.Hour)
		r.Table.ByPID[p.PID] = p
	}
	r.Table.ByPID[2] = &proc.Process{PID: 2, Comm: "systemd"}
	var killed []int
	r.kill = func(pids []int) { killed = append(killed, pids...) }
	r.Free()
	if want := []int{20, 21, 22, 23}; !slices.Equal(killed, want) {
		t.Fatalf("selected PIDs %v, want only workers %v\n%s", killed, want, out)
	}
}
