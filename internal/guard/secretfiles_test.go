package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHome sets up a home directory with an omp provider configuration
// holding fake credentials, a SQLite-like binary file and a config-listed
// file, and returns the home and the hook guarding them.
func fakeHome(t *testing.T) (string, Hook) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	agent := filepath.Join(home, ".omp", "agent")
	if err := os.MkdirAll(filepath.Join(agent, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(agent, "models.yml"):          "providers:\n  spark:\n    baseUrl: http://x\n    apiKey: FAKE-KEY\n    headers:\n      X-Auth-Token: FAKE-TOKEN\n",
		filepath.Join(agent, "agent.db"):            "SQLite format 3\x00FAKE",
		filepath.Join(agent, "sessions", "s.jsonl"): "{}\n",
		filepath.Join(home, "app", "creds.json"):    `{"user":"u","password":"FAKE"}`,
		filepath.Join(home, "app", "notes.txt"):     "nothing\n",
	}
	for p, body := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h := hook()
	h.SecretFiles = []string{"~/app/creds.*"}
	return home, h
}

// The forms a secret file's refusal names, and a credential key it holds.
const (
	stripForm = "del(.. |"
	grepForm  = "grep -c|-l"
	credKey   = "apiKey"
)

func TestSecretFileBashReads(t *testing.T) {
	home, h := fakeHome(t)
	for _, tc := range []struct {
		cmd, safe string
	}{
		// refused: the file read whole, with the strip built from its keys
		{"cat ~/.omp/agent/models.yml", "yq -y 'del(.. | .\"X-Auth-Token\"?, .apiKey?)'"},
		{"cat $HOME/.omp/agent/models.yml", stripForm},
		{`less "${HOME}/.omp/agent/models.yml"`, stripForm},
		{"head -n 20 " + home + "/.omp/agent/models.yml", stripForm},
		{"tail ~/.omp/agent/models.yml", stripForm},
		{"cd ~/.omp/agent && cat models.yml", stripForm},
		{"cat ~/.omp/agent/*.yml", stripForm},
		{"jq . ~/.omp/agent/models.yml", stripForm},
		{"yq '.' ~/.omp/agent/models.yml", stripForm},
		{"yq '.providers' < ~/.omp/agent/models.yml", stripForm},
		{"yq 'del(.. | .apiKey?)' ~/.omp/agent/models.yml", stripForm},
		{`yq 'del(.. | .apiKey?, ."X-Auth-Token"?) | input' ~/.omp/agent/models.yml`, stripForm},
		{"grep apiKey ~/.omp/agent/models.yml", grepForm},
		{"grep -r apiKey ~/.omp", grepForm},
		{"rg Key ~/.omp/agent", grepForm},
		{"cat ~/.omp/agent/models.yml | grep apiKey", stripForm},
		{"cp ~/.omp/agent/models.yml /tmp/m.yml", stripForm},
		{"sqlite3 ~/.omp/agent/agent.db .dump", "no YAML or JSON file"},
		{"strings ~/.omp/agent/agent.db", "no YAML or JSON file"},
		{"cat ~/app/creds.json", "del(.. | .password?)"},
		{"bash -c 'cat ~/.omp/agent/models.yml'", stripForm},
		// passes: metadata, counts, key names, a key-stripped read
		{"ls -la ~/.omp/agent/", ""},
		{"stat ~/.omp/agent/models.yml", ""},
		{"wc -c ~/.omp/agent/models.yml", ""},
		{"grep -c apiKey ~/.omp/agent/models.yml", ""},
		{"grep -l apiKey ~/.omp/agent/models.yml", ""},
		{"yq 'keys' ~/.omp/agent/models.yml", ""},
		{`yq -y 'del(.. | .apiKey?, ."X-Auth-Token"?)' ~/.omp/agent/models.yml`, ""},
		{`jq 'del(.. | .apiKey?, ."X-Auth-Token"?) | .providers' ~/.omp/agent/models.yml`, ""},
		{`cat ~/.omp/agent/models.yml | yq -y 'del(.. | .apiKey?, ."X-Auth-Token"?)'`, ""},
		{"cat ~/.omp/agent/models.yml | wc -l", ""},
		{"cat ~/.omp/agent/sessions/s.jsonl", ""},
		{"cat ~/app/notes.txt", ""},
		{"grep -r apiKey " + home, ""},
		{"echo hi > ~/.omp/agent/models.yml.bak", ""},
	} {
		d := decide(t, h, home, tc.cmd, nil)
		denied := d != nil && d.PermissionDecision == decisionDeny
		switch {
		case tc.safe == "" && denied:
			t.Errorf("%q refused: %s", tc.cmd, d.Reason)
		case tc.safe != "" && !denied:
			t.Errorf("%q passed, want a refusal", tc.cmd)
		case tc.safe != "" && !strings.Contains(d.Reason, tc.safe):
			t.Errorf("%q: refusal lacks %q:\n%s", tc.cmd, tc.safe, d.Reason)
		case denied && strings.Contains(d.Reason, "FAKE"):
			t.Errorf("%q: refusal carries a value:\n%s", tc.cmd, d.Reason)
		}
	}
}

func TestSecretFileTools(t *testing.T) {
	home, h := fakeHome(t)
	models := filepath.Join(home, ".omp", "agent", "models.yml")
	for _, tc := range []struct {
		tool   string
		input  map[string]any
		refuse bool
	}{
		{readTool, map[string]any{filePathKey: models}, true},
		{readTool, map[string]any{filePathKey: filepath.Join(home, "app", "creds.json")}, true},
		{readTool, map[string]any{filePathKey: filepath.Join(home, "app", "notes.txt")}, false},
		{readTool, map[string]any{filePathKey: filepath.Join(home, ".omp", "agent", "sessions", "s.jsonl")}, false},
		{grepTool, map[string]any{patternKey: credKey, pathKey: models, outputModeKey: contentKey}, true},
		{grepTool, map[string]any{patternKey: credKey, pathKey: filepath.Join(home, ".omp"), outputModeKey: contentKey}, true},
		{grepTool, map[string]any{patternKey: credKey, pathKey: filepath.Join(home, ".omp"), outputModeKey: contentKey, "glob": "*.jsonl"}, false},
		{grepTool, map[string]any{patternKey: credKey, pathKey: models}, false},
		{grepTool, map[string]any{patternKey: credKey, pathKey: models, outputModeKey: "count"}, false},
		{grepTool, map[string]any{patternKey: credKey, pathKey: home, outputModeKey: contentKey}, false},
	} {
		ev := toolEvent(tc.tool, tc.input)
		ev["cwd"] = home
		d := decideEvent(t, h, ev)
		denied := d != nil && d.PermissionDecision == decisionDeny
		if denied != tc.refuse {
			t.Errorf("%s %v: refused %v, want %v", tc.tool, tc.input, denied, tc.refuse)
		}
		if denied && (!strings.Contains(d.Reason, "beekeeper secret") || !strings.Contains(d.Reason, stripForm)) {
			t.Errorf("%s %v: refusal names no alternative:\n%s", tc.tool, tc.input, d.Reason)
		}
	}
}

func TestSecretFilesConfig(t *testing.T) {
	home, _ := fakeHome(t)
	f := newSecretFiles([]string{"~/app/creds.*", "relative/ignored"})
	if got := f.match(filepath.Join(home, "app", "creds.json")); got == "" {
		t.Error("a configured glob guards nothing")
	}
	if got := f.match(filepath.Join(home, ".omp", "agent", "models.yml")); got == "" {
		t.Error("the built-in list is gone beside the configured one")
	}
	for _, g := range f.globs {
		if !filepath.IsAbs(g) {
			t.Errorf("relative glob kept: %q", g)
		}
	}
	link := filepath.Join(home, "m.yml")
	if err := os.Symlink(filepath.Join(home, ".omp", "agent", "models.yml"), link); err != nil {
		t.Fatal(err)
	}
	if f.match(link) == "" {
		t.Error("a symlink to a guarded file is no read of it")
	}
}

func TestCredentialKey(t *testing.T) {
	for k, want := range map[string]bool{
		"apiKey": true, "api_key": true, "X-Auth-Token": true, "token": true, "clientSecret": true, "password": true,
		"Authorization": true, "credentials": true, "maxTokens": false, "baseUrl": false, "keys": false, "model": false,
	} {
		if got := credentialKey.MatchString(k); got != want {
			t.Errorf("%q: credential %v, want %v", k, got, want)
		}
	}
}
