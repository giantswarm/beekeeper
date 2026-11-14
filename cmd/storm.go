package cmd

import (
	"github.com/giantswarm/beekeeper/internal/proc"
)

// owner is the name of the session whose CLI is pid or one of its
// ancestors; "" outside every session.
func owner(t *proc.Table, pid int, owners map[int]string) string {
	if s := owners[pid]; s != "" {
		return s
	}
	for _, a := range t.Ancestors(pid) {
		if s := owners[a.PID]; s != "" {
			return s
		}
	}
	return ""
}

// display is a command line safe to print: the program's base name, the
// words before the first flag (its subcommands), and the flags without
// their values. A word after a flag may be the flag's value, a password
// among them, and is left out, as is a word that may carry a credential
// (a URL's user info, a key=value) and what follows a short option's
// letter (-psecret).
func display(args []string) string {
	s, _ := masked(args)
	return s
}

// masked is display's command line and whether it left anything out.
func masked(args []string) (string, bool) {
	command, cut := proc.MaskedCommand(args)
	return truncate(command, 80), cut
}
