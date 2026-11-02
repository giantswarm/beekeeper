package guard

import (
	"path"
	"slices"
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
// A command ssh runs runs on another host, where this binary's path names no
// gate: its refusal names no gated form.
func (h Hook) runnerRefusal(cmd string, bg bool) string {
	how, at := runnerMerges(cmd)
	if len(at) == 0 {
		return ""
	}
	s := "Refused: " + how + ", outside the gate (the lanes, the holds and the budget floor). Run each as its own " +
		"command line (a for loop gates every one)"
	if strings.HasPrefix(how, "ssh ") {
		return s + " on this machine."
	}
	return s + ", or with the gate written in:\n" + h.insertGate(cmd, at, bg, nil)
}

// ownedWhat names the blocking devctl commands in a refusal.
const ownedWhat = "a devctl pr merge, release promote, pr wait, release wait or rollout wait"

// runnerMerges returns the offsets of the blocking devctl commands in cmd
// that the gate rewrite left ungated, and how the first runs. It fails
// closed: an unquoted one the rewrite did not gate is at no command position
// it knows, so whatever command it is an argument of (a runner, a wrapper,
// a multi-call binary) may run it. A quoted one runs when a runner runs its
// quoted argument (parallel 'devctl …', ssh host 'devctl …'). A here-document
// a shell reads as its program is a command line of its own, read the same
// way. A mention in a text command's argument (gh, git commit, echo, grep, a
// beekeeper note or report), in a here-document that is content or in a
// variable's value is no run; a shell's -c string and its arguments are
// hiddenMerges', an assignment heldMerges'.
func runnerMerges(cmd string) (how string, at []int) {
	sc := scanShell(cmd)
	segs := sc.segments()
	content := contentBodies(sc)
	for _, hd := range sc.heredocs {
		if inRanges(content, hd.start) {
			continue
		}
		bodyHow, in := runnerMerges(cmd[hd.start:hd.end])
		for _, o := range in {
			at = append(at, hd.start+o)
		}
		if how == "" {
			how = bodyHow
		}
	}
	for _, m := range anyOwned.FindAllStringIndex(cmd, -1) {
		if gated.MatchString(cmd[:m[0]]) || inHeredoc(sc, m[0]) {
			continue
		}
		bare := sc.masked[m[0]] == cmd[m[0]]
		if bare && inBackquotes(cmd[:m[0]]) {
			if how == "" {
				how = "a backquote substitution runs " + ownedWhat
			}
			at = append(at, m[0])
			continue
		}
		if _, ok := assignedValue(sc, segs, m[0]); ok {
			continue
		}
		for i, sg := range segs {
			if m[0] < sg.start || m[0] >= sg.end {
				continue
			}
			before, plain := cmd[sg.start:m[0]], sc.masked[sg.start:m[0]]
			if textStage(sc, segs, i) || shellCArg.MatchString(plain) || noteText(plain) {
				break
			}
			r, text := runnerBefore(plain)
			if text {
				break
			}
			if r == "" && bare {
				r = "`" + firstWord(before) + "`"
			}
			if r != "" {
				if how == "" {
					how = r + " runs " + ownedWhat + " from its arguments"
				}
				at = append(at, m[0])
			}
			break
		}
	}
	slices.Sort(at)
	return how, at
}

// noteCommands are beekeeper's commands whose arguments are text for a
// person or the supervisor: a note, a park's reason, a report, an answer.
var noteCommands = map[string]bool{"note": true, "agents": true, "answer": true}

// noteText reports whether a simple command's unquoted words (masked) are a
// beekeeper note, park, report or answer, whose arguments are text.
func noteText(masked string) bool {
	w := strings.Fields(masked)
	return len(w) > 1 && path.Base(w[0]) == "beekeeper" && noteCommands[w[1]]
}

// firstWord is the first word of a simple command's text, as written.
func firstWord(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return s
}

// inBackquotes reports whether before, the text up to a command, ends in an
// unescaped backquote and blanks: the command starts a `…` substitution.
func inBackquotes(before string) bool {
	s := strings.TrimRight(before, " \t")
	return strings.HasSuffix(s, "`") && !strings.HasSuffix(s, "\\`")
}

// runnerBefore names the runner among the unquoted words of a simple
// command before an argument (masked: its quotes blanked), "" for none; text
// reports a text command the runner runs (find -exec grep, xargs echo),
// which reads the argument as text.
func runnerBefore(masked string) (runner string, text bool) {
	find := false
	for _, w := range strings.Fields(masked) {
		name := path.Base(w)
		switch {
		case runners[name]:
			runner, text = name, false
		case name == "find":
			find = true
		case find && findActions[w]:
			runner, text = "find "+w, false
		case runner != "" && textCommands[name]:
			runner, text = "", true
		}
	}
	return runner, text
}
