package secret_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

// envNotes is an env-style notes field holding the planted values.
const envNotes = "# Backstage\nexport GITHUB_TOKEN=\"" + token + "\"\n\nOPENAI_API_KEY=" + password + "\nnot a line\nEMPTY=\n"

const (
	notes   = "notesPlain"
	envItem = "op://Shared/Backstage Environment Variables/" + notes
	envKey  = envItem + "#OPENAI_API_KEY"
)

// The listing answers the KEY names of an env-style field and their
// values' lengths, never a value, also in its JSON form.
func TestItemsAnswerEnvKeysAndLengthsOnly(t *testing.T) {
	tools := secrettest.New(map[string]string{envItem: envNotes, vaultRef: token})
	got, err := ops(tools).Items(context.Background(), shared, secret.ItemsOptions{Match: "environment", EnvKeys: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Fields != nil {
		t.Fatalf("items = %+v", got)
	}
	want := []secret.EnvKey{
		{Field: notes, Key: "GITHUB_TOKEN", Length: len(token)},
		{Field: notes, Key: "OPENAI_API_KEY", Length: len(password)},
		{Field: notes, Key: "EMPTY", Length: 0},
	}
	if len(got[0].EnvKeys) != len(want) {
		t.Fatalf("env keys = %+v, want %+v", got[0].EnvKeys, want)
	}
	for i, k := range want {
		if got[0].EnvKeys[i] != k {
			t.Errorf("env key %d = %+v, want %+v", i, got[0].EnvKeys[i], k)
		}
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	noValue(t, "the env-key listing", string(raw))
	// a field without KEY=VALUE lines answers no key, a concealed one is
	// never parsed: its text before an "=" is part of the value
	tools.Vault[vaultRef] = "planted" + token + "=="
	plain, err := ops(tools).Items(context.Background(), shared, secret.ItemsOptions{Match: "api", EnvKeys: true})
	if err != nil || len(plain) != 1 || plain[0].EnvKeys != nil {
		t.Errorf("a concealed field's env keys = %+v, %v", plain, err)
	}
}

// One line of an env-style field is a copy's source like a field: into a
// SOPS path, and to a consumer's stdin, the value in no answer.
func TestCopyOneLineOfAnEnvStyleField(t *testing.T) {
	tools := secrettest.New(map[string]string{envItem: envNotes})
	dir, src := scratch(t)
	o := ops(tools)
	ctx := context.Background()
	r, err := secret.ParseRef(envKey)
	if err != nil {
		t.Fatal(err)
	}
	n, err := o.CopyValue(ctx, r, secret.Ref{File: src, Path: "stringData.openaiApiKey"})
	if err != nil || n != len(password) {
		t.Fatalf("copy = %d, %v", n, err)
	}
	if plain := decrypted(t, tools, src); !strings.Contains(plain, "openaiApiKey: "+password+"\n") {
		t.Errorf("the file after the copy:\n%s", plain)
	}
	if raw, _ := os.ReadFile(src); strings.Contains(string(raw), password) { //nolint:gosec // the test's scratch file
		t.Error("the copy wrote the value in plaintext")
	}
	keys, err := o.CopyValues(ctx, []secret.Pair{{Src: r, Path: "openai"}}, filepath.Join(dir, "new.sops.yaml"), nil)
	if err != nil || len(keys) != 1 || keys[0].Bytes != len(password) {
		t.Errorf("copy into a new file = %+v, %v", keys, err)
	}
	code, out, err := o.CopyToConsumer(ctx, r, []string{"sh", "-c", "cat; true", "--password-stdin"}, secret.Stdin{})
	if err != nil || code != 0 || out != "[redacted: "+envKey+"]" {
		t.Errorf("consumer = %d, %q, %v", code, out, err)
	}
	vs, err := o.Compare(ctx, r, secret.Ref{File: src, Path: "stringData.openaiApiKey"})
	if err != nil || len(vs) != 1 || vs[0].State != secret.Equal {
		t.Errorf("compare = %+v, %v", vs, err)
	}
	for _, c := range tools.Calls {
		if strings.Contains(c, "#") {
			t.Errorf("op was handed the key: %q", c)
		}
	}
}

// A key absent from the field, or named on two lines, is refused naming
// field and key alone; a malformed key is refused before op runs, and a
// line is never a write destination.
func TestEnvKeyRefusals(t *testing.T) {
	twice := envNotes + "OPENAI_API_KEY=" + token + "\n"
	tools := secrettest.New(map[string]string{envItem: twice})
	o := ops(tools)
	ctx := context.Background()
	_, src := scratch(t)
	for key, want := range map[string]string{"MISSING": "no line MISSING", "OPENAI_API_KEY": "2 lines carry OPENAI_API_KEY"} {
		_, err := o.CopyValue(ctx, secret.Ref{Op: envItem + "#" + key}, secret.Ref{File: src, Path: "stringData.x"})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s = %v", key, err)
		}
		noValue(t, "the refusal", err.Error())
	}
	for _, bad := range []string{envItem + "#", envItem + "#1KEY", envItem + "#A B"} {
		if _, err := secret.ParseRef(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if _, err := o.StoreValue(ctx, secret.Ref{Op: envKey}, "v"); err == nil || !strings.Contains(err.Error(), "source only") {
		t.Errorf("store into a line = %v", err)
	}
}
