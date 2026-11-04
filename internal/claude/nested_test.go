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

// A tool command belongs to its nearest session CLI, including a worker
// the daemon handed its spare. The daemon and terminal host are no owners.
func TestOwnCLI(t *testing.T) {
	var (
		daemon  = []string{cliComm, "daemon"}
		ptyHost = []string{cliComm + " bg-pty-host"}
		spare   = []string{cliComm + " bg-spare"}
		shell   = []string{"bash"}
		caller  = []string{"beekeeper"}
	)
	for _, tc := range []struct {
		name string
		argv [][]string
		want int
	}{
		{"case 0: desktop session", [][]string{{desktopEntrypoint}, {cliComm}, shell, caller}, 2},
		{"case 1: worker in a spare", [][]string{daemon, ptyHost, spare, shell, caller}, 3},
		{"case 2: spare before retitling", [][]string{daemon, {cliComm, "--bg-spare", "s"}, shell, caller}, 2},
		{"case 3: nested session inside a spare", [][]string{spare, shell, {cliComm, "-p", "task"}, shell, caller}, 3},
		{"case 4: helper processes only", [][]string{daemon, ptyHost, shell, caller}, 0},
		{"case 5: no session", [][]string{shell, caller}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tab := chain(tc.argv...)
			got := OwnCLI(tab, len(tc.argv))
			if tc.want == 0 {
				if got != nil {
					t.Fatalf("OwnCLI = %+v, want no owner", got)
				}
			} else if got == nil || got.PID != tc.want {
				t.Fatalf("OwnCLI = %+v, want PID %d", got, tc.want)
			}
		})
	}
}
