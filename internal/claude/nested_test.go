package claude

import (
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/proc"
)

// chain is a process table holding one parent chain, outermost first: each
// entry is a process's argv, named by its first word, its PID its position
// plus one.
func chain(argvs ...[]string) *proc.Table {
	t := &proc.Table{ByPID: map[int]*proc.Process{}}
	for i, argv := range argvs {
		t.ByPID[i+1] = &proc.Process{PID: i + 1, PPID: i, Comm: strings.Fields(argv[0])[0], Args: argv}
	}
	return t
}

// A command a session's tool runs is no nested one, whether the desktop or
// the daemon runs the session; a command a `claude -p` of that tool runs is.
func TestNested(t *testing.T) {
	var (
		desktop  = []string{desktopEntrypoint}
		daemon   = []string{cliComm, "daemon"}
		ptyHost  = []string{cliComm + " bg-pty-host"}
		spare    = []string{cliComm + " bg-spare"}
		cli      = []string{cliComm}
		nested   = []string{cliComm, "-p", "--", "register as X"}
		shell    = []string{"zsh"}
		register = []string{"beekeeper", "agents", "register"}
	)
	for _, c := range []struct {
		name string
		t    *proc.Table
		want bool
	}{
		{"desktop session", chain(desktop, cli, shell, register), false},
		{"background worker", chain(daemon, ptyHost, cli, shell, register), false},
		{"worker in a spare", chain(daemon, ptyHost, spare, shell, register), false},
		{"claude -p in a desktop session's tool", chain(desktop, cli, shell, nested, shell, register), true},
		{"claude -p in a background worker's tool", chain(daemon, ptyHost, spare, shell, nested, shell, register), true},
	} {
		if got := Nested(c.t, len(c.t.ByPID)); got != c.want {
			t.Errorf("%s: Nested = %v, want %v", c.name, got, c.want)
		}
	}
}
