package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
	if strings.Contains(p, plugin.Skill(plugin.Topics[1])) {
		t.Error("a topic the plugin serves stays out of the prompt")
	}
}

// A topic the installed plugin does not serve as this binary embeds it
// follows the rules in the prompt, ahead of the task.
func TestWorkerPromptCarriesTheTopicsThePluginLacks(t *testing.T) {
	task := "BK 2 thing"
	inline := plugin.Topics[:2]
	p := workerPrompt(taskPrompt(task), inline...)
	for _, topic := range inline {
		if i, j := strings.Index(p, plugin.Skill(topic)), strings.Index(p, task); i < 0 || j < i {
			t.Errorf("%s is not between the rules and the task:\n%s", topic, p)
		}
	}
	if !strings.Contains(p, "does not serve "+topicSkills(inline)+" as") || strings.Contains(p, plugin.Skill(plugin.Topics[3])) {
		t.Errorf("the prompt names the inline topics and only those:\n%s", p)
	}
	if briefOf(p) != task {
		t.Errorf("briefOf(prompt) = %q, want the task", briefOf(p))
	}
}

// pluginGaps keeps a topic out of the prompt only where an enabled
// beekeeper plugin carries the embedded SKILL.md byte for byte.
func TestPluginGaps(t *testing.T) {
	current, stale := t.TempDir(), t.TempDir()
	for _, topic := range plugin.Topics {
		writeSkill(t, current, topic, plugin.SkillFile(topic))
	}
	writeSkill(t, stale, plugin.Topics[0], plugin.SkillFile(plugin.Topics[0]))
	writeSkill(t, stale, plugin.Topics[1], []byte("---\nname: "+plugin.Topics[1]+"\n---\n# an older text\n"))
	const bk = pluginName + "@" + pluginName
	for name, tc := range map[string]struct {
		ps   []installedPlugin
		want []string
	}{
		"none installed":   {nil, plugin.Topics},
		"current":          {[]installedPlugin{{ID: bk, InstallPath: current, Enabled: true}}, nil},
		"current, project": {[]installedPlugin{{ID: bk, InstallPath: current, ProjectEnabled: true}}, nil},
		"disabled":         {[]installedPlugin{{ID: bk, InstallPath: current}}, plugin.Topics},
		"another plugin":   {[]installedPlugin{{ID: "keeper@beekeeper", InstallPath: current, Enabled: true}}, plugin.Topics},
		"out of date":      {[]installedPlugin{{ID: bk, InstallPath: stale, Enabled: true}}, plugin.Topics[1:]},
		"out of date and current": {[]installedPlugin{
			{ID: bk, InstallPath: stale, Enabled: true},
			{ID: "beekeeper@mirror", InstallPath: current, Enabled: true},
		}, nil},
	} {
		listPlugins = func(context.Context) ([]installedPlugin, error) { return tc.ps, nil }
		if got := pluginGaps(context.Background()); !slices.Equal(got, tc.want) {
			t.Errorf("%s: pluginGaps = %q, want %q", name, got, tc.want)
		}
	}
	listPlugins = func(context.Context) ([]installedPlugin, error) { return nil, errors.New("no claude") }
	if got := pluginGaps(context.Background()); !slices.Equal(got, plugin.Topics) {
		t.Errorf("no plugin list: pluginGaps = %q, want every topic", got)
	}
	listPlugins = func(context.Context) ([]installedPlugin, error) { return nil, nil }
	last := plugin.Topics[3:]
	if gapsNote(nil) != "" || !strings.Contains(gapsNote(last), topicSkills(last)) {
		t.Errorf("gapsNote: %q, %q", gapsNote(nil), gapsNote(last))
	}
}

func writeSkill(t *testing.T, dir, name string, raw []byte) {
	t.Helper()
	p := filepath.Join(dir, plugin.SkillsDir, name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
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
	if _, err := roleAddress(nil, supervisorRole, nil, noSocket); err == nil || !strings.Contains(err.Error(), "no session holds the supervisor's role") {
		t.Errorf("no holder: %v", err)
	}
	if got, err := roleAddress(holder, supervisorRole, nil, noSocket); err != nil || got != hostH60 {
		t.Errorf("no CLI: %q, %v", got, err)
	}
	live := []*claude.Session{{PID: 7, ID: "s60", HostID: hostH60, Name: cliName}}
	if got, err := roleAddress(holder, supervisorRole, live, noSocket); err != nil || got != cliName {
		t.Errorf("running CLI: %q, %v", got, err)
	}
	twins := append(live, &claude.Session{PID: 8, ID: "s61", Name: cliName})
	if _, err := roleAddress(holder, supervisorRole, twins, noSocket); err == nil {
		t.Error("an ambiguous name is refused")
	}
	if got, _ := roleAddress(&state.Supervisor{Party: state.Party{Name: "Guide run 3"}}, guideRole, nil, noSocket); got != "Guide run 3" {
		t.Errorf("no session known: the name, got %q", got)
	}
}
