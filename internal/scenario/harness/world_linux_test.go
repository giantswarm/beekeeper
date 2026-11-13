//go:build scenario && linux

package harness

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func testWorld(t *testing.T, opts Options) *World {
	t.Helper()
	if opts.Env == nil {
		opts.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
	}
	w, err := NewWorld(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// A session gets its worker's turns in order, an idle turn beyond them,
// and what it reports back is what a worker expectation reads.
func TestWorldHandsOutTurnsAndRecordsReports(t *testing.T) {
	w := testWorld(t, Options{Given: Given{Workers: []Worker{
		{Name: "app-.*", Harness: Claude, Turns: []Turn{
			{Steps: []Step{{Git: []string{"status"}}}, Tokens: &Tokens{Input: 7, Output: 3}, Reply: "pushed"},
			{Outcome: Crash},
		}},
		{Name: "looper", Repeat: true, Turns: []Turn{{Outcome: Budget, Usage: &Usage{Week: 60}}}},
	}}})
	s := &sessionID{Harness: Claude, Name: "app-worker", ID: "s1"}
	turn := func(s *sessionID) *turnPlan {
		t.Helper()
		rep := w.handle(request{Op: opTurn, Session: s, Dir: "/work/repo", PID: os.Getpid()})
		if rep.Error != "" || rep.Turn == nil {
			t.Fatalf("turn: %+v", rep)
		}
		return rep.Turn
	}
	first := turn(s)
	if first.Index != 0 || first.Outcome != Finish || first.Reply != "pushed" || first.Tokens.Input != 7 || len(first.Steps) != 1 || first.Vars["name"] != "app-worker" || first.Vars["host"] != LocalHost || first.Vars["dir"] != "/work/repo" {
		t.Fatalf("first turn: %+v", first)
	}
	if second := turn(s); second.Index != 1 || second.Outcome != Crash || second.Exit != 1 {
		t.Fatalf("second turn: %+v", second)
	}
	if third := turn(s); third.Index != 2 || third.Outcome != Finish || len(third.Steps) != 0 {
		t.Fatalf("a turn beyond the script: %+v", third)
	}
	// Another harness under the same name has no script: it ends idle.
	if other := turn(&sessionID{Harness: Codex, Name: "app-worker", ID: "t1"}); other.Index != 0 || other.Outcome != Finish || len(other.Steps) != 0 {
		t.Fatalf("an unscripted worker: %+v", other)
	}
	w.handle(request{Op: opTurnDone, Session: s, Report: &report{Index: 0, Outcome: Finish}})
	w.handle(request{Op: opTurnDone, Session: s, Report: &report{Index: 1, Outcome: Crash, Failure: "step 0 `git push` exited 1, want 0"}})
	if f := w.Failures(); len(f) != 1 || !strings.Contains(f[0], `worker "app-worker" (claude on local) turn 1: step 0`) {
		t.Fatalf("failures: %v", f)
	}
	three, yes, no := 3, true, false
	type workerCase struct {
		e    WorkerExpect
		want string
	}
	check := func(cases []workerCase) {
		t.Helper()
		for _, tc := range cases {
			err := w.CheckWorker(tc.e)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Errorf("CheckWorker(%+v) = %v, want %q", tc.e, err, tc.want)
			}
		}
	}
	// The third turn is handed out and has not reported: the second's
	// crash is not its outcome.
	check([]workerCase{
		{WorkerExpect{Name: "app-worker", Harness: Claude, Turns: &three, Running: &yes}, ""},
		{WorkerExpect{Name: "app-worker", Harness: Claude, Turns: &three, Outcome: Crash}, `its last turn has not ended yet, want it ended "crash"`},
		{WorkerExpect{Name: "app-.*", Harness: Claude, Outcome: Finish}, "has not ended yet"},
		{WorkerExpect{Name: "app-worker", Harness: Claude, Running: &no}, "running true, want false"},
		{WorkerExpect{Name: "nobody", Turns: &three}, "0 turn(s), want 3"},
		{WorkerExpect{Name: "app-worker", Host: "builder", Turns: &three}, "0 turn(s), want 3"},
	})
	w.handle(request{Op: opTurnDone, Session: s, Report: &report{Index: 2, Outcome: Finish}})
	check([]workerCase{
		{WorkerExpect{Name: "app-worker", Harness: Claude, Turns: &three, Outcome: Finish, Running: &yes}, ""},
		{WorkerExpect{Name: "app-.*", Harness: Claude, Outcome: Crash}, `ended "finish", want "crash"`},
	})
	// A budget turn fills the provider's session window from its end on.
	l := &sessionID{Harness: Claude, Name: "looper", ID: "s2"}
	for i := range 2 {
		if p := turn(l); p.Index != i || p.Outcome != Budget {
			t.Fatalf("repeated turn %d: %+v", i, p)
		}
	}
	if rep := w.handle(request{Op: opUsage, Harness: Claude}); rep.Usage == nil || rep.Usage.Session != 0 || rep.Full {
		t.Fatalf("usage before the budget turn ended: %+v", rep)
	}
	w.handle(request{Op: opTurnDone, Session: l, Report: &report{Index: 0, Outcome: Budget}})
	if rep := w.handle(request{Op: opUsage, Harness: Claude}); rep.Usage == nil || rep.Usage.Session != 100 || rep.Usage.Week != 60 || !rep.Full {
		t.Fatalf("usage after the budget turn: %+v", rep)
	}
}

// A failing stretch of codex's usage probe refuses the requests after the
// first answered ones, then the account answers again; claude's probe is
// not counted with it.
func TestWorldFailsAStretchOfUsageRequests(t *testing.T) {
	fail := &UsageFail{After: 1, Times: 2, Message: "GET x failed: 401 Unauthorized; content-type=; body="}
	w := testWorld(t, Options{Given: Given{Usage: map[string]Usage{Codex: {Week: 23, Fail: fail}, Claude: {Week: 40}}}})
	for i, want := range []bool{false, true, true, false, false} {
		w.handle(request{Op: opUsage, Harness: Claude})
		rep := w.handle(request{Op: opUsage, Harness: Codex})
		if rep.Usage == nil || rep.Usage.Week != 23 || (rep.Fail != nil) != want || want && rep.Fail.Message != fail.Message {
			t.Fatalf("request %d: %+v, want failing %t", i, rep, want)
		}
	}
	if rep := w.handle(request{Op: opUsage, Harness: Claude}); rep.Fail != nil {
		t.Fatalf("claude's probe failed: %+v", rep)
	}
}

// The world's answer to a call is the scenario's given call when one
// answers, and the fake's own model otherwise.
func TestWorldLeavesAModeledCallToItsFake(t *testing.T) {
	var asked []string
	w := testWorld(t, Options{Answer: func(program string, args []string, _, _ string, modeled bool) (string, string, int, bool) {
		asked = append(asked, program+" "+strings.Join(args, " "))
		switch {
		case program == Claude && slices.Contains(args, "/usage"):
			return "given usage\n", "", 0, true
		case modeled:
			return "", "", 0, false
		}
		return "", "no given call answers\n", 1, false
	}})
	if rep := w.handle(request{Op: opCall, Program: Claude, Args: []string{"-p", "/usage"}, Modeled: true}); rep.Given == nil || rep.Given.Stdout != "given usage\n" {
		t.Fatalf("a given call does not win over the model: %+v", rep)
	}
	if rep := w.handle(request{Op: opCall, Program: Claude, Args: []string{"agents", "--json"}, Modeled: true, Dir: "/work/hosts/builder/home"}); rep.Given != nil || rep.Machine != "builder" {
		t.Fatalf("a modeled call: %+v", rep)
	}
	if rep := w.handle(request{Op: opCall, Program: Claude, Args: []string{"mcp", "list"}}); rep.Given == nil || rep.Given.Exit != 1 {
		t.Fatalf("a call nothing answers: %+v", rep)
	}
	if len(asked) != 3 {
		t.Fatalf("calls the scenario's world saw: %v", asked)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The service manager runs a unit as systemd-run asks, answers systemctl
// about it and ends it as a stop does.
func TestUnitsRunStopAndFail(t *testing.T) {
	dir := t.TempDir()
	w := testWorld(t, Options{Artifacts: filepath.Join(dir, "artifacts"), Env: []string{"PATH=/usr/bin:/bin", "HOME=" + dir}})
	start := func(spec unitSpec) reply {
		return w.handle(request{Op: opUnitStart, Unit: &spec, Dir: dir})
	}
	state := func(name string) string { return w.handle(request{Op: opUnitState, Names: []string{name}}).State }
	list := func(states []string, patterns ...string) []string {
		var names []string
		for _, l := range w.handle(request{Op: opUnitList, States: states, Names: patterns}).Lines {
			names = append(names, strings.Fields(l)[0])
		}
		return names
	}
	marker := filepath.Join(dir, "unit-env")
	if rep := start(unitSpec{Name: "beekeeper-codex-1234abcd", Collect: true, KillMode: "mixed", Dir: dir, Env: []string{"WORKER=one"},
		Argv: []string{"sh", "-c", `printf '%s %s' "$WORKER" "$BEEKEEPER_SCENARIO_UNIT" > "$0"; exec sleep 60`, marker}}); rep.Exit != 0 {
		t.Fatalf("start: %+v", rep)
	}
	if got := state("beekeeper-codex-1234abcd"); got != unitActive {
		t.Fatalf("state %q, want active", got)
	}
	// The unit's processes carry its environment and its name.
	waitFor(t, "the unit's command to run", func() bool { b, _ := os.ReadFile(marker); return len(b) > 0 })
	if b, _ := os.ReadFile(marker); string(b) != "one beekeeper-codex-1234abcd.service" {
		t.Fatalf("the unit's environment: %q", b)
	}
	if got := list([]string{unitActive, "activating"}, "beekeeper-codex-*", "beekeeper-agy-*"); !slices.Equal(got, []string{"beekeeper-codex-1234abcd.service"}) {
		t.Fatalf("list-units: %v", got)
	}
	if got := list([]string{unitFailed}); len(got) != 0 {
		t.Fatalf("failed units: %v", got)
	}
	if rep := start(unitSpec{Name: "beekeeper-codex-1234abcd", Argv: []string{"true"}}); rep.Exit == 0 || !strings.Contains(rep.Message, "already loaded") {
		t.Fatalf("a second start under the name: %+v", rep)
	}
	if rep := w.handle(request{Op: opUnitVerb, Verb: "stop", Names: []string{"beekeeper-codex-1234abcd"}}); rep.Exit != 0 {
		t.Fatalf("stop: %+v", rep)
	}
	// A collected unit is unloaded once it ended: systemctl shows it inactive.
	if got := state("beekeeper-codex-1234abcd"); got != unitInactive {
		t.Fatalf("state after the stop %q, want inactive", got)
	}
	if got := list(nil); len(got) != 0 {
		t.Fatalf("units after the stop: %v", got)
	}
	// A unit whose command fails stays failed until reset-failed, unless
	// SuccessExitStatus names its exit status.
	if rep := start(unitSpec{Name: "broken", Argv: []string{"sh", "-c", "exit 3"}}); rep.Exit != 0 {
		t.Fatalf("start: %+v", rep)
	}
	waitFor(t, "the failed unit", func() bool { return state("broken") == unitFailed })
	if got := list([]string{unitFailed}, "bro*"); !slices.Equal(got, []string{"broken.service"}) {
		t.Fatalf("failed units: %v", got)
	}
	w.handle(request{Op: opUnitVerb, Verb: "reset-failed", Names: []string{"broken"}})
	if got := state("broken"); got != unitInactive {
		t.Fatalf("state after reset-failed %q", got)
	}
	if rep := start(unitSpec{Name: "quiet", Success: []string{"3", "SIGKILL"}, Argv: []string{"sh", "-c", "exit 3"}}); rep.Exit != 0 {
		t.Fatalf("start: %+v", rep)
	}
	waitFor(t, "the quiet unit's end", func() bool { return state("quiet") == unitInactive })
	// A command the unit's PATH does not have, and a verb on a unit that
	// is not there.
	if rep := start(unitSpec{Name: "nothing", Argv: []string{"no-such-command"}}); rep.Exit == 0 || !strings.Contains(rep.Message, "not found") {
		t.Fatalf("a missing command: %+v", rep)
	}
	if rep := w.handle(request{Op: opUnitVerb, Verb: "freeze", Names: []string{"nothing"}}); rep.Exit != 5 {
		t.Fatalf("freeze of no unit: %+v", rep)
	}
	// Each machine has its own units.
	if got := w.handle(request{Op: opUnitState, Names: []string{"quiet"}, Home: "/work/hosts/builder/home"}).State; got != unitInactive {
		t.Fatalf("a host sees the desk's unit: %q", got)
	}
}

// An ssh destination leads to its host's root, with the host's fault, the
// coordinator's name leads back to the desk, and a host step changes the
// fault while the scenario runs.
func TestSSHRoutesAndFaults(t *testing.T) {
	w := testWorld(t, Options{
		Env:   []string{"PATH=/work/bin", "HOME=/work/home", "BEEKEEPER_CONFIG=/work/config.yaml", "LANG=C.UTF-8"},
		Hosts: []Host{{Name: "builder", Fault: Unreachable}, {Name: "old", Fault: OlderGo, Slow: Duration{300 * time.Millisecond}}},
	})
	r := w.handle(request{Op: opSSH, Dest: "builder"}).SSH
	if r == nil || r.Root != "/work/hosts/builder" || r.Home != "/work/hosts/builder/home" || r.Fault != Unreachable {
		t.Fatalf("route to builder: %+v", r)
	}
	if !slices.Contains(r.Env, "HOME=/work/hosts/builder/home") || !slices.Contains(r.Env, "PATH=/work/hosts/builder/bin") || !slices.Contains(r.Env, "LANG=C.UTF-8") ||
		slices.ContainsFunc(r.Env, func(kv string) bool { return strings.HasPrefix(kv, "BEEKEEPER_CONFIG=") }) {
		t.Fatalf("the host's environment: %v", r.Env)
	}
	// older-go is the host's provisioning, no fault of its connection.
	if r := w.handle(request{Op: opSSH, Dest: "scenario@old"}).SSH; r == nil || r.Fault != "" || r.Slow != 300 {
		t.Fatalf("route to old: %+v", r)
	}
	if r := w.handle(request{Op: opSSH, Dest: CoordinatorAlias}).SSH; r == nil || r.Home != "/work/home" || !slices.Contains(r.Env, "BEEKEEPER_CONFIG=/work/config.yaml") {
		t.Fatalf("route to the coordinator: %+v", r)
	}
	if rep := w.handle(request{Op: opSSH, Dest: "elsewhere"}); rep.SSH != nil || rep.Exit != 255 || !strings.Contains(rep.Message, "Could not resolve hostname elsewhere") {
		t.Fatalf("an unknown destination: %+v", rep)
	}
	if err := w.ApplyHostStep(HostStep{Name: "builder", Clear: true}); err != nil {
		t.Fatal(err)
	}
	if r := w.handle(request{Op: opSSH, Dest: "builder"}).SSH; r == nil || r.Fault != "" {
		t.Fatalf("route after clear: %+v", r)
	}
	if err := w.ApplyHostStep(HostStep{Name: "builder", Fault: HostKeyChanged, Slow: Duration{time.Second}}); err != nil {
		t.Fatal(err)
	}
	if r := w.handle(request{Op: opSSH, Dest: "builder"}).SSH; r == nil || r.Fault != HostKeyChanged || r.Slow != 1000 {
		t.Fatalf("route after the fault: %+v", r)
	}
	if err := w.ApplyHostStep(HostStep{Name: "elsewhere", Clear: true}); err == nil {
		t.Fatal("a step of an unknown host passed")
	}
}

// A worker's steps run as its tool commands: in its directory, with the
// names of earlier steps, and stop at the first step that ends otherwise
// than its script says.
func TestRunSteps(t *testing.T) {
	dir := t.TempDir()
	env := []string{"PATH=/usr/bin:/bin", "WORKER=app-worker"}
	plan := &turnPlan{Vars: map[string]string{"name": "app-worker", "dir": dir}, Steps: []Step{
		{Write: &WriteFile{Path: "sub/body.md", Content: "from ${name}\n"}},
		{Run: []string{"sh", "-c", `cat body.md >&2; printf 'https://github.com/example/app/pull/2\n'`}, Dir: "sub", Save: "pull"},
		{Run: []string{"sh", "-c", `printf '%s %s' "$1" "$WORKER" > seen; cat; exit 4`, "sh", "${pull}"}, Stdin: "in ${dir}", Exit: 4},
		{Run: []string{"sh", "-c", "echo to stderr >&2; exit 2"}},
		{Write: &WriteFile{Path: "never.txt", Content: "x"}},
	}}
	var seen []string
	results, failure := runSteps(plan, dir, env, func(line string, r stepResult) { seen = append(seen, line) })
	if len(results) != 4 || len(seen) != 3 {
		t.Fatalf("results %d, commands seen %d: %+v", len(results), len(seen), results)
	}
	if !strings.Contains(failure, "step 3") || !strings.Contains(failure, "exited 2, want 0") || !strings.Contains(failure, "to stderr") {
		t.Fatalf("failure: %q", failure)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "seen")); string(b) != "https://github.com/example/app/pull/2 app-worker" {
		t.Fatalf("the saved name and the environment in a later step: %q", b)
	}
	if results[1].Output != "https://github.com/example/app/pull/2\nfrom app-worker" {
		t.Fatalf("the step in its own directory: %+v", results[1])
	}
	if results[2].Exit != 4 || results[2].Output != "in "+dir {
		t.Fatalf("the step with an expected exit code: %+v", results[2])
	}
	if _, err := os.Stat(filepath.Join(dir, "never.txt")); err == nil {
		t.Fatal("a step after the failed one ran")
	}
	if _, failure := runSteps(&turnPlan{Steps: []Step{{Run: []string{"no-such-command"}}}}, dir, env, nil); !strings.Contains(failure, "not found") {
		t.Fatalf("a missing command: %q", failure)
	}
}

func TestOriginOf(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/example/app.git":   "/work/origins/example/app.git",
		"https://github.com/example/app":       "/work/origins/example/app.git",
		"https://github.com/example/app/":      "/work/origins/example/app.git",
		"https://github.com/example":           "",
		"https://github.com/a/b/c.git":         "",
		"https://github.com/../../etc/app.git": "",
		"https://gitlab.com/example/app.git":   "",
		"file:///work/origins/example/app.git": "",
	} {
		if got := originOf(url); got != want {
			t.Errorf("originOf(%q) = %q, want %q", url, got, want)
		}
	}
}

// What a fake answers itself and what it leaves to the scenario's given
// calls: an unknown subcommand is never silently answered.
func TestModeledCalls(t *testing.T) {
	for _, tc := range []struct {
		modeled func([]string) bool
		args    string
		want    bool
	}{
		{claudeModeled, "--bg --remote-control w -n w -- brief", true},
		{claudeModeled, "--bg --resume 1234 message", true},
		{claudeModeled, "agents --json --all", true},
		{claudeModeled, "stop 1234abcd", true},
		{claudeModeled, "-p /usage --output-format json", true},
		{claudeModeled, "plugin list --json", true},
		{claudeModeled, "auth status", true},
		{claudeModeled, "--restricted --settings s.json --version", true},
		{claudeModeled, "mcp list", false},
		{claudeModeled, "plugin install x", false},
		{claudeModeled, "", false},
		{codexModeled, "exec --json -m gpt -", true},
		{codexModeled, "exec resume --json 0199 -", true},
		{codexModeled, "app-server", true},
		{codexModeled, "login status", true},
		{codexModeled, "login", false},
		{codexModeled, "mcp list", false},
		{agyModeled, "models", true},
		{agyModeled, "--input-format stream-json --output-format stream-json --model m", true},
		{agyModeled, "-p /usage", false},
		{ompModeled, "models --json", true},
		{ompModeled, "--mode rpc --no-ui --model local/qwen", true},
		{ompModeled, "stats", false},
		{systemctlModeled, "--user is-system-running", true},
		{systemctlModeled, "--user show -p ActiveState --value x", true},
		{systemctlModeled, "--user list-units --all --plain --no-legend --state=failed x-*", true},
		{systemctlModeled, "--user enable beekeeper-notify.service", false},
		{systemctlModeled, "--user", false},
		{ghModeled, "--version", true},
		{ghModeled, "auth token", true},
		{ghModeled, "pr create", false},
	} {
		if got := tc.modeled(strings.Fields(tc.args)); got != tc.want {
			t.Errorf("%q modeled %t, want %t", tc.args, got, tc.want)
		}
	}
	for args, want := range map[string]string{
		"--bg -n w -- do this":  "do this",
		"-p --model m the task": "task",
		"--bg --resume id msg":  "msg",
	} {
		if got := lastPrompt(strings.Fields(args)); got != want {
			t.Errorf("lastPrompt(%q) = %q, want %q", args, got, want)
		}
	}
}
