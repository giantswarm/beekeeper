package free

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

const orphanChildScript = "processChild.js"

const orphanPython = "python3"

const orphanApp = "app.js"

func TestOrphansDoNotMatchTaskText(t *testing.T) {
	r, _ := newRun(t, Options{Apply: true, Only: []string{Orphans}, Orphan: 30 * time.Minute})
	r.Table.ByPID[2] = &proc.Process{PID: 2, Comm: "systemd"}
	for _, p := range []*proc.Process{
		{PID: 10, Args: []string{"claude", "-p", "Investigate jest-worker and processChild.js"}},
		{PID: 11, Args: []string{orphanPython, "-c", `print("claude --output-format stream-json")`}},
		{PID: 12, Args: []string{litNode, orphanApp, "--label", "jest-worker processChild.js"}},
		{PID: 20, Args: []string{"/usr/bin/node", "/app/node_modules/jest-worker/build/workers/processChild.js"}},
		{PID: 21, Args: []string{"node", "--expose-gc", orphanChildScript}},
		{PID: 22, Args: []string{"/usr/bin/claude", "--output-format", "stream-json"}},
	} {
		p.PPID = 2
		p.Start = now.Add(-time.Hour)
		r.Table.ByPID[p.PID] = p
	}
	var killed []int
	r.kill = func(pids []int) { killed = append(killed, pids...) }
	r.Free()
	if want := []int{20, 21, 22}; !slices.Equal(killed, want) {
		t.Fatalf("killed %v, want %v", killed, want)
	}
}

func TestOrphanWorkerArguments(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"empty", nil, false},
		{"worker basename", []string{"node", "jest-worker"}, true},
		{"runtime option", []string{"node", "--max-old-space-size=256", orphanChildScript}, true},
		{"option terminator", []string{"node", "--", orphanChildScript}, true},
		{"eval", []string{"node", "-e", orphanChildScript}, false},
		{"print", []string{"node", "--print", "jest-worker"}, false},
		{"option value", []string{"node", "--require", orphanChildScript, orphanApp}, false},
		{"script argument", []string{"node", orphanApp, orphanChildScript}, false},
		{"script suffix", []string{"node", "processChild.js.backup"}, false},
		{"directory", []string{"node", "/jest-worker/app.js"}, false},
		{"other executable", []string{orphanPython, orphanChildScript}, false},
		{"headless brief", []string{"claude", "-p", "claude --output-format stream-json"}, false},
		{"joined argument", []string{"claude --output-format stream-json"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := orphanWorker(&proc.Process{Args: tc.args}); got != tc.want {
				t.Errorf("orphanWorker(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestOrphansRetainGuards(t *testing.T) {
	for _, guard := range []string{"parent", "age", "uid", "dry run"} {
		t.Run(guard, func(t *testing.T) {
			r, _ := newRun(t, Options{Apply: true, Only: []string{Orphans}, Orphan: 30 * time.Minute})
			p := &proc.Process{PID: 20, PPID: 1, Args: []string{"node", orphanChildScript}, Start: now.Add(-time.Hour)}
			r.Table.ByPID[p.PID] = p
			switch guard {
			case "parent":
				p.PPID = 2
				r.Table.ByPID[2] = &proc.Process{PID: 2, Comm: "node"}
			case "age":
				p.Start = now.Add(-time.Minute)
			case "uid":
				r.uid = func(int) int { return os.Getuid() + 1 }
			case "dry run":
				r.Apply = false
			}
			r.Free() // newRun's kill callback fails if called.
		})
	}
}
