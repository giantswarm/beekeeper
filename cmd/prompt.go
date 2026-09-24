package cmd

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// liveCommands are the commands that read what the prompt leaves out: the
// values that go stale within minutes.
var liveCommands = [][2]string{
	{"beekeeper sessions", "the running sessions, what each is on and runs, their memory"},
	{"beekeeper snapshot", "the machine's memory, swap, load and OOM kills, and what changed"},
	{"beekeeper watch", "one line per change, silent otherwise: the source of a Monitor"},
	{"beekeeper lanes", "the merge lanes now: the running merge, the settling rollout, who is next"},
	{"beekeeper lease list", "the leases and grant queues now"},
	{"beekeeper alerts snapshot", "the installations' current alerts"},
	{"beekeeper budget", "the GitHub budget and who draws on it"},
	{"beekeeper log", "the latest events"},
	{"beekeeper self-update --check", "whether a newer beekeeper is released"},
	{"gh issue view / gh pr view <n> --repo <owner/repo>", "an issue's or a pull request's state"},
}

// printPrompt prints the successor's session prompt: the configured
// instructions, the scope and the pending state, then the commands that read
// the live values. It carries no standing rule and no live value (a version,
// a memory figure, a pull request's state): those belong to the instructions
// and to the commands, and a prompt that restates them goes stale.
func (a *app) printPrompt(ctx context.Context, v *view, l *leaseList, al *alertsView) error {
	intro, err := a.instructions(v.st)
	if err != nil {
		return err
	}
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format+"\n", args...) }
	p("%s\n", intro)
	p("## Scope\n\n%s\n", a.scope(al))
	p("## Pending state, %s\n", a.stamp(a.now))
	a.promptNotes(p, v.st.Notes)
	a.promptTimers(p, v.st.Timers)
	a.promptLeases(p, l)
	a.promptHolds(p, a.activeHolds(v.st))
	a.promptLanes(p, a.laneViews(v.st))
	a.promptRecords(p, v.st.Records, v.raw)
	a.promptAgents(p, v.st.Agents)
	a.promptAlerts(p, al)
	p("## Live values\n\nRead them when you need them; this prompt holds none:\n")
	for _, c := range liveCommands {
		p("- `%s`: %s", c[0], c[1])
	}
	return ctx.Err()
}

// instructions opens the prompt: the configured skill or file.
func (a *app) instructions(st *state.State) (string, error) {
	sup := a.cfg.Supervisor
	from := ""
	if st.Supervisor != nil {
		from = fmt.Sprintf(" from %q", st.Supervisor.Name)
	}
	switch {
	case sup.Instructions != "":
		raw, err := os.ReadFile(sup.Instructions)
		if err != nil {
			return "", fmt.Errorf("supervisor.instructions: %w", err)
		}
		return strings.TrimSpace(string(raw)), nil
	case sup.Skill != "":
		return fmt.Sprintf("Run /%s: you take over the supervisor's watch%s.", sup.Skill, from), nil
	}
	return fmt.Sprintf("You take over the supervisor's watch%s. (No instructions are configured: set supervisor.skill or supervisor.instructions.)", from), nil
}

// scope is the configured scope, or what the configuration watches.
func (a *app) scope(al *alertsView) string {
	if a.cfg.Supervisor.Scope != "" {
		return strings.TrimSpace(a.cfg.Supervisor.Scope)
	}
	parts := []string{"the Claude Code sessions on this machine"}
	parts = append(parts, "the leases of "+strings.Join(a.cfg.Leasable(), ", "))
	if len(a.cfg.Lanes) > 0 {
		lanes := make([]string, len(a.cfg.Lanes))
		for i, l := range a.cfg.Lanes {
			lanes[i] = l.Name
		}
		parts = append(parts, "the merge lanes "+strings.Join(lanes, ", "))
	}
	if len(al.Targets) > 0 {
		names := make([]string, len(al.Targets))
		for i, t := range al.Targets {
			names[i] = t.Name
		}
		parts = append(parts, "the alerts of "+strings.Join(names, ", "))
	}
	return "Supervise " + strings.Join(parts, "; ") + "."
}

// stamp is a time with its zone: the successor may read it in another one.
func (a *app) stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return clock(a.now, t) + " " + t.Local().Format("MST")
}

type printer func(format string, args ...any)

func section(p printer, title string, empty bool, none string) bool {
	p("### %s\n", title)
	if empty {
		p("%s\n", none)
	}
	return !empty
}

func (a *app) promptNotes(p printer, notes []state.Note) {
	if !section(p, "Decisions and notes", len(notes) == 0, "None open.") {
		return
	}
	for _, n := range notes {
		var tags []string
		if n.For != "" {
			tags = append(tags, "for "+n.For)
		}
		if !n.Due.IsZero() {
			due := "due " + a.stamp(n.Due)
			if !n.Due.After(a.now) {
				due += ", overdue"
			}
			tags = append(tags, due)
		}
		line := fmt.Sprintf("- #%d", n.ID)
		if len(tags) > 0 {
			line += " (" + strings.Join(tags, ", ") + ")"
		}
		line += ": " + oneLine(n.Text)
		if n.Default != "" {
			line += "; if unanswered: " + oneLine(n.Default)
		}
		p("%s", line)
	}
	p("")
}

func (a *app) promptTimers(p printer, timers []state.Timer) {
	if !section(p, "Timers", len(timers) == 0, "None open.") {
		return
	}
	for _, t := range timers {
		due := a.stamp(t.Due)
		if !t.Due.After(a.now) {
			due += ", due"
		}
		p("- #%d (at %s, set by %q): %s", t.ID, due, t.By.Name, oneLine(t.What))
	}
	p("")
}

func (a *app) promptLeases(p printer, l *leaseList) {
	section(p, "Leases and grants", false, "")
	for _, h := range l.Held {
		gone := ""
		if h.State == holderGone {
			gone = ", its session has ended"
		}
		p("- %s held by %q since %s%s: %s", h.Env, h.Name, a.stamp(h.SinceTime()), gone, oneLine(h.Purpose))
	}
	if len(l.Free) > 0 {
		p("- free: %s", strings.Join(l.Free, ", "))
	}
	for _, r := range a.cfg.Leasable() {
		q := l.Queues[r]
		if len(q) == 0 {
			continue
		}
		names := make([]string, len(q))
		for i, g := range q {
			names[i] = fmt.Sprintf("%q (granted %s by %q)", g.To.Name, a.stamp(g.At), g.By.Name)
		}
		p("- %s is granted to %s", r, strings.Join(names, ", then "))
	}
	p("")
}

func (a *app) promptHolds(p printer, holds []state.Hold) {
	if !section(p, "Holds", len(holds) == 0, "Nothing is held.") {
		return
	}
	for _, h := range holds {
		until := "until lifted"
		if !h.Until.IsZero() {
			until = "until " + a.stamp(h.Until)
		}
		window := ""
		if h.Tool != "" {
			window = fmt.Sprintf(", the release window of %s (it closes once %s reports its new release)", h.Tool, h.Tool)
		}
		p("- %s, %s, by %q since %s%s: %s", holdTarget(h), until, h.By.Name, a.stamp(h.At), window, oneLine(h.Reason))
	}
	p("")
}

func (a *app) promptLanes(p printer, lanes []laneView) {
	if !section(p, "Merge lanes", len(lanes) == 0, "No lane is configured and nothing merges.") {
		return
	}
	for _, v := range lanes {
		var parts []string
		if v.Running != nil {
			parts = append(parts, fmt.Sprintf("running %s by %q since %s", v.Running.Key(), v.Running.By.Name, a.stamp(v.Running.Started)))
		}
		for _, m := range v.AllSettling {
			how := "until its release rolls"
			if m.Outside {
				how = "merged outside the gate, until its HelmReleases are Ready"
			}
			parts = append(parts, fmt.Sprintf("settling %s %s", m.Key(), how))
		}
		if v.Hold != nil {
			parts = append(parts, "held (see Holds)")
		}
		for i, m := range v.Waiting {
			parts = append(parts, fmt.Sprintf("%d. %s by %q, queued %s", i+1, m.Key(), m.By.Name, a.stamp(m.Joined)))
		}
		if len(parts) == 0 {
			parts = append(parts, "free")
		}
		where := ""
		if v.Installation != "" {
			where = " (" + v.Installation + ")"
		}
		p("- %s%s: %s", v.Name, where, strings.Join(parts, "; "))
	}
	p("")
}

func (a *app) promptRecords(p printer, records []state.Record, sessions []*claude.Session) {
	if !section(p, "Session records", len(records) == 0, "None.") {
		return
	}
	for _, r := range records {
		_, live := claude.Live(sessions, r.Session)
		if !live || !r.Ended.IsZero() {
			p("- %q has ended; it %s: re-query %s", r.Session.Name, oneLine(recordText(r)), r.Issue)
			continue
		}
		p("- %q %s", r.Session.Name, oneLine(recordText(r)))
	}
	p("")
}

func (a *app) promptAgents(p printer, agents []state.Agent) {
	if !section(p, "Agents", len(agents) == 0, "None registered.") {
		return
	}
	for _, ag := range agents {
		if ag.Task != "" {
			p("- %q on %s since %s", ag.Name, oneLine(ag.Task), a.stamp(ag.AssignedAt))
			continue
		}
		p("- %q idle since %s", ag.Name, a.stamp(cmp.Or(ag.IdleSince, ag.Registered)))
	}
	p("")
}

func (a *app) promptAlerts(p printer, al *alertsView) {
	section(p, "Alerts", false, "")
	if len(al.Targets) == 0 {
		p("No installation is watched (alerts.installations is empty and no leased one has a kube context).")
	}
	for _, t := range al.Targets {
		p("- %s (%s; context %s): %s", t.Name, t.Why, cmp.Or(t.Context, "none"), baselineText(al.Baselines[t.Name]))
	}
	p("\nThe baseline is %s: the next beekeeper watch compares against it and reads every %s.", alerts.NewStore(a.cfg.StateDir).Path(), al.Every)
	p("Ignored alert names: %s.", strings.Join(al.Ignore, ", "))
	if al.Team != "" {
		p("Marked team: %s; more than %d changes of one alertname are one line.", al.Team, al.Collapse)
	}
	p("")
}

// baselineText is an installation's baseline: its known alerts by name.
func baselineText(b *alerts.Installation) string {
	switch {
	case b == nil:
		return "no baseline yet"
	case b.Alerts == nil:
		return "unreachable, never read"
	}
	count := map[string]int{}
	for _, al := range b.Alerts {
		count[al.Alertname]++
	}
	names := slices.Sorted(maps.Keys(count))
	for i, n := range names {
		if count[n] > 1 {
			names[i] = fmt.Sprintf("%s ×%d", n, count[n])
		}
	}
	s := fmt.Sprintf("%d known", len(b.Alerts))
	if !b.Reachable {
		s = fmt.Sprintf("unreachable, %d known when last read", len(b.Alerts))
	}
	if len(names) > 0 {
		s += ": " + strings.Join(names, ", ")
	}
	return s
}

// oneLine folds a text onto one line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
