package cmd

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The events of a pace override.
const (
	paceOffVerb = "capacity.pace.off"
	paceOnVerb  = "capacity.pace.on"
)

// withPaceOffs puts the pace overrides of st in effect at now into readings,
// the provider readings a capacity verdict weighs.
func withPaceOffs(readings map[string]capacity.ProviderReadingResult, st *state.State, now time.Time) {
	if st == nil {
		return
	}
	for name, r := range readings {
		r.PaceOff = capacity.NewPaceOffView(st.PaceOff(name, now))
		readings[name] = r
	}
}

// activePaceOffs are the pace overrides of st in effect at now.
func activePaceOffs(st *state.State, now time.Time) []state.PaceOff {
	var out []state.PaceOff
	for _, p := range st.PaceOffs {
		if now.Before(p.Until) {
			out = append(out, p)
		}
	}
	return out
}

func (a *app) capacityPaceCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "pace",
		Short: "Let worker starts on a provider run ahead of its 7-day pace until a time",
		Long: `No worker start runs ahead of the even pace of its provider's 7-day window.
"capacity pace off <provider> --until <time> --reason <why>" records the
person's word that it may for a while, such as a usage reset in hand: until
--until, and only within the 7-day window current when it was set. The
provider's worker starts are then neither refused for running ahead of the
pace nor capped by its paceCaps; its weekly caps (sevenDayMax, a choice's
workerSevenDayMax, 100%), its 5-hour window, its ceiling and the machine
guards still bound them, so the supervisor loop schedules it up to the weekly
cap. The override ends at --until, once the provider's 7-day window starts
anew (the reset used, or the week over) or with "capacity pace on
<provider>"; then the pace holds again. A second "pace off" of the provider
replaces the first. beekeeper capacity shows it on the provider's line, --json
under providers.<name>.paceOff, and the hand-over lists it.

Without a subcommand, lists the pace overrides in effect.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return a.paceList() },
	}
	var until, reason string
	off := &cobra.Command{
		Use:   "off <provider> --until <time> --reason <why>",
		Short: "Let a provider's worker starts run ahead of its 7-day pace until a time",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return a.paceOff(args[0], until, reason) },
	}
	off.Flags().StringVar(&until, "until", "", `when it ends (required): a time ("23:00", "2026-11-15 23:00", RFC 3339) or a duration ("48h")`)
	off.Flags().StringVarP(&reason, "reason", "r", "", "why, in the person's words (required)")
	on := &cobra.Command{
		Use:   "on <provider>",
		Short: "End a provider's pace override: its worker starts keep to the pace again",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return a.paceOn(args[0]) },
	}
	c.AddCommand(off, on)
	return c
}

// paceProvider is the configured subscription provider s names.
func (a *app) paceProvider(s string) (string, error) {
	k := a.liveCapacity()
	names := k.ProviderNames()
	for _, name := range names {
		if !strings.EqualFold(name, strings.TrimSpace(s)) {
			continue
		}
		if k.Providers[name].SelfHosted() {
			return "", usageErr("provider %s is self-hosted: it has no pace", name)
		}
		return name, nil
	}
	return "", usageErr("provider %q is not in capacity.providers (%s)", s, strings.Join(names, ", "))
}

// paceOff records the pace override of provider until until, for reason.
func (a *app) paceOff(provider, until, reason string) error {
	name, err := a.paceProvider(provider)
	if err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	if strings.TrimSpace(until) == "" || reason == "" {
		return usageErr(`pace off needs --until <time> and --reason "<the person's words>"`)
	}
	u, err := untilTime(a.now, strings.TrimSpace(until))
	switch {
	case err != nil:
		return usageErr("--until %q: a time (23:00, 2026-11-15 23:00) or a duration (48h)", until)
	case !u.After(a.now):
		return usageErr("--until %q is not in the future", until)
	}
	me, err := a.caller()
	if err != nil {
		return err
	}
	p := state.PaceOff{Provider: name, Until: u.UTC(), Reason: reason, By: me, At: a.now.UTC()}
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.PaceOffs = slices.DeleteFunc(st.PaceOffs, func(x state.PaceOff) bool {
			return strings.EqualFold(x.Provider, name) || !a.now.Before(x.Until)
		})
		st.PaceOffs = append(st.PaceOffs, p)
		return []state.Event{event(me, paceOffVerb, "%s", paceOffText(a.now, p))}, nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.out, "pace off %s\n", paceOffText(a.now, p))
	return err
}

// paceOn ends the pace override of provider.
func (a *app) paceOn(provider string) error {
	provider = strings.TrimSpace(provider)
	me, err := a.caller()
	if err != nil {
		return err
	}
	var ended state.PaceOff
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		p := st.PaceOff(provider, a.now)
		if p == nil {
			return nil, refused("no pace override of %s is in effect (beekeeper capacity pace)", provider)
		}
		ended = *p
		st.PaceOffs = slices.DeleteFunc(st.PaceOffs, func(x state.PaceOff) bool {
			return strings.EqualFold(x.Provider, provider) || !a.now.Before(x.Until)
		})
		return []state.Event{event(me, paceOnVerb, "%s keeps to its 7-day pace again", ended.Provider)}, nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.out, "pace on %s: its worker starts keep to the 7-day pace again\n", ended.Provider)
	return err
}

// paceList prints the pace overrides in effect.
func (a *app) paceList() error {
	st, err := a.readSnapshot()
	if err != nil {
		return err
	}
	offs := activePaceOffs(st, a.now)
	if a.json {
		if offs == nil {
			offs = []state.PaceOff{}
		}
		return a.printJSON(offs)
	}
	a.printPaceOffs(offs)
	return nil
}

// paceOffText says p: "claude until 18:00 CET (or a new 7-day window), set by
// guide 09:12: <reason>".
func paceOffText(now time.Time, p state.PaceOff) string {
	return fmt.Sprintf("%s until %s or a new 7-day window, set by %s %s: %s", p.Provider, clockZone(now, p.Until), p.By.Name, clock(now, p.At), p.Reason)
}

// printPaceOffs lists pace overrides, one line each.
func (a *app) printPaceOffs(offs []state.PaceOff) {
	if len(offs) == 0 {
		_, _ = fmt.Fprintln(a.out, "no pace overrides")
		return
	}
	for _, p := range offs {
		_, _ = fmt.Fprintf(a.out, "- pace off %s\n", paceOffText(a.now, p))
	}
}
