package guard

import (
	"regexp"
	"slices"
	"strings"
)

// helpFlags are the arguments that make an invocation print its help or
// version and do nothing else.
var helpFlags = []string{"-h", "--help", "help", "-version", "--version"}

// subcommand is a plain word before the help flag (agentlab platform
// --help): a flag, a path or a package pattern is no subcommand.
var subcommand = regexp.MustCompile(`^[a-zA-Z][\w-]*$`)

// helpOnly reports whether the simple command whose program starts at offset
// at of cmd only prints its help or version: its arguments are subcommand
// words followed by one or more help flags, or go help, go version or go
// env. Such an invocation takes no build slot and is never refused for what
// the command would do without the flag.
func helpOnly(cmd string, at int) bool {
	words := invocation(cmd[at:])
	if len(words) < 2 {
		return false
	}
	if words[0] == "go" && slices.Contains([]string{"help", "version", "env"}, words[1]) {
		return true
	}
	args := words[1:]
	n := len(args)
	for n > 0 && slices.Contains(helpFlags, args[n-1]) {
		n--
	}
	if n == len(args) {
		return false
	}
	for _, a := range args[:n] {
		if !subcommand.MatchString(a) {
			return false
		}
	}
	return true
}

// notHelp is a commandMatch keep that passes over a match whose command only
// prints its help or version.
func notHelp(cmd string, m []int) bool { return !helpOnly(cmd, m[2]) }

// invocation is the words of the simple command s starts with, up to the
// operator ending it, its redirections left out.
func invocation(s string) []string {
	end := len(s)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(";|\n)`", c) >= 0 || c == '&' && (i == 0 || s[i-1] != '>') && (i+1 >= len(s) || s[i+1] != '>') {
			end = i
			break
		}
	}
	var words []string
	skip := false
	for _, w := range strings.Fields(s[:end]) {
		switch {
		case skip:
			skip = false
		case bareRedirect.MatchString(w):
			skip = true
		case !redirection.MatchString(w):
			words = append(words, w)
		}
	}
	return words
}
