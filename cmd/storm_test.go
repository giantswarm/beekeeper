package cmd

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// A process started since the last read counts toward the storm; a Go
// program's pidfd probe (its own command line, no CPU) is its parent.
func TestStormLineNamesTheFreshCommandsAndSessions(t *testing.T) {
	prev := named(table(
		&proc.Process{PID: 10, Args: strings.Fields("claude"), StartTicks: 1},
		&proc.Process{PID: 11, PPID: 10, Args: strings.Fields("zsh -c loop"), StartTicks: 1},
	))
	cur := named(table(
		prev.ByPID[10], prev.ByPID[11],
		&proc.Process{PID: 20, PPID: 11, Args: strings.Fields("kubectl get pods"), StartTicks: 5, CPU: time.Millisecond},
		&proc.Process{PID: 21, PPID: 20, Args: strings.Fields("kubectl get pods"), StartTicks: 5},
		&proc.Process{PID: 22, PPID: 11, Args: strings.Fields("kubectl get ns"), StartTicks: 5, CPU: time.Millisecond},
		&proc.Process{PID: 23, PPID: 1, Args: strings.Fields("helm list"), StartTicks: 5, CPU: time.Millisecond},
	))
	started := fresh(prev, cur)
	if len(started) != 3 {
		t.Fatalf("fresh = %d processes, want 3 (the probe folded)", len(started))
	}
	got := stormLine(220, 100, started, cur, map[int]string{10: "Board pull 5"})
	want := `PROCESS STORM: 220 forks/s (usual 100/s), top: kubectl 66 %, helm 33 %; sessions: "Board pull 5" 66 %`
	if got != want {
		t.Errorf("stormLine =\n %q\nwant\n %q", got, want)
	}
	if got := stormLine(180, 100, nil, cur, nil); !strings.HasSuffix(got, "none of them still running at the sample") {
		t.Errorf("stormLine with none seen = %q", got)
	}
}

// Four copies of a sleeping command under one parent are one stack, named
// with the count, the oldest age and the parent; one MCP server under each
// of many CLIs, interactive shells and short-lived copies are none.
func TestStacks(t *testing.T) {
	now := time.Now()
	old := func(pid, ppid int, age time.Duration, cmdline string) *proc.Process {
		args := strings.Fields(cmdline)
		return &proc.Process{PID: pid, PPID: ppid, Comm: args[0], Args: args, Start: now.Add(-age)}
	}
	ps := []*proc.Process{
		old(1, 0, time.Hour, "systemd"),
		old(2, 1, time.Hour, "waybar"),
		old(100, 1, time.Hour, "claude --session-id a"),
		old(101, 1, time.Hour, "claude --session-id b"),
		old(102, 1, time.Hour, "claude --session-id c"),
		old(103, 1, time.Hour, "claude --session-id d"),
	}
	// one MCP server per CLI
	for i := range 4 {
		ps = append(ps, old(200+i, 100+i, time.Hour, "github-mcp-server stdio"))
	}
	// login shells reparented to systemd
	for i := range 5 {
		ps = append(ps, old(300+i, 1, time.Hour, "/bin/zsh -l"))
	}
	// seven copies of a script, each running the same command
	for i := range 7 {
		ps = append(ps,
			old(400+i, 2, time.Duration(i+2)*time.Minute, "sh /home/u/freemem.sh"),
			old(500+i, 400+i, time.Duration(i+2)*time.Minute, "beekeeper free --summary"))
	}
	// four fresh copies: a burst, no pile-up
	for i := range 4 {
		ps = append(ps, old(600+i, 2, 10*time.Second, "sleep 1"))
	}
	got := stacks(table(ps...), now, 3, map[int]string{100: "A"})
	if len(got) != 1 {
		t.Fatalf("stacks = %+v, want one", got)
	}
	want := "STACKED 7 × beekeeper free --summary: oldest 8m0s, parent sh /home/u/freemem.sh"
	if l := got[0].line(); l != want {
		t.Errorf("line =\n %q\nwant\n %q", l, want)
	}
	if strings.Contains(got[0].key, "beekeeper") {
		t.Errorf("key %q carries the command line", got[0].key)
	}

	// Four sleeping copies under one parent inside a session.
	ps = []*proc.Process{old(100, 1, time.Hour, "claude"), old(110, 100, 5*time.Minute, "sh -c work")}
	for i := range 4 {
		ps = append(ps, old(700+i, 110, time.Duration(2+i)*time.Minute, "sleep 300"))
	}
	got = stacks(table(ps...), now, 3, map[int]string{100: "Board pull 9"})
	if len(got) != 1 || got[0].line() != `STACKED 4 × sleep 300: oldest 5m0s, parent sh -c, session "Board pull 9"` {
		t.Errorf("stacks = %+v", got)
	}
	if stacks(table(ps...), now, 4, nil) != nil {
		t.Error("four copies make a stack at stackMax 4")
	}
}

// A printed command line keeps the program, its subcommands and flag names,
// never a value that may be a credential.
func TestDisplayLeavesValuesOut(t *testing.T) {
	for args, want := range map[string]string{ //nolint:gosec // made-up values the display must drop
		"/usr/bin/docker run --rm -i -e HOST=h -e PASSWORD=s3cret image": "docker run --rm -i -e",
		"mysql -u root -p hunter2":                                       "mysql -u -p",
		"/home/u/.go/bin/beekeeper free --summary":                       "beekeeper free --summary",
		"curl https://user:pw@example.com":                               "curl",
		"nice -n 5 make":                                                 "nice -n",
		"mysql -uroot -phunter2 db":                                      "mysql -u -p",
		"app --token=s3cret --api-key s3cret -timeout=5m":                "app --token --api-key -timeout",
	} {
		if got := display(strings.Fields(args)); got != want {
			t.Errorf("display(%q) = %q, want %q", args, got, want)
		}
		if got := display([]string{args}); got != want {
			t.Errorf("display of the rewritten argv %q = %q, want %q", args, got, want)
		}
	}
}

func TestInteractiveShell(t *testing.T) {
	for args, want := range map[string]bool{
		"/bin/zsh -l":         true,
		"-zsh":                true,
		"bash":                true,
		"sh freemem.sh":       false,
		"zsh -c sleep":        false,
		"sleep 300":           false,
		"/usr/bin/fish -i -l": true,
	} {
		p := &proc.Process{Args: strings.Fields(args)}
		if got := interactiveShell(p); got != want {
			t.Errorf("interactiveShell(%q) = %v", args, got)
		}
	}
}

// A loop forking 100 processes a second on a machine that forks 10 is one
// PROCESS STORM line from the second sample over the threshold on and one ENDED line once it stops;
// four sleeping copies under one parent are one STACKED line and one ENDED.
// A negative threshold turns its line off.
func TestSampleProcsSaysAStormAndAStackOnce(t *testing.T) {
	var out bytes.Buffer
	cfg := &config.Config{}
	cfg.Watch.Repeat.Duration = 10 * time.Minute
	cfg.Watch.ForkRateMax, cfg.Watch.StackMax = 50, 3
	var forks uint64 = 1000
	w := &watcher{app: &app{out: &out, cfg: cfg}, last: map[string]time.Time{}}
	w.readForks = func() (uint64, error) { return forks, nil }

	now := time.Now()
	sleeping := table(&proc.Process{PID: 1, Args: strings.Fields("systemd"), Start: now.Add(-time.Hour)})
	for i := range 4 {
		sleeping.ByPID[10+i] = &proc.Process{PID: 10 + i, PPID: 1, Args: strings.Fields("sleep 600"), Start: now.Add(-2 * time.Minute)}
	}
	named(sleeping)
	empty := table()
	step := func(rate uint64, t *proc.Table) {
		forks += rate * 30
		w.sampleProcs(now, 30*time.Second, empty, t)
	}
	step(0, empty) // the first reading: no rate yet
	for range 3 {
		step(10, empty) // the machine's usual rate
	}
	for range 5 {
		step(100, sleeping)
	}
	step(1, empty)
	step(1, empty)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var storm, stacked, ended int
	for _, l := range lines {
		switch {
		case strings.Contains(l, "PROCESS STORM: 100 forks/s (usual 10/s)"):
			storm++
		case strings.Contains(l, "STACKED 4 × sleep 600: oldest 2m0s, parent systemd"):
			stacked++
		case strings.Contains(l, "ENDED PROCESS STORM"), strings.Contains(l, "ENDED STACKED 4 × sleep 600"):
			ended++
		}
	}
	if storm != 1 || stacked != 1 || ended != 2 || len(lines) != 4 {
		t.Errorf("the watch said:\n%s", out.String())
	}

	out.Reset()
	cfg.Watch.ForkRateMax, cfg.Watch.StackMax = -1, -1
	for range 3 {
		step(100, sleeping)
	}
	if out.Len() != 0 {
		t.Errorf("turned off, the watch said:\n%s", out.String())
	}
}

// 1,200 short-lived tsh and helm processes of two sessions, seen by
// three samples in a row, are one LOAD line naming the commands and the
// sessions, and one ENDED line once they are gone; a negative
// watch.toolProcsMax turns it off.
func TestSampleProcsSaysLoadOnce(t *testing.T) {
	var out bytes.Buffer
	cfg := &config.Config{}
	cfg.Watch.Repeat.Duration = 10 * time.Minute
	cfg.Watch.ForkRateMax, cfg.Watch.StackMax, cfg.Watch.ToolProcsMax = -1, -1, 1000
	cfg.Watch.Tools = []string{"tsh", "helm"}
	w := &watcher{app: &app{out: &out, cfg: cfg}, last: map[string]time.Time{}}
	w.readForks = func() (uint64, error) { return 0, nil }
	owners := map[int]string{1: agentOne, 2: agentFour}
	w.owners.Store(&owners)

	now := time.Now()
	busy := table(&proc.Process{PID: 1, Args: strings.Fields("claude")}, &proc.Process{PID: 2, Args: strings.Fields("claude")},
		&proc.Process{PID: 3, Args: strings.Fields("sleep 60")})
	for i := range 1200 {
		cli, parent := "tsh kube login", 1
		if i%4 == 0 {
			cli, parent = "helm list", 2
		}
		busy.ByPID[100+i] = &proc.Process{PID: 100 + i, PPID: parent, Args: strings.Fields(cli), Start: now}
	}
	named(busy)
	for range 3 {
		w.sampleProcs(now, 30*time.Second, nil, busy)
	}
	w.sampleProcs(now, 30*time.Second, nil, table())
	want := "LOAD: 1200 CLI processes over 1000, top: tsh 75 %, helm 25 %; sessions: \"Agent one\" 75 %, \"Agent four\" 25 %\n"
	if got := out.String(); !strings.Contains(got, want) || strings.Count(got, "\n") != 2 || !strings.Contains(got, "ENDED LOAD (since ") {
		t.Errorf("the watch said:\n%s", got)
	}

	out.Reset()
	cfg.Watch.ToolProcsMax = -1
	w.sampleProcs(now, 30*time.Second, nil, busy)
	if out.Len() != 0 {
		t.Errorf("turned off, the watch said:\n%s", out.String())
	}
}

// named gives each process its command name, the base of its program.
func named(t *proc.Table) *proc.Table {
	for _, p := range t.ByPID {
		if p.Comm == "" && len(p.Args) > 0 {
			p.Comm = filepath.Base(p.Args[0])
		}
	}
	return t
}
