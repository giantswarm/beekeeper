package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestWithReviewModelWithoutAModel(t *testing.T) {
	flags := []string{toolsFlag, ""}
	got, err := withReviewModel(" ", flags)
	if err != nil || !slices.Equal(got, flags) {
		t.Fatalf("got %q, %v: no model leaves the flags", got, err)
	}
}

func TestWithReviewModelInline(t *testing.T) {
	got, err := withReviewModel("claude-sonnet-5-5", []string{toolsFlag, ""})
	if err != nil || len(got) != 4 || got[2] != settingsFlag {
		t.Fatalf("got %q, %v", got, err)
	}
	var s struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(got[3]), &s); err != nil || s.Env[securityReviewEnv] != "claude-sonnet-5-5" {
		t.Errorf("settings %q (%v), want the review model in env", got[3], err)
	}
}

func TestWithReviewModelConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.json")
	if err := os.WriteFile(path, []byte(`{"env":{"KEEP":"1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	const workers = 32
	const model = "opus"
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			<-start
			for range 10 {
				flags, err := withReviewModel(model, []string{settingsFlag, path})
				if err != nil {
					t.Errorf("concurrent review settings: %v", err)
					return
				}
				raw, err := os.ReadFile(flags[1])
				var settings struct {
					Env map[string]string `json:"env"`
				}
				if err != nil || json.Unmarshal(raw, &settings) != nil || settings.Env[securityReviewEnv] != model || settings.Env["KEEP"] != "1" {
					t.Errorf("concurrent settings must stay complete: %q (%v)", raw, err)
					return
				}
			}
		})
	}
	close(start)
	wg.Wait()
}

// A profile's settings file is copied per model with the review model added:
// Claude Code reads one --settings, and the profile's own file stays.
func TestWithReviewModelMergesTheProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.json")
	if err := os.WriteFile(path, []byte(`{"enabledPlugins":{"p@m":true},"env":{"KEEP":"1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	flags := []string{toolsFlag, tToolBash, settingsFlag, path}
	got, err := withReviewModel("opus[1m]", flags)
	if err != nil {
		t.Fatal(err)
	}
	if flags[3] != path || len(got) != 4 || got[3] == path || filepath.Dir(got[3]) != filepath.Dir(path) {
		t.Fatalf("flags %q, got %q: want a sibling copy and the caller's flags untouched", flags, got)
	}
	var s struct {
		Plugins map[string]bool   `json:"enabledPlugins"`
		Env     map[string]string `json:"env"`
	}
	raw, err := os.ReadFile(got[3])
	if err != nil || json.Unmarshal(raw, &s) != nil {
		t.Fatalf("copy %s: %v", got[3], err)
	}
	if !s.Plugins["p@m"] || s.Env["KEEP"] != "1" || s.Env[securityReviewEnv] != "opus[1m]" {
		t.Errorf("copy %s, want the profile plus the review model", raw)
	}
	if raw, _ := os.ReadFile(filepath.Clean(path)); strings.Contains(string(raw), securityReviewEnv) {
		t.Errorf("the profile's own file changed: %s", raw)
	}
}

func TestWakeArgvCarriesTheReviewModel(t *testing.T) {
	a := &app{cfg: &config.Config{StateDir: t.TempDir()}}
	argv, err := a.profiledWakeArgv(t.Context(), "claude", wakeTarget{id: "s1", mode: state.ModeBypass, model: "claude-haiku-5-5"}, "go on")
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(argv, settingsFlag)
	if i < 0 || !strings.Contains(argv[i+1], `"SECURITY_REVIEW_MODEL":"claude-haiku-5-5"`) || i > slices.Index(argv, "--") {
		t.Errorf("argv %q, want the review model's settings before the message", argv)
	}
	if argv, _ := a.profiledWakeArgv(t.Context(), "claude", wakeTarget{id: "s1", mode: state.ModeBypass}, "go on"); slices.Contains(argv, settingsFlag) {
		t.Errorf("argv %q: a wake without a model keeps the person's setting", argv)
	}
}

// Two workers in one folder split its reviews by registration; one of
// another harness and a review before both are nobody's.
func TestAttachSDKChildren(t *testing.T) {
	projects := t.TempDir()
	dir := filepath.Join(projects, "-wt")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name string, lines ...string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	review := func(id, at string) {
		write(id+".jsonl",
			`{"type":"user","entrypoint":"sdk-py","timestamp":"`+at+`","message":{"content":"review"}}`,
			`{"type":"assistant","timestamp":"`+at+`","message":{"id":"`+id+`","model":"claude-opus-4-7","usage":{"input_tokens":1000000}}}`)
	}
	write("w1.jsonl", `{"type":"user","entrypoint":"cli","message":{"content":"task"}}`)
	write("w2.jsonl", `{"type":"user","entrypoint":"cli","message":{"content":"task"}}`)
	review("early", "2026-10-08T08:30:00Z")
	review("r-w1", "2026-10-08T09:30:00Z")
	review("r-w2", "2026-10-08T10:30:00Z")
	at := func(h int) time.Time { return time.Date(2026, 10, 8, h, 0, 0, 0, time.UTC) }
	views := []agentView{
		{agentFields: agentFields{Party: state.Party{Name: "W1", Session: "w1"}, Registered: at(9)}},
		{agentFields: agentFields{Party: state.Party{Name: "W2", Session: "w2"}, Registered: at(10)}},
		{agentFields: agentFields{Party: state.Party{Name: "O", Session: "w1"}, Registered: at(8), Harness: omp.Harness}},
	}
	a := &app{cfg: &config.Config{Claude: config.Claude{ProjectsDir: projects}, Metrics: config.Metrics{Models: map[string]config.Model{"claude-opus-4-7": {Input: 5}}}}}
	a.attachSDKChildren(views)
	names := func(cs []claude.SDKChild) (out []string) {
		for _, c := range cs {
			out = append(out, c.Session)
		}
		return out
	}
	if got := names(views[0].SDKChildren); !slices.Equal(got, []string{"r-w1"}) {
		t.Errorf("W1 children %q, want r-w1", got)
	}
	if got := names(views[1].SDKChildren); !slices.Equal(got, []string{"r-w2"}) {
		t.Errorf("W2 children %q, want r-w2", got)
	}
	if views[2].SDKChildren != nil {
		t.Errorf("omp worker children %+v", views[2].SDKChildren)
	}
	if cell := sdkChildrenCell(views[0].SDKChildren); cell != "1 $5.00 claude-opus-4-7" {
		t.Errorf("cell %q", cell)
	}
	if cell := sdkChildrenCell(nil); cell != "-" {
		t.Errorf("empty cell %q", cell)
	}
}
