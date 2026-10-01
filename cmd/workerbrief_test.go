package cmd

import (
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/plugin"
)

// A worker's first prompt is the shipped rules, then its task; a hand-over
// passes on the task alone and puts the rules ahead of it again.
func TestWorkerPromptCarriesTheRulesAndTheTask(t *testing.T) {
	task := "BK 1 thing: giantswarm/beekeeper#1 to done\n\nDo the thing."
	p := workerPrompt(taskPrompt(task))
	if !strings.Contains(p, plugin.WorkerRules()) {
		t.Fatal("the worker rules are not in the prompt")
	}
	if i, j := strings.Index(p, plugin.WorkerRules()), strings.Index(p, task); i < 0 || j < i {
		t.Errorf("the rules come first, then the task:\n%.300s", p)
	}
	if got := briefOf(p); got != task {
		t.Errorf("briefOf(prompt) = %q, want the task", got)
	}
	h := handover{agent: state.Agent{Party: state.Party{Name: "w", Session: "s"}, Task: "BK 1 thing"}, brief: briefOf(p)}
	next := workerPrompt(h.prompt())
	if strings.Count(next, plugin.WorkerRules()) != 1 || briefOf(next) != task {
		t.Errorf("a hand-over carries the rules once and the task as its brief:\n%s", next)
	}
	if briefTask(task) != "BK 1 thing: giantswarm/beekeeper#1 to done" {
		t.Errorf("the roster's task is the brief's first line: %q", briefTask(task))
	}
}

const (
	hostH60 = "local_h60"
	cliName = "Swap finding 1051"
)

// A message to the supervisor goes to the holder's running CLI by the name
// it answers to, its desktop session when none runs.
func TestRoleAddress(t *testing.T) {
	holder := &state.Supervisor{Party: state.Party{Name: "Supervisor run 60", Session: "s60", HostSession: hostH60}}
	if _, err := roleAddress(nil, supervisorRole, nil); err == nil || !strings.Contains(err.Error(), "no session holds the supervisor's role") {
		t.Errorf("no holder: %v", err)
	}
	if got, err := roleAddress(holder, supervisorRole, nil); err != nil || got != hostH60 {
		t.Errorf("no CLI: %q, %v", got, err)
	}
	live := []*claude.Session{{PID: 7, ID: "s60", HostID: hostH60, Name: cliName}}
	if got, err := roleAddress(holder, supervisorRole, live); err != nil || got != cliName {
		t.Errorf("running CLI: %q, %v", got, err)
	}
	twins := append(live, &claude.Session{PID: 8, ID: "s61", Name: cliName})
	if _, err := roleAddress(holder, supervisorRole, twins); err == nil {
		t.Error("an ambiguous name is refused")
	}
	if got, _ := roleAddress(&state.Supervisor{Party: state.Party{Name: "Guide run 3"}}, guideRole, nil); got != "Guide run 3" {
		t.Errorf("no session known: the name, got %q", got)
	}
}
