package omp

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The variables the tests hand a key under.
const (
	keyVar      = "A_KEY"
	otherKeyVar = "B_KEY"
)

// A provider's apiKey in the models file is the name of the variable omp
// reads the key from, or a value the file then carries; a provider without
// one, a keyless one, one the file lacks and a missing file have none.
func TestProviderKey(t *testing.T) {
	const planted = "planted-key-9f2e1c0b" //nolint:gosec // a planted test value
	file := filepath.Join(t.TempDir(), "models.yml")
	//nolint:gosec // the planted value
	raw := `providers:
  spark:
    baseUrl: http://spark.test/v1
    api: openai-completions
    apiKey: SPARK_API_KEY
    models: [{id: m, name: M}]
  leaky:
    apiKey: ` + planted + `
  local:
    baseUrl: http://localhost:11434/v1
    auth: none
`
	if err := os.WriteFile(file, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	for provider, want := range map[string]string{"spark": "SPARK_API_KEY", "local": "", "absent": ""} {
		if got, err := ProviderKey(file, provider); err != nil || got != want {
			t.Errorf("ProviderKey(%s) = %q, %v; want %q", provider, got, err, want)
		}
	}
	got, err := ProviderKey(file, "leaky")
	if !errors.Is(err, ErrKeyValue) || got != "" {
		t.Fatalf("a value = %q, %v; want ErrKeyValue", got, err)
	}
	if strings.Contains(err.Error(), planted) {
		t.Errorf("the error carries the value: %v", err)
	}
	if got, err := ProviderKey(filepath.Join(t.TempDir(), "none.yml"), "spark"); err != nil || got != "" {
		t.Errorf("a missing file = %q, %v; want none", got, err)
	}
	if err := os.WriteFile(file, []byte("providers: [not, a, map]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ProviderKey(file, "spark"); err == nil {
		t.Error("an unreadable file passed")
	}
}

func TestVarName(t *testing.T) {
	for name, ok := range map[string]bool{"SPARK_API_KEY": true, "K": true, "dummy": false, "sk-planted": false, "_X": false, "": false, "a1B2c3": false} {
		if VarName.MatchString(name) != ok {
			t.Errorf("VarName(%q) = %v", name, !ok)
		}
	}
}

// The tool shell is written once, executable, and kept while its content is
// the shipped one.
func TestWriteToolShell(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteToolShell(dir)
	if err != nil {
		t.Fatal(err)
	}
	if path != ToolShellPath(dir) || filepath.Base(path) != "bash" {
		t.Errorf("path = %s", path)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("stat = %v, %v; want executable", fi, err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 3\n"), 0o700); err != nil { //nolint:gosec // the test's own file
		t.Fatal(err)
	}
	if _, err := WriteToolShell(dir); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != ToolShell { //nolint:gosec // the test's own file
		t.Error("a changed tool shell stayed")
	}
}
