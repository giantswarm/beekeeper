package cmd

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	testEffortHigh = "high"
	testEffortLow  = "low"
)

func TestEffortBackgroundLaunchAndRecord(t *testing.T) {
	for _, level := range []string{"", testEffortLow, "medium", testEffortHigh, "xhigh", "max"} {
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
	l := &supervisorLoop{a: r.a}
	if _, err := l.run(t.Context(), agentsName, agentStartName, "effort-worker", "brief"); err != nil {
		t.Fatal(err)
	}
	call := r.called(agentsName, agentStartName)
	i := slices.Index(call, effortFlag)
	if i < 0 || call[i+1] != testEffortHigh {
		t.Fatalf("start omitted supervisor effort: %q", call)
	}
	r.calls = nil
	if _, err := l.run(t.Context(), agentsName, agentStartName, "effort-worker", "brief", effortFlag, testEffortLow); err != nil {
		t.Fatal(err)
	}
	call = r.called(agentsName, agentStartName)
	if strings.Count(strings.Join(call, " "), effortFlag) != 1 || call[len(call)-1] != testEffortLow {
		t.Fatalf("explicit effort overridden: %q", call)
	}
}
