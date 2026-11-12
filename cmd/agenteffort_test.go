package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	testEffortHigh = "high"
	testEffortLow  = "low"
	testEffortMax  = "max"
	testEffortX    = "xhigh"
	testScanRule   = "^scan"
	testScanName   = "scan deps"
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

func TestEffortLoopStartDefault(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Supervisor.Effort = testEffortHigh
	l := &Loop{Host: r.a}
	if _, err := l.Run(t.Context(), agentsName, agentStartName, "effort-worker", "brief"); err != nil {
		t.Fatal(err)
	}
	call := r.called(agentsName, agentStartName)
	i := slices.Index(call, effortFlag)
	if i < 0 || call[i+1] != testEffortHigh {
		t.Fatalf("start omitted supervisor effort: %q", call)
	}
	r.calls = nil
	if _, err := l.Run(t.Context(), agentsName, agentStartName, "effort-worker", "brief", effortFlag, testEffortLow); err != nil {
		t.Fatal(err)
	}
	call = r.called(agentsName, agentStartName)
	if strings.Count(strings.Join(call, " "), effortFlag) != 1 || call[len(call)-1] != testEffortLow {
		t.Fatalf("explicit effort overridden: %q", call)
	}
}

func TestEffortRulePrecedence(t *testing.T) {
	a, _ := stubApp(t)
	a.cfg.Agents.Models = []config.AgentModelRule{
		{Match: testScanRule, Kind: taskKindReview, Effort: testEffortX},
		{Match: testScanRule, Effort: testEffortHigh},
	}
	for _, tc := range []struct{ name, kind, flag, want string }{
		{testScanName, workerRole, "", testEffortHigh},
		{testScanName, taskKindReview, "", testEffortX},
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

func TestEffortLoopYieldsToRule(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Supervisor.Effort = testEffortHigh
	r.a.cfg.Agents.Models = []config.AgentModelRule{{Match: testScanRule, Kind: taskKindReview, Effort: testEffortLow}}
	l := &Loop{Host: r.a}
	if _, err := l.Run(t.Context(), agentsName, agentStartName, "scan-review", "brief", "--"+reviewFlag); err != nil {
		t.Fatal(err)
	}
	if call := r.called(agentsName, agentStartName); slices.Contains(call, effortFlag) {
		t.Fatalf("loop default masked the matching rule: %q", call)
	}
	r.calls = nil
	if _, err := l.Run(t.Context(), agentsName, agentStartName, "scan-worker", "brief"); err != nil {
		t.Fatal(err)
	}
	call := r.called(agentsName, agentStartName)
	if i := slices.Index(call, effortFlag); i < 0 || call[i+1] != testEffortHigh {
		t.Fatalf("worker without a rule lost the loop default: %q", call)
	}
}

func TestCapacityEffortAfterHarnessSelection(t *testing.T) {
	a, _ := stubApp(t)
	dir := t.TempDir()
	a.cfg.Codex.ConfigFile = filepath.Join(dir, "config.toml")
	if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte(`{"models":[{"slug":"test-model","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a.cfg.Agents.Models = []config.AgentModelRule{{Match: ".*", Harness: codex.Harness, Kind: taskKindReview, Effort: testEffortLow}}
	for _, tc := range []struct {
		name, kind, harness, explicit, choice, want string
		invalid                                     bool
	}{
		{"worker choice", workerRole, codex.Harness, "", testEffortHigh, testEffortHigh, false},
		{"review rule", taskKindReview, codex.Harness, "", testEffortHigh, testEffortLow, false},
		{"explicit override", taskKindReview, codex.Harness, testEffortHigh, testEffortLow, testEffortHigh, false},
		{"agy after codex", taskKindReview, harnessAgy, "", "", "", false},
		{"unsupported model effort", workerRole, codex.Harness, "", testEffortMax, "", true},
		{"explicit agy refusal", workerRole, harnessAgy, testEffortHigh, "", "", true},
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
	v := &capacityView{Choice: &choiceView{Candidates: []choiceCandidate{{Harness: codex.Harness, Model: "test-model", Effort: testEffortHigh}}}}
	ch, err := pickAutoChoice(v, true)
	if err != nil || ch.Effort != testEffortHigh {
		t.Fatalf("choice=%+v err=%v", ch, err)
	}
}
