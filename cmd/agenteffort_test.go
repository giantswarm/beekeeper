package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	testEffortHigh = "high"
	testEffortLow  = "low"
	testEffortMax  = "max"
	testEffortX    = "xhigh"
	testScanRule   = "^scan"
	testScanName   = "scan deps"
	// testPlainModel is a configured choice's model without an effort.
	testPlainModel = "plain-model"
)

func TestEffortBackgroundLaunchAndRecord(t *testing.T) {
	for _, level := range []string{"", testEffortLow, "medium", testEffortHigh, testEffortX, testEffortMax} {
		t.Run("level="+level, func(t *testing.T) {
			a, _ := stubApp(t)
			plat.Machine = tableMachine{plat.Machine}
			useLauncher(t, &startRecorder{Launcher: plat.Launcher})
			a.cfg.Agents.Mode = config.ModeBackground
			calls := stubBackground(t, "backgrounded · "+bgShortID+" · "+bgName+"\n", bgSession{ID: bgShortID, SessionID: bgID, Name: bgName})
			by := state.Party{Session: "effort-starter"}
			_, err := a.startAgent(t.Context(), agentStart{name: bgName, brief: paramTask, dir: t.TempDir(), by: &by, effort: level})
			if err != nil {
				t.Fatal(err)
			}
			argv := (*calls)[0].argv
			i := slices.Index(argv, effortFlag)
			if level == "" {
				if i >= 0 {
					t.Fatalf("default overridden: %q", argv)
				}
			} else if i < 0 || argv[i+1] != level {
				t.Fatalf("effort missing: %q", argv)
			}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Starts) != 1 || st.Starts[0].Effort != level {
				t.Fatalf("starts: %+v", st.Starts)
			}
			events, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == verbStart })
			if err != nil || len(events) != 1 {
				t.Fatalf("events: %+v %v", events, err)
			}
			if level != "" && !strings.Contains(events[0].Detail, "effort "+level) {
				t.Fatalf("event: %+v", events[0])
			}
		})
	}
}

func TestEffortDryRunAndWake(t *testing.T) {
	for _, mode := range []string{config.ModeDesktop, config.ModeBackground} {
		dir := t.TempDir()
		var out bytes.Buffer
		a := &app{cfg: &config.Config{StateDir: dir, Agents: config.Agents{Dir: dir, Mode: mode}}, out: &out}
		if err := a.dryRunTurn(t.Context(), agentStart{name: "effort-worker", brief: paramTask, effort: testEffortHigh}); err != nil {
			t.Fatal(err)
		}
		if strings.Count(out.String(), "--effort high") != 1 {
			t.Fatalf("dry run: %s", out.String())
		}
		if mode == config.ModeDesktop && strings.Contains(out.String(), "seed turn:") {
			t.Fatalf("effort dropped on desktop: %s", out.String())
		}
	}
	argv := wakeArgv("claude", wakeTarget{id: "effort-worker", mode: state.ModeBypass, effort: testEffortLow}, "resume")
	i := slices.Index(argv, effortFlag)
	if i < 0 || argv[i+1] != testEffortLow || i > slices.Index(argv, "--") {
		t.Fatalf("wake: %q", argv)
	}
}

// TestEffortLoopPassesItsDefaultLast checks that the loop passes
// supervisor.effort to its starts as --default-effort, never as --effort: an
// explicit flag would mask the start's rule, choice and continued session.
// A start the loop gives an --effort of its own gets no default beside it.
func TestEffortLoopPassesItsDefaultLast(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Supervisor.Effort = testEffortHigh
	l := &loop.Loop{Host: r.a}
	if _, err := l.Run(t.Context(), agentsName, agentStartName, "effort-worker", "brief", "--harness", capacity.HarnessAuto); err != nil {
		t.Fatal(err)
	}
	call := r.called(agentsName, agentStartName)
	if i := slices.Index(call, "--default-effort"); slices.Contains(call, effortFlag) || i < 0 || call[i+1] != testEffortHigh {
		t.Fatalf("the loop's default is not the start's last: %q", call)
	}
	r.calls = nil
	if _, err := l.Run(t.Context(), agentsName, agentStartName, "effort-worker", "brief", effortFlag, testEffortLow); err != nil {
		t.Fatal(err)
	}
	call = r.called(agentsName, agentStartName)
	if slices.Contains(call, "--default-effort") || strings.Count(strings.Join(call, " "), effortFlag) != 1 || call[len(call)-1] != testEffortLow {
		t.Fatalf("explicit effort overridden: %q", call)
	}
}

func TestEffortRulePrecedence(t *testing.T) {
	a, _ := stubApp(t)
	a.cfg.Agents.Models = []config.AgentModelRule{
		{Match: testScanRule, Kind: capacity.TaskKindReview, Effort: testEffortX},
		{Match: testScanRule, Effort: testEffortHigh},
	}
	for _, tc := range []struct{ name, kind, flag, want string }{
		{testScanName, workerRole, "", testEffortHigh},
		{testScanName, capacity.TaskKindReview, "", testEffortX},
		{testScanName, workerRole, testEffortLow, testEffortLow},
		{testFixWord, workerRole, "", ""},
	} {
		got, err := a.startEffort(tc.name, tc.kind, tc.flag)
		if err != nil || got != tc.want {
			t.Errorf("%+v: got %q, %v", tc, got, err)
		}
	}
	if _, err := a.startEffort(testFixWord, workerRole, "extreme"); err == nil {
		t.Fatal("invalid flag accepted")
	}
	a.cfg.Guide.Effort = testEffortMax
	if got := a.sessionEffort(agentStart{name: "scan guide", role: guideName}); got != testEffortMax {
		t.Fatalf("guide effort: %q", got)
	}
	if got := a.sessionEffort(agentStart{name: testScanName, role: workerRole}); got != testEffortHigh {
		t.Fatalf("rule effort: %q", got)
	}
}

// TestEffortLoopDefaultYieldsToRuleAndChoice checks the order of a
// start's effort under the loop's --default-effort: a matching rule, then
// the choice's effort, then the default; agy gets none, and a start without
// the default keeps the harness default.
func TestEffortLoopDefaultYieldsToRuleAndChoice(t *testing.T) {
	a, _ := stubApp(t)
	start := a.agentStartCmd()
	if err := start.Flags().Set("default-effort", testEffortHigh); err != nil {
		t.Fatal(err)
	}
	a.cfg.Agents.Models = []config.AgentModelRule{{Match: testScanRule, Kind: capacity.TaskKindReview, Effort: testEffortLow}}
	a.cfg.Capacity.Choices = []config.Choice{{Harness: claudeComm, Model: bgModel, Effort: testEffortX}, {Harness: claudeComm, Model: testPlainModel}}
	for _, tc := range []struct{ name, kind, harness, model, choice, want string }{
		{"scan-review", capacity.TaskKindReview, claudeComm, testPlainModel, "", testEffortLow},
		{"scan-worker", workerRole, claudeComm, testPlainModel, "", testEffortHigh},
		{"auto pick", workerRole, claudeComm, bgModel, testEffortX, testEffortX},
		{"named choice", workerRole, claudeComm, bgModel, "", testEffortX},
		{"agy pick", workerRole, capacity.HarnessAgy, "gemini", "", ""},
	} {
		got, err := a.selectedEffort(tc.name, tc.kind, tc.harness, tc.model, "", tc.choice)
		if err != nil || got != tc.want {
			t.Errorf("%+v: got %q, %v", tc, got, err)
		}
	}
	if err := start.Flags().Set("default-effort", ""); err != nil {
		t.Fatal(err)
	}
	if got, err := a.selectedEffort("scan-worker", workerRole, claudeComm, testPlainModel, "", ""); err != nil || got != "" {
		t.Fatalf("a start without the loop's default took one: %q, %v", got, err)
	}
}

// TestEffortContinuationKeepsTheContinuedSession checks that a fresh
// continuation of a parked worker, and a start that resumes a roster agent
// under its name, keep the effort that worker was started with on its
// harness when no rule or choice names one, and that a choice's effort
// still wins.
func TestEffortContinuationKeepsTheContinuedSession(t *testing.T) {
	a, _ := stubApp(t)
	parked := state.Agent{Party: state.Party{Name: "app 1", Session: "parked-session"}, Task: "Continue example/app#1", Park: &state.Park{At: a.now.Add(-time.Hour), Continuing: true}}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, parked)
		st.Starts = append(st.Starts, state.Start{Party: parked.Party, Model: testPlainModel, Effort: testEffortMax, At: a.now.Add(-2 * time.Hour)})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	next := continuationName(parked)
	for _, tc := range []struct{ name, harness, choice, want string }{
		{next, claudeComm, "", testEffortMax},
		{parked.Name, claudeComm, "", testEffortMax},
		{next, codex.Harness, "", ""},
		{next, claudeComm, testEffortHigh, testEffortHigh},
		{"app 2", claudeComm, "", ""},
	} {
		got, err := a.selectedEffort(tc.name, workerRole, tc.harness, testPlainModel, "", tc.choice)
		if err != nil || got != tc.want {
			t.Errorf("%+v: got %q, %v", tc, got, err)
		}
	}
}

func TestCapacityEffortAfterHarnessSelection(t *testing.T) {
	a, _ := stubApp(t)
	dir := t.TempDir()
	a.cfg.Codex.ConfigFile = filepath.Join(dir, "config.toml")
	if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte(`{"models":[{"slug":"test-model","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a.cfg.Agents.Models = []config.AgentModelRule{{Match: ".*", Harness: codex.Harness, Kind: capacity.TaskKindReview, Effort: testEffortLow}}
	for _, tc := range []struct {
		name, kind, harness, explicit, choice, want string
		invalid                                     bool
	}{
		{"worker choice", workerRole, codex.Harness, "", testEffortHigh, testEffortHigh, false},
		{"review rule", capacity.TaskKindReview, codex.Harness, "", testEffortHigh, testEffortLow, false},
		{"explicit override", capacity.TaskKindReview, codex.Harness, testEffortHigh, testEffortLow, testEffortHigh, false},
		{"agy after codex", capacity.TaskKindReview, capacity.HarnessAgy, "", "", "", false},
		{"unsupported model effort", workerRole, codex.Harness, "", testEffortMax, "", true},
		{"explicit agy refusal", workerRole, capacity.HarnessAgy, testEffortHigh, "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := a.selectedEffort("worker", tc.kind, tc.harness, "test-model", tc.explicit, tc.choice)
			if (err != nil) != tc.invalid || got != tc.want {
				t.Fatalf("got %q, %v; want %q, invalid=%v", got, err, tc.want, tc.invalid)
			}
		})
	}
}

func TestForcedChoiceRetainsEffort(t *testing.T) {
	v := &capacity.View{Choice: &capacity.ChoiceView{Candidates: []capacity.ChoiceCandidate{{Harness: codex.Harness, Model: "test-model", Effort: testEffortHigh}}}}
	ch, err := pickAutoChoice(v, true)
	if err != nil || ch.Effort != testEffortHigh {
		t.Fatalf("choice=%+v err=%v", ch, err)
	}
}
