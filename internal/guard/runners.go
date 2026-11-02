package guard

import (
	"path"
	"strings"
)

var (
	// runners run a command from their arguments: the argument after them is
	// a command the shell never sees at a command position, so the gate
	// rewrite cannot reach it. find runs one only after findActions.
	runners = wordSet("xargs parallel watch sudo doas su run0 systemd-run nsenter unshare chroot entr rush fd fdfind ssh")
	// findActions are find's actions that run a command from the words after them.
	findActions = wordSet("-exec -execdir -ok -okdir")
)

// runnerRefusal returns why the call runs a blocking devctl command where
// the gate rewrite cannot reach it, as an argument of a runner (xargs, find
// -exec, parallel, …) or in a backquote substitution, "" when it runs none:
// it starts with no gate before it, outside the lanes, the holds and the
// budget floor, with devctl's own bypass. The refusal names the call with
// the gate written in before devctl.
func (h Hook) runnerRefusal(cmd string, bg bool) string {
	how, at := runnerMerges(cmd)
	if len(at) == 0 {
		return ""
	}
	return "Refused: " + how + ", outside the gate (the lanes, the holds and the budget floor). Run each as its own " +
		"command line (a for loop gates every one), or with the gate written in:\n" + h.insertGate(cmd, at, bg, nil)
}

// ownedWhat names the blocking devctl commands in a refusal.
const ownedWhat = "a devctl pr merge, release promote, pr wait, release wait or rollout wait"

// runnerMerges returns the offsets of the blocking devctl commands in cmd
// that a runner runs from its arguments or a backquote substitution runs,
// with no gate before them, and how the first runs. A mention in a text
// command's argument (gh, git commit, echo, grep) or in a here-document is no
// run; a shell's -c string is hiddenMerges'.
func runnerMerges(cmd string) (how string, at []int) {
	sc := scanShell(cmd)
	segs := sc.segments()
	for _, m := range anyOwned.FindAllStringIndex(cmd, -1) {
		if gated.MatchString(cmd[:m[0]]) || inHeredoc(sc, m[0]) {
			continue
		}
		if sc.masked[m[0]] == cmd[m[0]] && inBackquotes(cmd[:m[0]]) {
			if how == "" {
				how = "a backquote substitution runs " + ownedWhat
			}
			at = append(at, m[0])
			continue
		}
		for i, sg := range segs {
			if m[0] < sg.start || m[0] >= sg.end {
				continue
			}
			if r := runnerBefore(sc.masked[sg.start:m[0]]); r != "" && !textStage(sc, segs, i) {
				if how == "" {
					how = r + " runs " + ownedWhat + " from its arguments"
				}
				at = append(at, m[0])
			}
			break
		}
	}
	return how, at
}

// inBackquotes reports whether before, the text up to a command, ends in an
// unescaped backquote and blanks: the command starts a `…` substitution.
func inBackquotes(before string) bool {
	s := strings.TrimRight(before, " \t")
	return strings.HasSuffix(s, "`") && !strings.HasSuffix(s, "\\`")
}

// runnerBefore names the runner among the unquoted words of a simple
// command before an argument (masked: its quotes blanked), "" for none: a
// text command the runner runs (find -exec grep, xargs echo) reads the
// argument as text.
func runnerBefore(masked string) string {
	runner, find := "", false
	for _, w := range strings.Fields(masked) {
		name := path.Base(w)
		switch {
		case runners[name]:
			runner = name
		case name == "find":
			find = true
		case find && findActions[w]:
			runner = "find " + w
		case runner != "" && textCommands[name]:
			runner = ""
		}
	}
	return runner
}
