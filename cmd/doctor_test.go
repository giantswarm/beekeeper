package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/install"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// The doctor takes a finished worker off the roster once its CLI is idle,
// a desktop CLI kept warm included, a relieved role holder at once and an idle worker without a CLI once it
// went stale, archiving the desktop sessions of the finished and the stale
// ones; it retitles a started session the desktop dropped the title of,
// and leaves busy agents, role holders and sessions a person started alone.
func TestPlanChores(t *testing.T) {
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	agent := func(id string, idle time.Duration) state.Agent {
		return state.Agent{Party: state.Party{Session: id, Name: "test: " + id}, IdleSince: now.Add(-idle)}
	}
	done, doneBusy := agent("done", time.Minute), agent("done-busy", time.Minute)
	done.Done, doneBusy.Done = true, true
	task, doneTask := agent("task", 48*time.Hour), agent("done-task", time.Minute)
	task.Task, doneTask.Task, doneTask.Done = "a task", "a new task", true
	st := &state.State{
		Agents: []state.Agent{done, doneBusy, agent("relieved", 0), agent("stale", 48*time.Hour), agent("stale-live", 48*time.Hour),
			agent("fresh", time.Hour), task, agent("holder", 48*time.Hour), agent("untitled", 0), agent("titled", 0), agent("own", 0), agent("archived", 0), doneTask},
		Guide: &state.Role{Holder: &state.Supervisor{Party: state.Party{Session: "holder"}}, Relieved: []state.Relief{{Party: state.Party{Session: "relieved"}}}},
	}
	for _, id := range []string{"untitled", "titled", "archived", "holder"} {
		st.Starts = append(st.Starts, state.Start{Party: state.Party{Session: id, HostSession: "local_" + id}})
	}
	// done's CLI still runs, idle: the desktop keeps the session warm.
	sessions := []*claude.Session{{ID: "done"}, {ID: "done-busy"}, {ID: "done-task"}, {ID: "stale-live"}, {ID: "untitled"}, {ID: "titled"}, {ID: "own"}}
	record := func(host string) (*claude.Record, bool) {
		switch host {
		case "local_untitled":
			return &claude.Record{Title: "klaus-lab-3d"}, true
		case "local_titled":
			return &claude.Record{Title: "test: titled"}, true
		case "local_archived":
			return &claude.Record{IsArchived: true}, true
		}
		return &claude.Record{}, true
	}
	busy := func(p state.Party) bool { return p.Session == "done-busy" }
	var got []string
	for _, c := range planChores(st, sessions, record, busy, 24*time.Hour, now) {
		got = append(got, c.String())
	}
	want := []string{
		`take "test: done" off the roster and archive its desktop session (it reported its work done)`,
		`take "test: relieved" off the roster (it was relieved of its role, which relays instead)`,
		`take "test: stale" off the roster and archive its desktop session (idle 2d, its CLI no longer runs)`,
		`retitle local_untitled "test: untitled" (the desktop recorded "klaus-lab-3d" instead of "test: untitled")`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("chores:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if c := planChores(st, sessions, record, busy, 0, now); slices.ContainsFunc(c, func(c chore) bool { return strings.Contains(c.why, "idle") }) {
		t.Errorf("staleAfter 0 takes no stale agent off: %v", c)
	}
}

// A configured fault is probed; a failing one whose remedy may run
// unattended is remedied and probed again, one that may not is only
// remedied when named; a remedy that does not fix it says so.
func TestCheckFaults(t *testing.T) {
	const stuck, unit = "stuck", "unit"
	broken := map[string]bool{unit: true, "manual": true, stuck: true}
	var ran []string
	run := func(_ context.Context, command string, _ time.Duration) error {
		name, verb, _ := strings.Cut(command, " ")
		if verb == "fix" {
			ran = append(ran, name)
			if name != stuck {
				broken[name] = false
			}
			return nil
		}
		if broken[name] {
			return errors.New("exit status 1")
		}
		return nil
	}
	faults := []config.Fault{
		{Name: "ok", Probe: "ok probe", Remedy: "ok fix", Unattended: true},
		{Name: unit, Probe: unit + " probe", Remedy: unit + " fix", Unattended: true},
		{Name: "manual", Probe: "manual probe", Remedy: "manual fix"},
		{Name: stuck, Probe: stuck + " probe", Remedy: stuck + " fix", Unattended: true},
	}
	fix := func(f config.Fault) bool { return f.Unattended }
	got := checkFaults(context.Background(), faults, fix, run)
	if !slices.Equal(ran, []string{unit, stuck}) {
		t.Errorf("remedies ran: %v, want unit and stuck", ran)
	}
	for i, want := range []string{"absent", `fixed fault "unit"`, "runs attended only: beekeeper doctor --fault manual", "also after its remedy"} {
		if !strings.Contains(got[i].String(), want) {
			t.Errorf("fault %s: %q, want %q", faults[i].Name, got[i], want)
		}
	}
	got = checkFaults(context.Background(), faults[2:3], func(config.Fault) bool { return true }, run)
	if !got[0].fixed {
		t.Errorf("--fault manual: %q", got[0])
	}
}

// A fault the doctor could not fix is one note for the person however
// often it is seen, and the note closes once the fault is absent.
func TestNoteFaults(t *testing.T) {
	st := &state.State{}
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	failing := []faultFinding{{fault: config.Fault{Name: "unit", Probe: "p", Remedy: "r"}}}
	for range 3 {
		noteFaults(st, failing, notePerson, now)
	}
	if len(st.Notes) != 1 || st.Notes[0].For != notePerson || !strings.Contains(st.Notes[0].Text, "beekeeper doctor --fault unit") {
		t.Fatalf("notes after three sightings: %+v", st.Notes)
	}
	failing[0].healthy = true
	if lines, _ := noteFaults(st, failing, notePerson, now); len(st.Notes) != 0 || len(lines) != 1 {
		t.Errorf("absent again: notes %+v, lines %v", st.Notes, lines)
	}
}

// One steward's turn archives a batch, its own session last as "self",
// under the person's agreement and after agents archivable confirmed it.
func TestArchiveRequest(t *testing.T) {
	msg := archiveRequest(steward{host: "local_y"}, []string{"local_x", "local_y", "local_z"}, "beekeeper doctor", "the standing directive in ~/.claude/CLAUDE.md")
	for _, want := range []string{`"local_x", "local_z", "self"`, "archive_session", "the standing directive in ~/.claude/CLAUDE.md", "beekeeper agents archivable local_x local_y local_z", "never for a session they started"} {
		if !strings.Contains(msg, want) {
			t.Errorf("request lacks %q: %q", want, msg)
		}
	}
}

// agents idle --done marks the work finished and clears the agent's own
// session record, no other; a new assignment clears done.
func TestIdleDone(t *testing.T) {
	a, out := noteApp(t)
	run := func(args ...string) {
		t.Helper()
		c := a.agentsCmd()
		c.SetArgs(args)
		c.SetOut(out)
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: state.Party{Name: agentOne}}}
		st.Records = []state.Record{
			{Session: state.Party{Name: agentOne}, Issue: "giantswarm/beekeeper#772"},
			{Session: state.Party{Name: unrosteredName}, Issue: "giantswarm/beekeeper#1"},
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	run("assign", agentOne, "a task")
	run(verbIdle, flagDone, flagReport, "merged "+prURL, flagProblem, noProblems)
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Agents) != 1 || !st.Agents[0].Done || st.Agents[0].Task != "" {
		t.Fatalf("after idle --done: %+v", st.Agents)
	}
	if len(st.Records) != 1 || st.Records[0].Session.Name != unrosteredName {
		t.Fatalf("records after idle --done: %+v", st.Records)
	}
	run("assign", agentOne, "another task")
	if st, _ = a.store.Read(); st.Agents[0].Done {
		t.Errorf("an assignment keeps done: %+v", st.Agents[0])
	}
}

// The flags of agents idle the tests pass.
const (
	verbIdle    = "idle"
	flagDone    = "--done"
	flagReport  = "--report"
	flagProblem = "--problem"
)

// agents idle --done requires the report's "Problems found", and each
// finding reaches the supervisor's watch once.
func TestIdleDoneProblemsFound(t *testing.T) {
	const task, merged = "the task", "merged " + prURL
	a, out := noteApp(t)
	run := func(args ...string) error {
		t.Helper()
		c := a.agentsCmd()
		c.SetArgs(args)
		c.SetOut(out)
		c.SetErr(out)
		c.SilenceUsage, c.SilenceErrors = true, true
		return c.Execute()
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: state.Party{Name: agentOne}, Task: task}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{verbIdle, flagDone},
		{verbIdle, flagDone, flagReport, merged},
		{verbIdle, flagDone, flagProblem, noProblems},
		{verbIdle, flagDone, flagReport, merged, flagProblem, " "},
		{verbIdle, flagDone, flagReport, merged, flagProblem, noProblems, flagProblem, "the reconciler creates no repository"},
	} {
		if err := run(args...); err == nil {
			t.Errorf("%v passed", args)
		}
	}
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"bare #N in report":       {[]string{verbIdle, flagDone, flagReport, "merged #7", flagProblem, noProblems}, "#7 without its full URL (https://github.com/<owner>/<repo>/issues/7)"},
		"owner/repo#N in report":  {[]string{verbIdle, flagDone, flagReport, "merged x/y#7", flagProblem, noProblems}, "x/y#7 without its full URL (https://github.com/x/y/issues/7)"},
		"bare #N in problem":      {[]string{verbIdle, flagDone, flagReport, merged, flagProblem, "blocked by #9"}, "#9 without its full URL (https://github.com/o/r/issues/9)"},
		"owner/repo#N in problem": {[]string{verbIdle, flagDone, flagReport, merged, flagProblem, "see x/y#9"}, "x/y#9 without its full URL (https://github.com/x/y/issues/9)"},
	} {
		if err := run(c.args...); Code(err) != ExitRefused || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want refusal with %q", name, err, c.want)
		}
	}
	if st, _ := a.store.Read(); st.Agents[0].Task != task || st.Agents[0].Done {
		t.Fatalf("a refused report changed the agent: %+v", st.Agents[0])
	}
	found := []string{
		"giantswarm/github: the reconciler creates no repository; worked around with gh",
		"follow-up: docs (note #3, timer #4, `#5`, `multi\n#6`, color: #123; background-color: #123456;, " + prURL + "#issuecomment-1)\n````md\n```\n#8\n```\n````",
	}
	reportText := merged + ", released v1.2.3"
	if err := run(verbIdle, flagDone, flagReport, reportText, flagProblem, found[0], flagProblem, found[1]); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if rs := st.WorkerReports; len(rs) != 1 || rs[0].Text != reportText || len(rs[0].Problems) != 2 || rs[0].Problems[0] != found[0] || rs[0].Task != task || rs[0].By.Name != agentOne {
		t.Fatalf("reports = %+v", st.WorkerReports)
	}
	lines, _ := firePending(st, nil, time.Now())
	if len(lines) != 3 || !strings.HasPrefix(lines[0], `WORKER REPORT by "`+agentOne+`" (task: the task): `+merged) ||
		!strings.HasPrefix(lines[1], `PROBLEM FOUND by "`+agentOne+`": `+found[0]) {
		t.Errorf("lines = %q", lines)
	}
	if lines, _ := firePending(st, nil, time.Now()); len(lines) != 0 || len(st.WorkerReports) != 0 {
		t.Errorf("printed again: %q", lines)
	}
}

// A park on a person reaches the supervisor's watch once; a park on a
// merge ahead in the lane, a timer or the supervisor's lease grant does not,
// and a park on a lease is said once as a lease wait with its queue place.
func TestParkedOnPersonIsSaidOnce(t *testing.T) {
	now := time.Now()
	lab := labOne
	queued := state.Party{Name: "on a lease", Session: "s2"}
	st := &state.State{
		Grants: []state.Grant{{Resource: lab, To: state.Party{Name: "first", Session: "s1"}}, {Resource: lab, To: queued}},
		Agents: []state.Agent{
			{Party: state.Party{Name: "on a note"}, Task: "t", Park: &state.Park{On: "#12", Waits: "the rollout window"}},
			{Party: state.Party{Name: "on Timo"}, Task: "t", Park: &state.Park{Waits: "Timo's go"}},
			{Party: state.Party{Name: "on a reply"}, Task: "t", Park: &state.Park{Waits: "a colleague's reply on the timer setting"}},
			{Party: state.Party{Name: "on a merge"}, Task: "t", Park: &state.Park{On: "giantswarm/x#3", Waits: "the merge ahead of mine in the lane"}},
			{Party: queued, Task: "t", Park: &state.Park{On: leaseParkOn(lab), Waits: "the lease " + lab + ": the supervisor's review proof"}},
			{Party: state.Party{Name: "on a grant"}, Task: "t", Park: &state.Park{Waits: "the v1.2.0 release and the supervisor's agentlab-2 grant"}},
			{Party: state.Party{Name: "on a probe"}, Task: "t", Park: &state.Park{Waits: "the rollout probe the supervisor set"}},
		},
	}
	lines, evs := parkedNews(st, "Timo", now)
	if len(lines) != 4 || len(evs) != 4 ||
		!strings.HasPrefix(lines[0], `PARKED ON A PERSON: "on a note" parked on #12`) ||
		!strings.HasPrefix(lines[1], `PARKED ON A PERSON: "on Timo"`) || !strings.Contains(lines[1], "tells Timo") ||
		!strings.HasPrefix(lines[2], `PARKED ON A PERSON: "on a reply"`) ||
		!strings.HasPrefix(lines[3], `PARKED ON A LEASE: "on a lease" parked on lease agentlab-1`) ||
		!strings.Contains(lines[3], `number 2 in the queue of agentlab-1 behind "first"`) {
		t.Fatalf("lines = %q", lines)
	}
	if lines, _ := parkedNews(st, "Timo", now); len(lines) != 0 {
		t.Errorf("said again: %q", lines)
	}
}

// Operational handoffs are not news for the person; explicit human waits are.
func TestParkOnPersonOperationalHandoffs(t *testing.T) {
	for name, tc := range map[string]struct {
		on, waits string
		want      bool
	}{
		"supervisor ready": {"", "rollout ready; waiting for the supervisor", false},
		"supervisor go":    {"", "awaiting the supervisor's go", false},
		"guide handoff":    {"", "the guide handoff ready", false},
		"gate":             {"", "the supervisor's gate", false},
		"install chain":    {"", "the supervisor's install chain", false},
		"local note":       {"#787", "install chain ready", true},
		"central note":     {"central#12", "gate ready", true},
		"named person":     {"", "operator's go on the install chain", true},
		"answer":           {"", "awaiting the person's answer", true},
		"colleague review": {"example/tool#12", "a colleague's review", true},
	} {
		t.Run(name, func(t *testing.T) {
			p := &state.Park{On: tc.on, Waits: tc.waits}
			if got := parkOnPerson(p, "operator"); got != tc.want {
				t.Errorf("parkOnPerson(%+v) = %v, want %v", p, got, tc.want)
			}
		})
	}
}

// The stale binaries are the other beekeeper processes whose file was
// replaced, by pid; the doctor leaves the watches to WATCH STALE.
func TestStaleBinaries(t *testing.T) {
	p := func(pid int, comm, cmdline string) *proc.Process {
		return &proc.Process{PID: pid, Comm: comm, Args: strings.Fields(cmdline)}
	}
	tab := &proc.Table{ByPID: map[int]*proc.Process{
		5: p(5, project.Name, "beekeeper agents reopen"),
		3: p(3, project.Name, "beekeeper gate -- devctl pr merge o/r 1"),
		4: p(4, project.Name, "beekeeper watch"),
		6: p(6, project.Name, "beekeeper snapshot"),
		7: p(7, merge.Tool, "devctl pr merge o/r 1"),
		8: p(8, project.Name, "beekeeper doctor"),
	}}
	replaced := func(pid int) bool { return pid != 6 }
	got := staleBinaries(tab, 8, replaced)
	if len(got) != 3 || got[0].PID != 3 || got[1].PID != 4 || got[2].PID != 5 {
		t.Fatalf("stale binaries = %v", got)
	}
	rest := slices.DeleteFunc(got, isWatch)
	if len(rest) != 2 || staleLine(rest[0]) != "stale binary: pid 3 (beekeeper gate --) runs a beekeeper an install replaced; "+
		"it re-executes the installed one at its next step" {
		t.Errorf("without the watches = %v: %q", rest, staleLine(rest[0]))
	}
	if len(rest) == 2 && staleLine(rest[1]) != "stale binary: pid 5 (beekeeper agents reopen) runs a beekeeper an install replaced; "+
		"its saves of the state are refused until it ends or is restarted" {
		t.Errorf("a reopen: %q", staleLine(rest[1]))
	}
}

// The doctor checks ~/.gemini/config/hooks.json when agy is installed,
// reporting when the file or beekeeper's PreToolUse entry is missing or
// points to another binary, and passing when the entry is in place alongside
// other named hooks.
func TestCheckAgyHooks(t *testing.T) {
	home := t.TempDir()
	exe := "/opt/bin/beekeeper"
	if got := checkAgyHooks(home, exe, false); got != "" {
		t.Fatalf("agy not installed: got %q, want empty", got)
	}
	if got := checkAgyHooks(home, exe, true); !strings.Contains(got, "~/.gemini/config/hooks.json is missing; run `beekeeper install`") {
		t.Fatalf("missing hooks.json: got %q", got)
	}
	var e install.Env
	e.Exe = exe
	e.Spec.Home = home
	e.Settings = home + "/.claude/settings.json"
	e.AgyHooks = agyHooksPath(home, true)
	e.Config = home + "/.config/beekeeper/config.yaml"
	e.StateDir = home + "/.local/state/beekeeper"
	e.Setup = platform.Stub().Setup
	e.Out = &bytes.Buffer{}
	if err := install.Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if got := checkAgyHooks(home, exe, true); got != "" {
		t.Errorf("installed: got %q, want empty", got)
	}
	if got := checkAgyHooks(home, "/other/beekeeper", true); !strings.Contains(got, "runs /opt/bin/beekeeper instead of /other/beekeeper") {
		t.Errorf("other binary: got %q", got)
	}
}

// The doctor checks ~/.gemini/config/skills.json and the skills directory it
// points at when agy is installed, reporting when the file, beekeeper's entry
// or a skill file is missing or stale, and passing when the entry and skills
// are in place alongside other entries.
func TestCheckAgySkills(t *testing.T) {
	home := t.TempDir()
	stateDir := filepath.Join(home, ".local", "state", "beekeeper")
	if got := checkAgySkills(home, stateDir, false); got != "" {
		t.Fatalf("agy not installed: got %q, want empty", got)
	}
	if got := checkAgySkills(home, stateDir, true); !strings.Contains(got, "~/.gemini/config/skills.json is missing; run `beekeeper install`") {
		t.Fatalf("missing skills.json: got %q", got)
	}
	var e install.Env
	e.Exe = "/opt/bin/beekeeper"
	e.Spec.Home = home
	e.Settings = home + "/.claude/settings.json"
	e.AgySkills = agySkillsPath(home, true)
	e.Config = home + "/.config/beekeeper/config.yaml"
	e.StateDir = stateDir
	e.Setup = platform.Stub().Setup
	e.Out = &bytes.Buffer{}
	if err := install.Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if got := checkAgySkills(home, stateDir, true); got != "" {
		t.Errorf("installed: got %q, want empty", got)
	}
	if got := checkAgySkills(home, filepath.Join(home, ".local", "state", "other"), true); !strings.Contains(got, "points to ~/.local/state/beekeeper/skills instead of ~/.local/state/other/skills") {
		t.Errorf("stale path: got %q", got)
	}
	if err := os.WriteFile(filepath.Join(install.AgySkillsDir(stateDir), "park", "SKILL.md"), []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := checkAgySkills(home, stateDir, true); !strings.Contains(got, "serves the skills park unlike beekeeper") {
		t.Errorf("stale skill file: got %q", got)
	}
}

func TestCheckCodexHooks(t *testing.T) {
	home := t.TempDir()
	stateDir := filepath.Join(home, ".local", "state", "beekeeper")
	exe := "/usr/local/bin/beekeeper"
	if got := checkCodexHooks(home, stateDir, exe, false); got != "" {
		t.Fatalf("codex not installed: got %q, want empty", got)
	}
	if got := checkCodexHooks(home, stateDir, exe, true); !strings.Contains(got, "~/.codex/hooks.json is missing; run `beekeeper install`") {
		t.Fatalf("missing hooks.json: got %q", got)
	}
	var e install.Env
	e.Exe = exe
	e.Spec.Home = home
	e.Settings = home + "/.claude/settings.json"
	e.CodexHooks = codexHooksPath(home, true)
	e.Config = home + "/.config/beekeeper/config.yaml"
	e.StateDir = stateDir
	e.Setup = platform.Stub().Setup
	e.Out = &bytes.Buffer{}
	if err := install.Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if got := checkCodexHooks(home, stateDir, exe, true); got != "" {
		t.Errorf("installed: got %q, want empty", got)
	}
	if got := checkCodexHooks(home, stateDir, "/other/beekeeper", true); !strings.Contains(got, "runs /usr/local/bin/beekeeper instead of /other/beekeeper") {
		t.Errorf("other binary: got %q", got)
	}
	// Missing worker hooks.json under <stateDir>/codex/home/hooks.json is caught too.
	if err := os.Remove(install.CodexWorkerHooks(stateDir)); err != nil {
		t.Fatal(err)
	}
	if got := checkCodexHooks(home, stateDir, exe, true); !strings.Contains(got, ".local/state/beekeeper/codex/home/hooks.json is missing; run `beekeeper install`") {
		t.Errorf("missing worker hooks.json: got %q", got)
	}
}

func TestCheckCodexSkills(t *testing.T) {
	home := t.TempDir()
	stateDir := filepath.Join(home, ".local", "state", "beekeeper")
	if got := checkCodexSkills(home, stateDir, false); got != "" {
		t.Fatalf("codex not installed: got %q, want empty", got)
	}
	if got := checkCodexSkills(home, stateDir, true); !strings.Contains(got, "~/.codex/skills is missing; run `beekeeper install`") {
		t.Fatalf("missing skills dir: got %q", got)
	}
	var e install.Env
	e.Exe = "/usr/local/bin/beekeeper"
	e.Spec.Home = home
	e.Settings = home + "/.claude/settings.json"
	e.CodexSkills = codexSkillsPath(home, true)
	e.Config = home + "/.config/beekeeper/config.yaml"
	e.StateDir = stateDir
	e.Setup = platform.Stub().Setup
	e.Out = &bytes.Buffer{}
	if err := install.Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if got := checkCodexSkills(home, stateDir, true); got != "" {
		t.Errorf("installed: got %q, want empty", got)
	}
	if err := os.WriteFile(filepath.Join(install.AgySkillsDir(stateDir), "park", "SKILL.md"), []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := checkCodexSkills(home, stateDir, true); !strings.Contains(got, "serves the skills park unlike beekeeper") {
		t.Errorf("stale skill file: got %q", got)
	}

	// Custom configured Codex.ConfigFile / Codex.AuthFile derives the user Codex directory.
	customCodexDir := filepath.Join(home, "custom-codex")
	customCfg := config.Codex{ConfigFile: filepath.Join(customCodexDir, "config.toml")}
	if got := codexHooksPath(home, true, customCfg); got != filepath.Join(customCodexDir, "hooks.json") {
		t.Errorf("codexHooksPath(customCfg) = %q", got)
	}
	if got := codexSkillsPath(home, true, customCfg); got != filepath.Join(customCodexDir, "skills") {
		t.Errorf("codexSkillsPath(customCfg) = %q", got)
	}
	if got := checkCodexHooks(home, stateDir, "/usr/local/bin/beekeeper", true, customCfg); !strings.Contains(got, "~/custom-codex/hooks.json is missing; run `beekeeper install`") {
		t.Errorf("checkCodexHooks(customCfg): got %q", got)
	}
	if got := checkCodexSkills(home, stateDir, true, customCfg); !strings.Contains(got, "~/custom-codex/skills is missing; run `beekeeper install`") {
		t.Errorf("checkCodexSkills(customCfg): got %q", got)
	}
}
