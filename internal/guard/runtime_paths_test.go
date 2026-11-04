package guard

import (
	"testing"
	"time"
)

func TestRunTimeoutLinterPaths(t *testing.T) {
	for _, argv := range [][]string{
		{"/usr/local/bin/golangci-lint", executionRunCommand},
		{"./bin/golangci-lint", executionRunCommand},
		{"sh", "-c", "cd /tmp && /usr/local/bin/golangci-lint run"},
	} {
		t.Run(CommandHead(argv), func(t *testing.T) {
			if got := RunTimeout(Options{}, argv); got != 5*time.Minute {
				t.Fatalf("linter deadline = %s, want 5m", got)
			}
			if got := RunTimeout(Options{Class: ClassLab}, argv); got != 0 {
				t.Fatalf("slotless deadline = %s, want unbounded", got)
			}
			if got := RunTimeout(Options{Timeout: time.Hour}, argv); got != time.Hour {
				t.Fatalf("explicit deadline = %s, want 1h", got)
			}
		})
	}
}

func TestRunTimeoutLinterPathArguments(t *testing.T) {
	for _, argv := range [][]string{
		{"sh", "-c", "echo /usr/local/bin/golangci-lint"},
		{"/usr/local/bin/golangci-lint", "--help"},
		{"sh", "-c", "printf '%s' '/usr/local/bin/golangci-lint run'"},
	} {
		if got := RunTimeout(Options{}, argv); got != 30*time.Minute {
			t.Errorf("%v: deadline = %s, want build default", argv, got)
		}
	}
}
