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
