package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestProfileName(t *testing.T) {
	cfg := &config.Config{Agents: config.Agents{Profile: "minimal"}, Supervisor: config.Supervisor{Role: config.Role{Profile: "lean"}}}
	for role, want := range map[string]string{workerRole: "minimal", supervisorRole.name: "lean", guideRole.name: "", "": ""} {
		if got := profileName(cfg, role); got != want {
			t.Errorf("profileName(%q) = %q, want %q", role, got, want)
		}
	}
	sup, guide, worker := state.Party{Session: "s"}, state.Party{Session: "g"}, state.Party{Session: "w"}
	st := &state.State{}
	st.SetSupervisorRole(state.Role{Holder: &state.Supervisor{Party: sup}})
	st.SetGuideRole(state.Role{Holder: &state.Supervisor{Party: guide}})
	for p, want := range map[state.Party]string{sup: supervisorRole.name, guide: guideRole.name, worker: workerRole} {
		if got := agentRole(st, p); got != want {
			t.Errorf("agentRole(%s) = %q, want %q", p.Session, got, want)
		}
	}
}

// A profile disables every installed plugin it does not keep, of every
// marketplace (an organisation's synced copy included), allows only its MCP
// servers and adds its deny rules; an empty list keeps everything.
func TestProfileSettings(t *testing.T) {
	installed := []string{"keeper@keeper", "base@desk", "base@synced", "content@desk", "content@synced"}
	s := profileSettings(config.Profile{Plugins: []string{"keeper", "base@desk"}, MCPServers: []string{"claude-in-chrome", "board"}, Deny: []string{"Agent"}}, installed)
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"allowedMcpServers":[{"serverName":"claude-in-chrome"},{"serverName":"board"}],` +
		`"enabledPlugins":{"base@synced":false,"content@desk":false,"content@synced":false},"permissions":{"deny":["Agent"]}}`
	if string(raw) != want {
		t.Errorf("settings = %s\nwant       %s", raw, want)
	}
	if s := profileSettings(config.Profile{Tools: []string{tToolBash}}, installed); len(s) != 0 {
		t.Errorf("a profile of tools alone has settings %v", s)
	}
}

func TestProfileFlags(t *testing.T) {
	listed := 0
	old := installedPlugins
	installedPlugins = func(context.Context) ([]string, error) {
		listed++
		return []string{"beekeeper@beekeeper", "craft@shop"}, nil
	}
	t.Cleanup(func() { installedPlugins = old })
	a := &app{cfg: &config.Config{StateDir: t.TempDir()}}
	flags, err := a.profileFlags(context.Background(), config.MinimalProfile)
	if err != nil {
		t.Fatal(err)
	}
	if len(flags) != 4 || flags[0] != toolsFlag || flags[2] != settingsFlag || listed != 1 {
		t.Fatalf("flags = %q (plugins listed %d times)", flags, listed)
	}
	tools := strings.Split(flags[1], ",")
	for _, keep := range []string{tToolBash, "Read", "Edit", "Write", "Skill", "ToolSearch", "SendMessage"} {
		if !slices.Contains(tools, keep) {
			t.Errorf("minimal drops %s: %q", keep, tools)
		}
	}
	for _, drop := range []string{"Artifact", "AskUserQuestion", "Workflow", "Agent"} {
		if slices.Contains(tools, drop) {
			t.Errorf("minimal keeps %s: %q", drop, tools)
		}
	}
	raw, err := os.ReadFile(flags[3])
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		EnabledPlugins    map[string]bool     `json:"enabledPlugins"`
		AllowedMcpServers []map[string]string `json:"allowedMcpServers"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if len(s.EnabledPlugins) != 1 || s.EnabledPlugins["craft@shop"] || len(s.AllowedMcpServers) != 1 || s.AllowedMcpServers[0]["serverName"] != "claude-in-chrome" {
		t.Errorf("minimal's settings = %s", raw)
	}
	if _, err := a.profileFlags(context.Background(), "lean"); err == nil {
		t.Error("an unknown profile gave flags")
	}
}

// A profiled turn carries the profile's flags ahead of its prompt, beside
// its bypass and Chrome connection.
func TestProfiledTurnFlags(t *testing.T) {
	profile := []string{toolsFlag, "Read,Edit", settingsFlag, "/state/profiles/minimal.json"}
	for turn, argv := range map[string][]string{
		"start": headlessStartArgv("claude", "id", "n", "", "b", profile...),
		"wake":  wakeArgv("claude", wakeTarget{id: "id", mode: state.ModeBypass}, "m", profile...),
	} {
		flags := argv[:slices.Index(argv, "--")]
		if !slices.Contains(flags, chromeFlag) || !slices.Equal(flags[len(flags)-4:], profile) {
			t.Errorf("%s argv = %q, want %s and the profile's flags before the prompt", turn, argv, chromeFlag)
		}
	}
}

func TestDryRunStartNamesTheProfile(t *testing.T) {
	old := installedPlugins
	installedPlugins = func(context.Context) ([]string, error) { return nil, nil }
	t.Cleanup(func() { installedPlugins = old })
	dir := t.TempDir()
	var out bytes.Buffer
	a := &app{cfg: &config.Config{StateDir: dir, Agents: config.Agents{Dir: dir, Profile: config.MinimalProfile}}, out: &out}
	if err := a.dryRunStart(context.Background(), agentStart{name: "w", role: workerRole, dir: dir}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "under profile minimal: its task runs as a headless turn under profile minimal") || !strings.Contains(got, settingsFlag) {
		t.Errorf("dry run = %s", got)
	}
	out.Reset()
	a.cfg.Agents.Profile = ""
	if err := a.dryRunStart(context.Background(), agentStart{name: "w", role: workerRole, dir: dir}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "under no profile") || strings.Contains(got, settingsFlag) {
		t.Errorf("dry run without a profile = %s", got)
	}
}
