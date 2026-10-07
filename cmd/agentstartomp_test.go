//go:build unix

package cmd

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A wake of an omp agent goes to its inbox while its process reads it, and
// is refused once none does: nothing resumes an omp agent.
func TestWakeOmp(t *testing.T) {
	const id = "1234abcd-0000-4000-8000-000000000000"
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &app{cfg: &config.Config{StateDir: dir}, store: store, out: &out}
	ag := state.Agent{Party: state.Party{HostSession: omp.HostPrefix + id, Name: "test: omp"}}
	inbox := omp.InboxPath(dir, id)
	if err := omp.MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	if err := a.wakeOmp(state.Party{Name: "test"}, ag, id, "hello"); err == nil || !strings.Contains(err.Error(), "no longer runs") {
		t.Fatalf("wake with no reader = %v, want the refusal", err)
	}
	r, err := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // the test's inbox
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if err := a.wakeOmp(state.Party{Name: "test"}, ag, id, "hello"); err != nil {
		t.Fatal(err)
	}
	if line, _ := bufio.NewReader(r).ReadString('\n'); !strings.Contains(line, `"message":"hello"`) {
		t.Fatalf("inbox got %q", line)
	}
	if !strings.Contains(out.String(), "written to its inbox") {
		t.Fatalf("output %q", out.String())
	}
}

func TestRoleTextNamesTheHarness(t *testing.T) {
	sv := &sessionView{Session: newOmpSession()}
	if got := roleText(sv, "agent: fix it"); got != "omp busy agent: fix it" {
		t.Fatalf("roleText = %q", got)
	}
}

func newOmpSession() *claude.Session {
	return &claude.Session{Harness: omp.Harness, State: omp.StateBusy}
}

// An omp agent's model is named, from --model or omp.model, and is one
// omp lists by its exact selector: no default, no pattern.
func TestOmpModel(t *testing.T) {
	const local, remote = "ollama/qwen3.5:9b", "vllm/qwen3-8b"
	listed := func() ([]string, error) { return []string{local, remote}, nil }
	for _, tc := range []struct {
		flag, configured, want, err string
	}{
		{configured: local, want: local},
		{flag: remote, configured: local, want: remote},
		{err: "needs its model"},
		{configured: "qwen3.5", err: `lists no model "qwen3.5"`},
	} {
		got, err := ompModel(tc.flag, tc.configured, listed)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("ompModel(%q, %q) = %q, %v; want the error %q", tc.flag, tc.configured, got, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ompModel(%q, %q) = %q, %v; want %q", tc.flag, tc.configured, got, err, tc.want)
		}
	}
	if _, err := ompModel("", local, func() ([]string, error) { return nil, errors.New("omp failed") }); err == nil {
		t.Error("an unreadable model list passed")
	}
	if argv := ompArgv("omp", local); !slices.Equal(argv[len(argv)-2:], []string{modelFlag, local}) {
		t.Errorf("ompArgv = %v", argv)
	}
}

// An omp agent on a provider of omp.providers gets the provider's key from
// the reference, under the variable the models file names; a start on a
// key the file carries while a reference names it, on a variable no
// reference fills, or with no variable named is refused; a provider the
// config does not know starts on omp's own key as before.
func TestOmpCredentials(t *testing.T) {
	const planted = "planted-key-7b2e" //nolint:gosec // a planted test value
	const ref = sparkRef
	write := func(t *testing.T, raw string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "models.yml")
		if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, tc := range map[string]struct {
		models, model, ref, wantName, err string
	}{
		"from the vault":    {models: "providers: {spark: {apiKey: SPARK_API_KEY}}", model: sparkModel, ref: ref, wantName: "SPARK_API_KEY"},
		"keyless provider":  {models: "providers: {local: {auth: none}}", model: "local/m"},
		"omp's own key":     {models: "providers: {spark: {apiKey: " + planted + "}}", model: sparkModel},
		"built-in provider": {models: "providers: {}", model: "amazon-bedrock/m"},
		"value and ref":     {models: "providers: {spark: {apiKey: " + planted + "}}", model: sparkModel, ref: ref, err: "replace the value"},
		"ref without name":  {models: "providers: {spark: {baseUrl: x}}", model: sparkModel, ref: ref, err: "names no variable"},
		"name without ref":  {models: "providers: {spark: {apiKey: SPARK_API_KEY}}", model: sparkModel, err: "fills from no reference"},
	} {
		cfg := &config.Config{Omp: config.Omp{ModelsFile: write(t, tc.models)}}
		if tc.ref != "" {
			cfg.Omp.Providers = map[string]config.OmpProvider{"spark": {APIKey: tc.ref}}
		}
		a := &app{cfg: cfg}
		creds, err := a.ompCredentials(tc.model)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: %v, want the error %q", name, err, tc.err)
			}
			if err != nil && strings.Contains(err.Error(), planted) {
				t.Errorf("%s: the error carries the value: %v", name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		switch {
		case tc.wantName == "" && len(creds) != 0:
			t.Errorf("%s: credentials %+v, want none", name, creds)
		case tc.wantName != "" && (len(creds) != 1 || creds[0].Name != tc.wantName || creds[0].Ref.Op != tc.ref):
			t.Errorf("%s: credentials %+v", name, creds)
		}
	}
}

// The tests' vault provider: a model of it and its key's reference.
const (
	sparkModel = "spark/m"
	sparkRef   = "op://Shared/spark/credential" //nolint:gosec // a reference, no value
)

// A brokered omp start on a provider of omp.providers reads the vault,
// by its model or omp.model; any other start does not.
func TestOmpStartNeedsVault(t *testing.T) {
	const ompHarness = "--harness=omp"
	a := &app{cfg: &config.Config{Omp: config.Omp{Model: sparkModel, Providers: map[string]config.OmpProvider{"spark": {APIKey: sparkRef}}}}}
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{agentStartName, ompHarness, "--", "w", briefMD}, true},
		{[]string{agentStartName, ompHarness, "--model=spark/other", "--", "w", briefMD}, true},
		{[]string{agentStartName, ompHarness, "--model=ollama/m", "--", "w", briefMD}, false},
		{[]string{agentStartName, "--", "w", briefMD}, false},
		{[]string{"wake", "--", "w", "hi"}, false},
		{[]string{agentStartName, "--", ompHarness, briefMD}, false},
	} {
		if got := a.ompStartNeedsVault(tc.args); got != tc.want {
			t.Errorf("ompStartNeedsVault(%q) = %v", tc.args, got)
		}
	}
	if a.ompStartBrokered(sparkModel) {
		t.Error("brokered without secret.session")
	}
	a.cfg.Secret.Session = true
	if !a.ompStartBrokered("") || a.ompStartBrokered("ollama/m") {
		t.Error("the brokering does not follow the provider")
	}
	t.Setenv(sandbox.Brokered, "1")
	if a.ompStartBrokered(sparkModel) {
		t.Error("the broker's own call brokered again")
	}
}

// An omp agent's beekeeper commands act as its roster entry, even where its
// tool shell carries another session's variables: agents idle --done
// finishes it.
func TestOmpAgentIsTheCaller(t *testing.T) {
	const id = "1234abcd-0000-4000-8000-000000000000"
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "some-other-session")
	t.Setenv(omp.EnvAgent, id)
	t.Setenv(omp.EnvName, "omp worker")
	a := &app{cfg: &config.Config{StateDir: dir}, store: store, out: &bytes.Buffer{}}
	me, err := a.caller()
	if err != nil {
		t.Fatal(err)
	}
	ag := state.Agent{Party: state.Party{HostSession: omp.HostPrefix + id, Name: "omp worker"}}
	if !ag.Is(me) || me.Session != "" {
		t.Fatalf("caller = %+v, want the omp agent's roster entry", me)
	}
}
