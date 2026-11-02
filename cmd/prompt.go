package cmd

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// roleSlash is the bare slash command that opens a prompt of rl as the
// person would type it ("/guide "), "" when rl runs no skill. A role
// skill carries disable-model-invocation, so the Skill tool refuses it: only
// a prompt that starts with its slash command loads it.
func roleSlash(cfg config.Role) string {
	if cfg.Instructions != "" || cfg.Skill == "" {
		return ""
	}
	return "/" + cfg.Skill + " "
}

// roleInstructions opens rl's successor prompt: the configured skill's bare
// slash command or the file.
func (a *app) roleInstructions(rl role, r state.Role) (string, error) {
	cfg := rl.cfg(a.cfg)
	from := ""
	if r.Holder != nil {
		from = fmt.Sprintf(" from %q", r.Holder.Name)
	}
	switch {
	case cfg.Instructions != "":
		raw, err := os.ReadFile(cfg.Instructions)
		if err != nil {
			return "", fmt.Errorf("%s.instructions: %w", rl.name, err)
		}
		return strings.TrimSpace(string(raw)), nil
	case cfg.Skill != "":
		return fmt.Sprintf("%syou take over %s%s.", roleSlash(cfg), rl.duty, from), nil
	}
	return fmt.Sprintf("You take over %s%s. (No instructions are configured: set %s.skill or %s.instructions.)", rl.duty, from, rl.name, rl.name), nil
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

func (a *app) promptRecords(p printer, st *state.State, sessions []*claude.Session) {
	records, dropped := a.liveRecords(st, st.Records, sessions)
	if !section(p, "Session records", len(st.Records) == 0, "None.") {
		return
	}
	parked := parkedRows(st, records, sessions, a.now)
	for _, r := range records {
		if i := slices.IndexFunc(parked, func(pr parkedRow) bool { return pr.Session.Is(r.Session) }); i >= 0 {
			p("- %q %s", r.Session.Name, oneLine(parked[i].text()))
			continue
		}
		_, live := claude.Live(sessions, r.Session)
		if !live || !r.Ended.IsZero() {
			p("- %q has ended; it %s: re-query %s", r.Session.Name, oneLine(recordText(r)), r.Issue)
			continue
		}
		p("- %q %s", r.Session.Name, oneLine(recordText(r)))
	}
	if dropped > 0 {
		p("- %s", droppedText(dropped))
	}
	p("")
}

func (a *app) promptAgents(p printer, agents []state.Agent, sessions []*claude.Session) {
	if !section(p, "Agents", len(agents) == 0, "None registered.") {
		return
	}
	for _, ag := range agents {
		if ag.Task != "" && ag.Park != nil {
			p("- %q on %s since %s, %s", ag.Name, oneLine(ag.Task), a.stamp(ag.AssignedAt), oneLine(parkedText(ag.Park, a.now)))
			continue
		}
		if _, live := claude.Live(sessions, ag.Party); ag.Task != "" && !live {
			p("- %q on %s since %s; its CLI is not running (stopped by a reboot or a crash): resume it with `%s`", ag.Name, oneLine(ag.Task), a.stamp(ag.AssignedAt), resumeHint(ag))
			continue
		}
		if ag.Task != "" {
			p("- %q on %s since %s", ag.Name, oneLine(ag.Task), a.stamp(ag.AssignedAt))
			continue
		}
		p("- %q idle since %s", ag.Name, a.stamp(cmp.Or(ag.IdleSince, ag.Registered)))
	}
	p("")
}

// promptCapacity says the roster's count against the target; the headroom
// is a live value, read with beekeeper capacity.
func (a *app) promptCapacity(p printer, st *state.State, sessions []*claude.Session) {
	section(p, "Capacity", false, "")
	k, u := a.cachedUsageCapacity(a.liveCapacity())
	line := countAgents(st, sessions, a.now).line(k)
	if u != nil && u.Error == "" {
		line += "; " + u.capacitySuffix(a.now)
	}
	p("%s; `beekeeper capacity` says whether a start fits.\n", line)
}

// lastRead is " at <clock>" of an installation's last reading, if known.
func lastRead(now time.Time, b *alerts.Installation) string {
	if b.Seen.IsZero() {
		return ""
	}
	return " at " + clock(now, b.Seen)
}

// oneLine folds a text onto one line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// escapedLine keeps a report on one physical output line without losing
// whitespace in its event. Backslashes are escaped so the text is reversible.
func escapedLine(s string) string {
	return strings.NewReplacer("\\", "\\\\", "\r", "\\r", "\n", "\\n").Replace(s)
}
