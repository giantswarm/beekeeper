package guard

import (
	"runtime"
	"strings"
	"testing"
)

func TestCrossGoGuard(t *testing.T) {
	t.Setenv("GOOS", "")
	target := "windows"
	if runtime.GOOS == target {
		target = "linux"
	}
	for _, tt := range []struct {
		cmd  string
		deny bool
	}{
		{"GOOS=" + target + " go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...", true},
		{"env GOOS='" + target + "' go run ./cmd/tool", true},
		{"GOOS=" + target + " go test ./...", true},
		{"GOOS=" + target + " go test -exec='' ./...", true},
		{"GOOS=" + target + " go test -exec=runner -exec= ./...", true},
		{"GOOS=" + target + " go test ./... -args -exec=runner", true},
		{"bash -c 'GOOS=" + target + " go run ./cmd/tool'", true},
		{"GOOS=" + target + " bash -c 'go test ./...'", true},
		{"export GOOS=" + target + "; go run ./cmd/tool", true},
		{"GOOS=" + target + "; go test ./...", true},
		{"GOOS=" + target + " go test -exec runner ./...", false},
		{"GOOS=" + target + " go test -exec=runner ./...", false},
		{"GOOS=" + target + " go build ./...", false},
		{"GOOS=" + target + " govulncheck ./...", false},
		{"GOOS=" + runtime.GOOS + " go run ./cmd/tool", false},
		{"go run ./cmd/tool", false},
		{"echo 'GOOS=" + target + " go run ./cmd/tool'", false},
	} {
		t.Run(tt.cmd, func(t *testing.T) {
			d := decide(t, hook(), t.TempDir(), tt.cmd, nil)
			denied := d != nil && d.PermissionDecision == decisionDeny
			if denied != tt.deny {
				t.Fatalf("deny = %v, want %v: %+v", denied, tt.deny, d)
			}
			if tt.deny && (!strings.Contains(d.Reason, "go build") || !strings.Contains(d.Reason, "host binary")) {
				t.Fatalf("missing alternative: %s", d.Reason)
			}
		})
	}
}

func TestCrossGoInheritedEnvironment(t *testing.T) {
	target := "windows"
	if runtime.GOOS == target {
		target = "linux"
	}
	t.Setenv("GOOS", target)
	if crossGoRefusal("go run ./cmd/tool") == "" {
		t.Fatal("inherited cross-target GOOS allowed")
	}
	if r := crossGoRefusal("GOOS=" + runtime.GOOS + " go run ./cmd/tool"); r != "" {
		t.Fatal(r)
	}
}
