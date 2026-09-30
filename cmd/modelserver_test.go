package cmd

import (
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/machine"
)

// A lab resource and the kind node its first cluster runs.
const (
	labOne     = "agentlab-1"
	labOneNode = "172.21.0.3 agentlab-control-plane"
	labTwo     = "agentlab-2"
	bigModel   = "big"
)

func TestModelBreaches(t *testing.T) {
	resources := []string{labOne, labTwo}
	lab1 := machine.OllamaModel{Name: bigModel, SizeMiB: 18000, Client: labOneNode}
	lab2 := machine.OllamaModel{Name: "mid", SizeMiB: 6000, Client: "172.21.0.2 agentlab-2-control-plane"}
	host := machine.OllamaModel{Name: "host", SizeMiB: 3000, Client: "127.0.0.1"}
	a := lease.Holder{Env: labOne, Session: "a", Name: "Agent A"}
	b := lease.Holder{Env: labTwo, Session: "b", Name: "Agent B"}
	msA := func(gib int) lease.Holder {
		return lease.Holder{Env: config.ModelServer, Session: "a", Name: "Agent A", BudgetGiB: gib}
	}
	for name, tc := range map[string]struct {
		models  []machine.OllamaModel
		holders []lease.Holder
		want    map[string]bool // model → unloaded
		reason  string
	}{
		"no lease: a lab load is unloaded, a host load named": {
			[]machine.OllamaModel{lab1, host}, []lease.Holder{a},
			map[string]bool{bigModel: true, "host": false}, "nobody holds the model-server lease",
		},
		"the holder's lab within its budget": {
			[]machine.OllamaModel{lab1}, []lease.Holder{a, msA(24)}, map[string]bool{}, "",
		},
		"another session's lab": {
			[]machine.OllamaModel{lab2}, []lease.Holder{a, b, msA(24)},
			map[string]bool{"mid": true}, `the model server is "Agent A"'s (lab agentlab-2, held by "Agent B")`,
		},
		"over budget: the largest goes": {
			[]machine.OllamaModel{host, lab1}, []lease.Holder{a, msA(16)},
			map[string]bool{bigModel: true}, `"Agent A"'s models hold 21000 MiB, over its model-server budget of 16 GiB`,
		},
		"a claim without budget takes the default": {
			[]machine.OllamaModel{lab1}, []lease.Holder{a, msA(0)}, map[string]bool{bigModel: true}, "over its model-server budget of 12 GiB",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := modelBreaches(tc.models, tc.holders, resources, 12)
			if len(got) != len(tc.want) {
				t.Fatalf("breaches %v, want %v", got, tc.want)
			}
			for _, br := range got {
				unload, ok := tc.want[br.Model.Name]
				if !ok || unload != br.Unload {
					t.Errorf("%s: unload %v, want %v (listed %v)", br.Model.Name, br.Unload, unload, ok)
				}
				if !strings.Contains(br.Reason, tc.reason) {
					t.Errorf("reason %q lacks %q", br.Reason, tc.reason)
				}
			}
		})
	}
}

func TestLabOf(t *testing.T) {
	res := []string{labOne, labTwo, "kind-x"}
	for client, want := range map[string]string{
		labOneNode:                            labOne,
		"172.21.0.2 agentlab-2-control-plane": labTwo,
		"172.21.0.9 kind-x-worker2":           "kind-x",
		"172.21.0.5 agentlab-registry":        "",
		"127.0.0.1":                           "",
	} {
		if got := labOf(machine.OllamaModel{Client: client}, res); got != want {
			t.Errorf("labOf(%q) = %q, want %q", client, got, want)
		}
	}
}

func TestModelServerLine(t *testing.T) {
	bs := []modelBreach{{Model: machine.OllamaModel{Name: bigModel, SizeMiB: 1, Client: "c"}, Reason: "r", Unload: true}, {Model: machine.OllamaModel{Name: "h", SizeMiB: 2}, Reason: "s"}}
	got := modelServerLine(bs, map[string]error{bigModel: nil})
	if want := "big 1 MiB loaded by c: r; unloaded | h 2 MiB loaded by an unknown client: s"; got != want {
		t.Errorf("line %q, want %q", got, want)
	}
}

func TestClaimBudget(t *testing.T) {
	a := &app{cfg: &config.Config{Ollama: config.Ollama{BudgetGiB: 12, MaxBudgetGiB: 24}}}
	for _, tc := range []struct {
		res  string
		gib  int
		set  bool
		want int
		ok   bool
	}{
		{config.ModelServer, 0, false, 12, true},
		{config.ModelServer, 8, true, 8, true},
		{config.ModelServer, 30, true, 0, false},
		{config.ModelServer, 0, true, 0, false},
		{labOne, 0, false, 0, true},
		{labOne, 8, true, 0, false},
	} {
		got, err := a.claimBudget(tc.res, tc.gib, tc.set)
		if got != tc.want || (err == nil) != tc.ok {
			t.Errorf("claimBudget(%s, %d, %v) = %d, %v", tc.res, tc.gib, tc.set, got, err)
		}
	}
}
