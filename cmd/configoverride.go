package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/watch"
)

// The events of a configuration override, from its set to its end.
const (
	overrideSetVerb      = "config.override"
	overrideEndVerb      = "config.override.end"
	overrideRestoreVerb  = "config.restored"
	overrideKeepVerb     = "config.kept"
	overrideDecisionVerb = "config.override.decision"
	overrideFailVerb     = "config.override.failed"
)

// overrideRetake is how long a restore may hold an ended override before
// another tick takes it again: the one that took it died before it wrote.
const overrideRetake = 10 * time.Minute

// overrideAbsent is how a line shows a key the file did not have.
const overrideAbsent = "(absent)"

// overrideSpec is what config set --until was given.
type overrideSpec struct {
	until, reason string
}

// overrideEnd parses --until: an issue or pull request (owner/repo#n or its
// URL) whose closing ends the override, or a time or duration from now.
func overrideEnd(now time.Time, s string) (ref string, until time.Time, err error) {
	if r, ok := parseRef(s); ok {
		return refName(r), time.Time{}, nil
	}
	until, err = untilTime(now, s)
	switch {
	case err != nil:
		return "", time.Time{}, usageErr("--until %q: a time (22:55, 2h) or an issue or pull request (owner/repo#n)", s)
	case !until.After(now):
		return "", time.Time{}, usageErr("--until %q is not in the future", s)
	}
	return "", until.UTC(), nil
}

// overrideEndText says when o ends: "until 18:00 CET" or "until owner/repo#n
// closes".
func overrideEndText(now time.Time, o state.Override) string {
	if o.Ref != "" {
		return "until " + o.Ref + " closes"
	}
	return "until " + clockZone(now, o.Until)
}

// overrideKeys names o's keys with the values it set and the values before:
// "github.floor -1 (was 500)".
func overrideKeys(o state.Override) string {
	parts := make([]string, 0, len(o.Keys))
	for _, k := range o.Keys {
		parts = append(parts, fmt.Sprintf("%s %s (was %s)", k.Key, yamlLine(k.Value), previousText(k)))
	}
	return strings.Join(parts, ", ")
}

func previousText(k state.OverrideKey) string {
	if k.Absent {
		return overrideAbsent
	}
	return yamlLine(k.Previous)
}

// yamlLine is a YAML value on one line, for a line or an event.
func yamlLine(v string) string { return truncate(oneLine(v), 80) }

// setOverride writes settings into the configuration file at path as an
// override that ends at spec.until, recorded with each key's value before:
// a key another override in effect holds is taken over with that one's value
// before, so the end restores the value before both. A state write that
// fails puts the values before back.
func (a *app) setOverride(path string, settings []config.Setting, spec overrideSpec) error {
	ref, until, err := overrideEnd(time.Now(), spec.until)
	if err != nil {
		return err
	}
	if strings.TrimSpace(spec.reason) == "" {
		return usageErr(`--until needs --reason "<why the override is in effect>"`)
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	keys := make([]state.OverrideKey, 0, len(settings))
	for _, s := range settings {
		if s.Key == "stateDir" || strings.HasPrefix(s.Key, "stateDir.") {
			return refused("stateDir cannot be overridden temporarily: restoring it would switch state history")
		}
		prev, found, err := config.Get(path, s.Key)
		if err != nil {
			return err
		}
		keys = append(keys, state.OverrideKey{Key: s.Key, Value: s.Value, Previous: prev, Absent: !found})
	}
	if _, err := config.Set(path, settings); err != nil {
		if errors.Is(err, config.ErrInvalid) {
			return refused("%v", err)
		}
		return err
	}
	if err := a.load(); err != nil {
		return a.undoOverride(path, keys, err)
	}
	me, err := a.caller()
	if err != nil {
		return a.undoOverride(path, keys, err)
	}
	o := state.Override{Path: path, Reason: strings.TrimSpace(spec.reason), Ref: ref, Until: until, By: me, At: a.now.UTC()}
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		o.Keys = slices.Clone(keys)
		evs := takeOverKeys(st, &o, me)
		st.NextOverride++
		o.ID = st.NextOverride
		st.Overrides = append(st.Overrides, o)
		e := event(me, overrideSetVerb, "#%d %s %s: %s (%s)", o.ID, overrideKeys(o), overrideEndText(a.now, o), o.Reason, o.Path)
		return append(evs, about(e, o.Ref)), nil
	})
	if err != nil {
		return a.undoOverride(path, keys, err)
	}
	_, err = fmt.Fprintf(a.out, "override #%d: %s, %s\n", o.ID, overrideKeys(o), overrideEndText(a.now, o))
	return err
}

// takeOverKeys moves o's keys out of the overrides of st in effect on the
// same file, o taking each one's value before; an override left without
// keys ends. It returns the events of what it took.
func takeOverKeys(st *state.State, o *state.Override, me state.Party) []state.Event {
	var evs []state.Event
	for i := range o.Keys {
		k := &o.Keys[i]
		for j := range st.Overrides {
			prior := &st.Overrides[j]
			if prior.Path != o.Path || !prior.Ended.IsZero() || prior.Decision != "" {
				continue
			}
			n := slices.IndexFunc(prior.Keys, func(pk state.OverrideKey) bool { return pk.Key == k.Key })
			if n < 0 {
				continue
			}
			k.Previous, k.Absent = prior.Keys[n].Previous, prior.Keys[n].Absent
			prior.Keys = slices.Delete(prior.Keys, n, n+1)
			evs = append(evs, event(me, overrideEndVerb, "#%d %s taken over by the next override", prior.ID, k.Key))
		}
	}
	st.Overrides = slices.DeleteFunc(st.Overrides, func(p state.Override) bool { return len(p.Keys) == 0 })
	return evs
}

// undoOverride puts the values before keys back after an override could not
// be recorded, and returns cause.
func (a *app) undoOverride(path string, keys []state.OverrideKey, cause error) error {
	set, unset := restoreChange(keys)
	if _, err := config.Change(path, set, unset); err != nil {
		return fmt.Errorf("%w; the override stays written, its values before not restored: %w", cause, err)
	}
	return fmt.Errorf("%w; the values before are restored", cause)
}

// restoreChange is the change that puts the values before keys back.
func restoreChange(keys []state.OverrideKey) ([]config.Setting, []string) {
	var set []config.Setting
	var unset []string
	for _, k := range keys {
		if k.Absent {
			unset = append(unset, k.Key)
		} else {
			set = append(set, config.Setting{Key: k.Key, Value: k.Previous})
		}
	}
	return set, unset
}

// restoreOverride puts o's values before back into its file. A key whose
// value changed since the override set it is a decision for the person:
// without force nothing is written and the decision says what changed. A key
// back at its value before already is restored. A restore whose result would
// not load is a decision too.
func restoreOverride(o state.Override, force bool) (decision string, err error) {
	var pending []state.OverrideKey
	var changed []string
	for _, k := range o.Keys {
		cur, found, err := config.Get(o.Path, k.Key)
		if err != nil {
			return "", err
		}
		if k.Absent && !found || !k.Absent && found && config.SameValue(cur, k.Previous) {
			continue
		}
		if !found || !config.SameValue(cur, k.Value) {
			now := overrideAbsent
			if found {
				now = yamlLine(cur)
			}
			changed = append(changed, fmt.Sprintf("%s is %s, the override set %s", k.Key, now, yamlLine(k.Value)))
		}
		pending = append(pending, k)
	}
	if len(changed) > 0 && !force {
		return strings.Join(changed, "; "), nil
	}
	if len(pending) == 0 {
		return "", nil
	}
	set, unset := restoreChange(pending)
	if _, err := config.Change(o.Path, set, unset); err != nil {
		if errors.Is(err, config.ErrInvalid) {
			return "the values before do not load: " + err.Error(), nil
		}
		return "", err
	}
	return "", nil
}

// overridesDue are the overrides of st whose end arrived at now: the time
// came, or found says the issue or pull request closed; and those ended
// over overrideRetake ago that no restore finished.
func overridesDue(st *state.State, found map[int]checkResult, now time.Time) map[int]string {
	due := map[int]string{}
	for _, o := range st.Overrides {
		switch {
		case o.Decision != "":
		case !o.Ended.IsZero():
			if now.Sub(o.Ended) >= overrideRetake {
				due[o.ID] = o.Ending
			}
		case o.Ref == "" && !o.Until.IsZero() && !now.Before(o.Until):
			due[o.ID] = "its time " + clockZone(now, o.Until) + " came"
		case o.Ref != "" && found[o.ID].Holds:
			due[o.ID] = o.Ref + " closed"
		}
	}
	return due
}

// overrideChecks are the issue-closed checks of the overrides of st that end
// with an issue or pull request, as timers the timer checks read: due from
// the override's set, checked every timerEvery.
func overrideChecks(st *state.State) []state.Timer {
	var out []state.Timer
	for _, o := range st.Overrides {
		if o.Ref != "" && o.Ended.IsZero() && o.Decision == "" {
			out = append(out, state.Timer{ID: o.ID, Due: o.At, When: "issue-closed " + o.Ref, Checked: o.Checked})
		}
	}
	return out
}

// settleOverrides restores the overrides whose end arrived, one tick of the
// watch or the supervisor loop: check reads the issues and pull requests
// that end them. Taking an override is one state write, so of the watch and
// the loop only one restores it. It returns the lines to say.
func (a *app) settleOverrides(ctx context.Context, by state.Party, check func([]state.Timer) map[int]checkResult) ([]string, error) {
	st, err := a.store.Peek()
	if err != nil || len(st.Overrides) == 0 {
		return nil, err
	}
	found := check(overrideChecks(st))
	now := a.now
	if len(overridesDue(st, found, now)) == 0 && len(found) == 0 {
		return nil, nil
	}
	var taken []state.Override
	var lines []string
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		taken, lines = nil, nil
		var evs []state.Event
		due := overridesDue(st, found, now)
		for i := range st.Overrides {
			o := &st.Overrides[i]
			if why, ok := due[o.ID]; ok {
				o.Ended, o.Ending = now.UTC(), why
				taken = append(taken, *o)
				lines = append(lines, fmt.Sprintf("CONFIG OVERRIDE ENDED: #%d, %s: restoring %s", o.ID, why, overrideKeys(*o)))
				evs = append(evs, about(event(by, overrideEndVerb, "#%d %s: %s", o.ID, why, overrideKeys(*o)), o.Ref))
				continue
			}
			if r, ok := found[o.ID]; ok {
				if r.Unreadable && (o.Found != r.Reason) {
					lines = append(lines, fmt.Sprintf("CONFIG OVERRIDE UNREADABLE: #%d, %s cannot be read (%s); it stays in effect", o.ID, o.Ref, r.Reason))
				}
				o.Checked, o.Found = now.UTC(), r.Reason
			}
		}
		return evs, nil
	})
	if err != nil {
		return lines, err
	}
	for _, o := range taken {
		decision, rerr := restoreOverride(o, false)
		l, err := a.finishOverride(by, o, decision, rerr)
		lines = append(lines, l...)
		if err != nil {
			return lines, err
		}
	}
	return lines, nil
}

// loopSettleOverrides is the loop's tick of the overrides: its GitHub reads
// take the loop's allowance and wait under the budget floor as its timers'.
func loopSettleOverrides(l *loop.Loop, ctx context.Context, st *state.State) {
	a := asApp(l.Host)
	held := loop.BoardBudgetLow(st.Budget, a.cfg.GitHub.Floor, a.now) ||
		a.cfg.GitHub.Floor > 0 && loop.LoopBudgetUnknownOrLow(st.Budget, a.cfg.GitHub.Floor, a.now) || a.resetFirst(st)
	lines, err := a.settleOverrides(ctx, loop.LoopParty, func(ts []state.Timer) map[int]checkResult {
		return a.loopTimerChecks(ctx, ts, a.now, held)
	})
	snapshot := *a
	w := &watch.Watcher{Host: &snapshot}
	for _, line := range lines {
		w.EmitNow("timer", "%s", line)
	}
	if err != nil {
		l.SayOnce("overrides", "SUPERVISOR CONFIG OVERRIDES FAILED: %v", err)
		return
	}
	delete(l.Said, "overrides")
}

// finishOverride records how the restore of the ended override o went:
// restored, it is gone; a decision asks guide.person in a note and keeps it
// listed; a failed write leaves it to a tick after overrideRetake.
func (a *app) finishOverride(by state.Party, o state.Override, decision string, rerr error) ([]string, error) {
	var lines []string
	person := a.cfg.Guide.Person
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		lines = nil
		i := slices.IndexFunc(st.Overrides, func(c state.Override) bool { return c.ID == o.ID })
		if i < 0 {
			return nil, nil
		}
		cur := &st.Overrides[i]
		switch {
		case rerr != nil:
			// Ended stays: a tick takes it again after overrideRetake.
			lines = append(lines, fmt.Sprintf("CONFIG RESTORE FAILED: #%d, %s: %v; it is tried again in %s", o.ID, o.Ending, rerr, dur(overrideRetake)))
			return []state.Event{about(event(by, overrideFailVerb, "#%d %s: %v", o.ID, o.Ending, rerr), o.Ref)}, nil
		case decision != "":
			cur.Decision = decision
			evs := []state.Event{about(event(by, overrideDecisionVerb, "#%d %s: %s", o.ID, o.Ending, decision), o.Ref)}
			line := fmt.Sprintf("CONFIG OVERRIDE NEEDS A DECISION: #%d, %s, but %s; nothing restored", o.ID, o.Ending, decision)
			if person != "" {
				d := noteDraft{
					Question:  truncate(fmt.Sprintf("Config override #%d ended (%s): restore its values before?", o.ID, o.Ending), questionMax),
					StatusQuo: truncate(decision+"; set for: "+o.Reason, statusQuoMax),
					Why:       "a key changed since the override, so restoring would undo a later change",
					Options: []string{
						fmt.Sprintf("restore: beekeeper config restore %d --force puts the values before back", o.ID),
						fmt.Sprintf("keep: beekeeper config keep %d keeps the current values", o.ID),
					},
				}
				st.NextNote++
				cur.Note = st.NextNote
				st.Notes = append(st.Notes, state.Note{ID: cur.Note, For: person, Text: d.text(), By: by, At: a.now.UTC(), Kind: noteDecision,
					Question: d.Question, StatusQuo: d.StatusQuo, Options: d.Options,
					Default: "the current values stay and the override stays listed until config restore or keep"})
				evs = append(evs, event(by, "note.add", "#%d %s", cur.Note, d.text()))
				line += fmt.Sprintf("; note #%d asks %s", cur.Note, person)
			}
			lines = append(lines, line)
			return evs, nil
		}
		st.Overrides = slices.Delete(st.Overrides, i, i+1)
		lines = append(lines, fmt.Sprintf("CONFIG RESTORED: #%d, %s: %s", o.ID, o.Ending, restoredKeys(o)))
		return []state.Event{about(event(by, overrideRestoreVerb, "#%d %s: %s (%s)", o.ID, o.Ending, restoredKeys(o), o.Path), o.Ref)}, nil
	})
	return lines, err
}

// restoredKeys names o's keys with the values a restore put back.
func restoredKeys(o state.Override) string {
	parts := make([]string, 0, len(o.Keys))
	for _, k := range o.Keys {
		parts = append(parts, k.Key+" "+previousText(k))
	}
	return strings.Join(parts, ", ")
}

// overridesCmds are config's commands on the overrides in effect.
func (a *app) overridesCmds() []*cobra.Command {
	list := &cobra.Command{
		Use:   "overrides",
		Short: "List the configuration overrides in effect: their keys, reasons and ends",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if err := a.load(); err != nil {
				return err
			}
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(st.Overrides)
			}
			a.printOverrides(st.Overrides)
			return nil
		},
	}
	var force bool
	restore := &cobra.Command{
		Use:   "restore <id>",
		Short: "End an override now: put its values before back",
		Long: `restore ends an override now and puts the values before it back. A key
whose value changed since the override set it is refused (exit 3) and
nothing is written; --force restores it anyway.

Exit codes: 0 restored, 2 usage, 3 refused.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return a.endOverride(args[0], func(o state.Override) (string, error) { return restoreOverride(o, force) })
		},
	}
	restore.Flags().BoolVar(&force, "force", false, "restore keys that changed since the override too")
	keep := &cobra.Command{
		Use:   "keep <id>",
		Short: "End an override now, keeping the current values",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return a.endOverride(args[0], nil)
		},
	}
	return []*cobra.Command{list, restore, keep}
}

// endOverride ends the override arg by hand: restore puts its values back
// (nil keeps the current ones). The decision note it asked closes.
func (a *app) endOverride(arg string, restore func(state.Override) (string, error)) error {
	ids, err := parseIDs([]string{arg}, "override")
	if err != nil {
		return err
	}
	if err := a.load(); err != nil {
		return err
	}
	me, err := a.caller()
	if err != nil {
		return err
	}
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(st.Overrides, func(o state.Override) bool { return o.ID == ids[0] })
	if i < 0 {
		return refused("override #%d is not in effect", ids[0])
	}
	o := st.Overrides[i]
	verb, how := overrideKeepVerb, "kept: "+overrideKeys(o)
	if restore != nil {
		decision, err := restore(o)
		switch {
		case err != nil:
			return err
		case decision != "":
			return refused("override #%d: %s; nothing restored (--force restores it anyway)", o.ID, decision)
		}
		verb, how = overrideRestoreVerb, "restored by hand: "+restoredKeys(o)
	}
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		var evs []state.Event
		st.Overrides = slices.DeleteFunc(st.Overrides, func(c state.Override) bool { return c.ID == o.ID })
		evs = append(evs, about(event(me, verb, "#%d %s (%s)", o.ID, how, o.Path), o.Ref))
		if o.Note != 0 {
			st.Notes = slices.DeleteFunc(st.Notes, func(n state.Note) bool {
				if n.ID == o.Note {
					evs = append(evs, event(me, noteDone, "#%d closed, override #%d %s", n.ID, o.ID, strings.TrimPrefix(verb, "config.")))
					return true
				}
				return false
			})
		}
		return evs, nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.out, "override #%d %s\n", o.ID, how)
	return err
}

// printOverrides lists the overrides in effect, one line each.
func (a *app) printOverrides(overrides []state.Override) {
	if len(overrides) == 0 {
		_, _ = fmt.Fprintln(a.out, "no configuration overrides")
		return
	}
	for _, o := range overrides {
		end := overrideEndText(a.now, o)
		switch {
		case o.Decision != "":
			end += fmt.Sprintf(", ended (%s), waits on a decision: %s", o.Ending, o.Decision)
			if o.Note != 0 {
				end += fmt.Sprintf(" (note #%d)", o.Note)
			}
		case !o.Ended.IsZero():
			end += ", ended (" + o.Ending + "), restoring"
		}
		_, _ = fmt.Fprintf(a.out, "- #%d %s [%s, set by %s %s]: %s\n", o.ID, overrideKeys(o), end, o.By.Name, clock(a.now, o.At), o.Reason)
	}
}
