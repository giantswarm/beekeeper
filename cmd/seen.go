package cmd

import (
	"bytes"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// fact is one thing a reading command says: Key names it, Line is how it
// is printed, and Sig is what makes it news to a caller who has read it:
// the Line without the figures that move on every read (an age, a
// countdown, a memory size).
type fact struct{ Key, Sig, Line string }

// readMark is what a caller last read from a command: its facts' Sigs.
type readMark struct {
	At    time.Time         `json:"at"`
	Facts map[string]string `json:"facts"`
}

// fullFlag adds --full to a reading command.
func fullFlag(c *cobra.Command, full *bool) {
	c.Flags().BoolVar(full, "full", false, "print everything, not only what changed since your last read")
}

// delta prints what the caller has not read yet: the facts that are new or
// changed since its last read of cmd, and the keys of those gone, or one
// "no change" line. The whole text (printFull) is for a first read, --full
// and a caller outside a Claude session (a person at a terminal). Every
// read moves the caller's mark to the facts it now has.
func (a *app) delta(cmd string, full bool, facts []fact, printFull func()) error {
	me, err := a.caller()
	if err != nil {
		printFull()
		return nil
	}
	file := "seen." + cmd + "." + fileKey(me) + ".json"
	var prev readMark
	found, err := a.store.ReadFile(file, &prev)
	if err != nil {
		return err
	}
	cur := readMark{At: a.now.UTC(), Facts: make(map[string]string, len(facts))}
	for _, f := range facts {
		cur.Facts[f.Key] = f.Sig
	}
	if err := a.store.WriteFile(file, cur); err != nil {
		return err
	}
	if !found || full {
		printFull()
		return nil
	}
	var lines []string
	for _, f := range facts {
		if sig, ok := prev.Facts[f.Key]; !ok || sig != f.Sig {
			lines = append(lines, f.Line)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(prev.Facts)) {
		if _, ok := cur.Facts[k]; !ok {
			lines = append(lines, "gone: "+k)
		}
	}
	if len(lines) == 0 {
		_, err = fmt.Fprintf(a.out, "no change since %s\n", clock(a.now, prev.At))
		return err
	}
	_, _ = fmt.Fprintf(a.out, "since %s:\n", clock(a.now, prev.At))
	_, err = fmt.Fprintln(a.out, strings.Join(lines, "\n"))
	return err
}

// capture returns what print writes to the output.
func (a *app) capture(print func()) string {
	out := a.out
	defer func() { a.out = out }()
	var b bytes.Buffer
	a.out = &b
	print()
	return b.String()
}

// moving are the figures that change on every read: durations (5m, 1h2m,
// 3.5s, with the words around them) and token counts (312k).
var moving = regexp.MustCompile(`\b\d+(\.\d+)?((ms|h|m|s)(\d+(\.\d+)?(m|s))*( left| ago)?|k)\b`)

// textFacts turns a section's text into one fact per line, the table
// padding and a table's header left out; a moving figure is no news.
func textFacts(section, text string) []fact {
	var facts []fact
	for i, l := range strings.Split(text, "\n") {
		l = strings.Join(strings.Fields(l), " ")
		if l == "" || i == 0 && l == strings.ToUpper(l) && strings.Contains(l, " ") {
			continue
		}
		sig := moving.ReplaceAllString(l, "")
		facts = append(facts, fact{Key: section + ": " + sig, Sig: sig, Line: section + ": " + l})
	}
	return facts
}

// sessionFacts are the running sessions, their records and overlaps: a
// session is news when what it is on, runs or holds changes, not when its
// age, memory, context or last hour move.
func (a *app) sessionFacts(v *view) []fact {
	var facts []fact
	for _, s := range v.Sessions {
		role := roleText(s, s.Role)
		cmds := make([]string, 0, len(s.Commands))
		for _, c := range s.Commands {
			cmds = append(cmds, commandName(c.Args))
		}
		key := fmt.Sprintf("session %q", s.Name)
		facts = append(facts, fact{
			Key:  key,
			Sig:  strings.Join([]string{on(s), strings.Join(cmds, "; "), role}, "|"),
			Line: fmt.Sprintf("%s on %s, runs %s, ctx %s, %s", key, on(s), running(s.Commands), contextText(s.Metrics), role),
		})
	}
	facts = append(facts, textFacts("record", a.capture(func() { a.printRecords(v.st.Records, v.raw) }))...)
	for _, o := range v.Overlaps {
		l := "overlap " + o.Key + ": " + strings.Join(o.Sessions, ", ")
		facts = append(facts, fact{Key: "overlap " + o.Key, Sig: l, Line: l})
	}
	return facts
}

// agentFacts are the registered agents: an agent is news when its task or
// its reachability changes, not when its reply window counts down.
func (a *app) agentFacts(views []agentView) []fact {
	facts := make([]fact, 0, len(views))
	for _, v := range views {
		task, since := "(idle)", v.IdleSince
		if v.Task != "" {
			task, since = v.Task, v.AssignedAt
		}
		reach, _, _ := strings.Cut(v.Reachable, ",")
		key := fmt.Sprintf("agent %q", v.Name)
		facts = append(facts, fact{
			Key:  key,
			Sig:  strings.Join([]string{task, clock(a.now, since), reach}, "|"),
			Line: fmt.Sprintf("%s %s since %s, %s", key, task, clock(a.now, since), v.Reachable),
		})
	}
	return facts
}
