//go:build unix

package secret

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A tool on no PATH but in a Go tool directory runs: the sandbox broker's
// calls get the service manager's PATH, without go install's directory.
func TestExecFindsAToolBeyondPATH(t *testing.T) {
	gobin := t.TempDir()
	if err := os.WriteFile(filepath.Join(gobin, "kindish"), []byte("#!/bin/sh\necho \"$@\"\n"), 0o755); err != nil { //nolint:gosec // an executable the test runs
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GOBIN", gobin)
	t.Setenv("GOPATH", "")
	t.Setenv("HOME", t.TempDir())
	out, err := Exec(context.Background(), "", nil, nil, "kindish", "get", "kubeconfig")
	if err != nil || strings.TrimSpace(string(out)) != "get kubeconfig" {
		t.Fatalf("Exec = %q, %v", out, err)
	}
	_, err = Exec(context.Background(), "", nil, nil, "absent")
	if err == nil || !strings.HasPrefix(err.Error(), "absent: ") || !strings.Contains(err.Error(), ", nor in ") || !strings.Contains(err.Error(), gobin) {
		t.Errorf("an absent tool: %v", err)
	}
}

// A tool's refusal reaches the caller whole, every line of it, with what a
// pattern redacts stripped: op's validator names the cause past its first
// line and past 160 characters.
func TestExecRelaysTheWholeRefusal(t *testing.T) {
	key := "AGE-SECRET-KEY-1" + strings.Repeat("Q7XZ", 14) + "K2"
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '[ERROR] unable to process line 1: Validation: (validateVaultItem failed to Validate), Couldn'\\''t validate the item: \"[ItemValidator] has found 1 errors, 0 warnings: \\nDetails:\\nErrors:\\n{1. %s: the field the validator refuses}\"\\n' '" + key + "' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "opish"), []byte(script), 0o755); err != nil { //nolint:gosec // an executable the test runs
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	_, err := Exec(context.Background(), "", nil, nil, "opish", "item", "create", "-")
	if err == nil || !strings.HasPrefix(err.Error(), "opish: exit 1 ([ERROR]") || !strings.Contains(err.Error(), "Details: Errors: {1. ") ||
		!strings.Contains(err.Error(), "the field the validator refuses}") {
		t.Errorf("a refusal = %v", err)
	}
	if err != nil && strings.Contains(err.Error(), key) {
		t.Error("the refusal carries a secret")
	}
}
