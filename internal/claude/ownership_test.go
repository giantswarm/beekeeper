package claude

import (
	"testing"

	"github.com/giantswarm/beekeeper/internal/proc"
)

func TestOwnCLIBackgroundWorker(t *testing.T) {
	for _, args := range [][]string{
		{"claude", "--session-id", "worker"},
		{"claude bg-spare"},
		{"claude", "bg-spare"},
		{"claude", "--bg-spare"},
	} {
		t.Run(args[len(args)-1], func(t *testing.T) {
			owner := &proc.Process{PID: 3, PPID: 2, Comm: "claude", Args: args}
			tab := proc.NewTable(
				&proc.Process{PID: 1, Comm: "claude", Args: []string{"claude daemon"}},
				&proc.Process{PID: 2, PPID: 1, Comm: "claude", Args: []string{"claude bg-pty-host"}},
				owner,
				&proc.Process{PID: 4, PPID: 3, Comm: "bash", Args: []string{"bash"}},
				&proc.Process{PID: 5, PPID: 4, Comm: "beekeeper", Args: []string{"beekeeper", "agents", "idle", "--done"}},
			)
			if got := OwnCLI(tab, 5); got != owner {
				t.Fatalf("OwnCLI = %v, want worker at PID 3", got)
			}
			if got := OwnCLI(tab, 2); got != nil {
				t.Fatalf("OwnCLI of daemon helper = %v, want nil", got)
			}
		})
	}
}
