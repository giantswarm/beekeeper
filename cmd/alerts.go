package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// errInterrupted ends a reading cut short by SIGINT or SIGTERM: every
// port-forward has ended and the baseline is unchanged.
var errInterrupted = errors.New("interrupted: nothing read, the baseline is unchanged")

func (a *app) alertsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "alerts",
		Short: "The alerts of the installations the sessions work on",
		Long: `Read each installation's Alertmanager through a bounded kubectl port-forward
(Mimir's mimir/mimir-alertmanager:8080 with the giantswarm tenant, else
monitoring/kube-prometheus-stack-alertmanager:9093): active, unsilenced and
uninhibited alerts, the names in alerts.ignore left out. alerts.team's alerts
that only InhibitionOutsideWorkingHours inhibits are read too: working hours
keep them from paging, not from the watch. The installations are
alerts.installations plus every held lease whose name resolves to a kube
context, read in parallel, each within alerts.timeout; a failed port-forward
or request is tried again until the timeout.

beekeeper watch runs the watch every alerts.every; these commands run it
once, for a look or a script. capture records the answers and replay runs
the watch's rules over recorded ones, to try a floor or damper setting.`,
		Args: cobra.NoArgs,
	}
	watch := &cobra.Command{
		Use:   "watch",
		Short: "One line per alert that is NEW or RESOLVED since the last reading",
		Long: `Read once, print what changed since the baseline and keep the new one:

  HH:MM:SS ALERT NEW|FLAPPING <installation> <severity> <team> <alertname> <namespace/object[@cluster]> since <start>
  HH:MM:SS ALERT RESOLVED <installation> <severity> <team> <alertname> <namespace/object[@cluster]>

A page says PAGE and alerts.team's alerts carry the team in capitals. More
than alerts.collapse changes of one alertname are one line with a count. An
alert below its installation's floor (alerts.installations[].floor) is never
printed; it stays in the baseline, so a changed floor prints no burst. The
alerts.flap.changes-th change of an alert within alerts.flap.window is one
FLAPPING line (since its first change in the window), and its changes print
nothing until it has been stable for the window. An installation's first
reading prints its set as OPEN lines; one that does not answer is an
unreachable line with how long its alerts have been unseen (its set is kept,
nothing reads as resolved), said again every 15 minutes while it does not
answer, and one reachable-again line when it is back. The lease holder and the sessions
running commands against the installation follow in brackets; a NEW line
also names the merges into the installation's lanes (merging, merged) and
the lease claims on it of the last 30 minutes, each with its session.

The baseline has one owner: while a beekeeper watch reads the alerts, this
command refuses.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			store := alerts.NewStore(a.cfg.StateDir)
			owned, owner, err := store.Own()
			if err != nil {
				return err
			}
			if !owned {
				return fmt.Errorf("the alerts are read by pid %d (a beekeeper watch) since %s", owner.PID, clock(a.now, owner.Since))
			}
			defer func() { _ = store.Release() }()
			for _, l := range a.alertCycle(ctx, store) {
				_, _ = fmt.Fprintln(a.out, time.Now().Format("15:04:05"), l)
			}
			if ctx.Err() != nil {
				return errInterrupted
			}
			return nil
		},
	}
	snapshot := &cobra.Command{
		Use:   "snapshot",
		Short: "The current alerts, grouped by severity, team, alertname and cluster",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			lines := a.alertSnapshot(ctx)
			if ctx.Err() != nil {
				return errInterrupted
			}
			if a.json {
				return a.printJSON(lines)
			}
			_, _ = fmt.Fprintln(a.out, strings.Join(lines, "\n"))
			return nil
		},
	}
	imp := &cobra.Command{
		Use:   "import <dir>",
		Short: "Take over another watcher's baseline: one <installation>.json per installation",
		Long: `Add the baselines in dir (one <installation>.json each, as
{"reachable": true, "alerts": {"<fingerprint>": {…}}}) for every installation
beekeeper has none of yet, so switching from another watcher prints no burst
of NEW lines.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			store := alerts.NewStore(a.cfg.StateDir)
			owned, owner, err := store.Own()
			if err != nil {
				return err
			}
			if !owned {
				return fmt.Errorf("the alerts are read by pid %d; stop that watch first", owner.PID)
			}
			defer func() { _ = store.Release() }()
			st, err := store.Load()
			if err != nil {
				return err
			}
			done, err := alerts.Import(st, args[0])
			if err != nil {
				return err
			}
			if len(done) == 0 {
				return errors.New("nothing to import: every installation there has a baseline already, or there is none")
			}
			if err := store.Save(st); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(a.out, "imported %s\n", strings.Join(done, ", "))
			return nil
		},
	}
	capture := &cobra.Command{
		Use:   "capture <dir>",
		Short: "Record each installation's Alertmanager answer as <dir>/<installation>.json, for replay",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			targets := a.alertTargets(ctx)
			answers := a.alertReader().Read(ctx, targets)
			if ctx.Err() != nil {
				return errInterrupted
			}
			if err := os.MkdirAll(args[0], 0o700); err != nil {
				return err
			}
			for i, t := range targets {
				ans := answers[i]
				if !ans.OK {
					_, _ = fmt.Fprintf(a.out, "%s unreachable: %s\n", t.Name, ans.Why)
					continue
				}
				path, err := alerts.WriteAnswer(args[0], t.Name, ans.Alerts)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(a.out, "%s: %d alerts in %s\n", t.Name, len(ans.Alerts), path)
			}
			return nil
		},
	}
	var every time.Duration
	replay := &cobra.Command{
		Use:   "replay <dir>...",
		Short: "The lines watch would print for recorded answers, from the current baseline; writes nothing",
		Long: `Each dir is one reading as alerts capture records it, of the installations
it has an answer for; the readings are --every apart (default alerts.every),
the first now. The lines are what the watch would print for them, starting
from the current baseline, with the floors, the damper and the owner hints
of the configuration: a floor or damper setting tried on recorded answers
before the watch gets it. The baseline is read, never owned or written.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := alerts.NewStore(a.cfg.StateDir).Load()
			if err != nil {
				return err
			}
			known := map[string]alerts.Target{}
			for _, t := range a.alertTargets(cmd.Context()) {
				known[t.Name] = t
			}
			at := time.Now()
			for i, dir := range args {
				recorded, err := alerts.ReadAnswers(dir)
				if err != nil {
					return err
				}
				var targets []alerts.Target
				var answers []alerts.Answer
				for _, name := range slices.Sorted(maps.Keys(recorded)) {
					targets = append(targets, cmp.Or(known[name], alerts.Target{Name: name}))
					answers = append(answers, alerts.Answer{OK: true, Alerts: recorded[name]})
				}
				now := at.Add(time.Duration(i) * cmp.Or(every, a.cfg.Alerts.Every.Duration))
				for _, l := range a.alertLines(st, targets, answers, now) {
					_, _ = fmt.Fprintln(a.out, now.Format("15:04:05"), l)
				}
			}
			return nil
		},
	}
	replay.Flags().DurationVar(&every, "every", 0, "the time between two readings (default alerts.every)")
	c.AddCommand(watch, snapshot, imp, capture, replay)
	return c
}

func (a *app) alertRules() alerts.Rules {
	al := a.cfg.Alerts
	floors := map[string]string{}
	for _, in := range al.Installations {
		if in.Floor != "" {
			floors[in.Name] = in.Floor
		}
	}
	return alerts.Rules{Ignore: al.Ignore, Team: al.Team, Collapse: al.Collapse, Floors: floors,
		Flap: alerts.Damper{Changes: al.Flap.Changes, Window: al.Flap.Window.Duration}}
}

func (a *app) alertReader() alerts.Reader {
	return alerts.Reader{Kubectl: a.cfg.Alerts.Kubectl, Timeout: a.cfg.Alerts.Timeout.Duration}
}

// alertTargets are the configured installations and the leased ones.
func (a *app) alertTargets(ctx context.Context) []alerts.Target {
	var configured []alerts.Target
	for _, in := range a.cfg.Alerts.Installations {
		configured = append(configured, alerts.Target{Name: in.Name, Context: in.Context})
	}
	leased := map[string]string{}
	if holders, err := lease.Dir(a.cfg.LeaseDir).List(); err == nil {
		for _, h := range holders {
			leased[h.Env] = fmt.Sprintf("%q", cmp.Or(h.Name, h.Holder))
		}
	}
	return alerts.Targets(configured, leased, a.alertReader().Contexts(ctx))
}

// alertCycle reads every installation once, returns the lines of what
// changed and keeps the new baseline. An interrupted reading keeps nothing.
func (a *app) alertCycle(ctx context.Context, store *alerts.Store) []string {
	targets := a.alertTargets(ctx)
	answers := a.alertReader().Read(ctx, targets)
	if ctx.Err() != nil {
		return nil
	}
	st, err := store.Load()
	if err != nil {
		return []string{"ALERTS baseline unreadable: " + err.Error()}
	}
	lines := a.alertLines(st, targets, answers, time.Now())
	if err := store.Save(st); err != nil {
		lines = append(lines, "ALERTS baseline not saved: "+err.Error())
	}
	return lines
}

// alertLines steps every target's baseline in st with its answer and returns
// the lines, each with who is on the installation in brackets, a NEW one
// also with the merges and lease claims that likely caused it.
func (a *app) alertLines(st *alerts.State, targets []alerts.Target, answers []alerts.Answer, now time.Time) []string {
	rules := a.alertRules()
	var lines []string
	var on func(alerts.Target) []string
	var hints func(string) []string
	for i, t := range targets {
		changed, next := rules.Step(t.Name, st.Installations[t.Name], answers[i], now)
		st.Installations[t.Name] = next
		if len(changed) == 0 {
			continue
		}
		if on == nil {
			on, hints = a.onInstallation(), a.ownerHints(now)
		}
		lines = append(lines, decorate(changed, on(t), func() []string { return hints(t.Name) })...)
	}
	return lines
}

// decorate appends who is on the installation to each line in brackets, and
// to a NEW line also the owner hints, asked for once. Each part is said
// once per reading: the lines after it share it.
func decorate(lines, who []string, hints func() []string) []string {
	var owners []string
	asked := false
	said := map[string]bool{}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		parts := who
		if alerts.IsNew(l) {
			if !asked {
				owners, asked = hints(), true
			}
			parts = append(slices.Clone(who), owners...)
		}
		parts = slices.DeleteFunc(slices.Clone(parts), func(p string) bool { return said[p] })
		for _, p := range parts {
			said[p] = true
		}
		if len(parts) > 0 {
			l += " [" + strings.Join(parts, "; ") + "]"
		}
		out = append(out, l)
	}
	return out
}

// hintWindow is how far back the owner hints of a new alert reach.
const hintWindow = 30 * time.Minute

// ownerHints returns the owner hints of an installation at now from the
// event log; an unreadable log is no hint.
func (a *app) ownerHints(now time.Time) func(string) []string {
	var events []state.Event
	if a.store != nil {
		events, _ = a.store.Events(0, func(e state.Event) bool {
			return strings.HasPrefix(e.Verb, "merg") || e.Verb == "lease.claim"
		})
	}
	return func(installation string) []string {
		return ownerHints(events, a.cfg.LaneOf, installation, now)
	}
}

// ownerHints are what went on on an installation around a new alert at now,
// newest first, worded as timing, not cause: the merges into its lanes that are running or merged within
// hintWindow (merging, merged; the latest event of a pull request counts,
// so a failed or dropped merge is none) and the lease claims on it, each
// with its session. events are oldest first.
func ownerHints(events []state.Event, laneOf func(string) config.Lane, installation string, now time.Time) []string {
	type hint struct {
		at   time.Time
		text string
	}
	latest := map[string]hint{}
	var order []string
	for _, e := range events {
		if e.At.Before(now.Add(-hintWindow)) || e.At.After(now) {
			continue
		}
		key, text := "", ""
		switch {
		case e.Verb == "lease.claim":
			if !strings.HasPrefix(e.Detail, installation+":") {
				continue
			}
			key, text = "lease "+e.By.Name, fmt.Sprintf("claimed by %q at %s", e.By.Name, e.At.UTC().Format("15:04Z"))
		case strings.HasPrefix(e.Verb, "merg"):
			fields := strings.Fields(strings.ReplaceAll(e.Detail, ":", " "))
			if len(fields) == 0 {
				continue
			}
			repo, _, ok := strings.Cut(fields[0], "#")
			if !ok || laneOf(repo).Installation != installation {
				continue
			}
			key = fields[0]
			switch e.Verb {
			case "merging":
				text = fmt.Sprintf("during merging %s by %q since %s", key, e.By.Name, e.At.UTC().Format("15:04Z"))
			case "merged":
				text = fmt.Sprintf("during merged %s by %q at %s", key, e.By.Name, e.At.UTC().Format("15:04Z"))
			case "merge.queued":
				continue
			}
		default:
			continue
		}
		if _, seen := latest[key]; !seen {
			order = append(order, key)
		}
		latest[key] = hint{e.At, text}
	}
	var out []hint
	for _, k := range order {
		if h := latest[k]; h.text != "" {
			out = append(out, h)
		}
	}
	slices.SortStableFunc(out, func(a, b hint) int { return b.at.Compare(a.at) })
	texts := make([]string, len(out))
	for i, h := range out {
		texts[i] = h.text
	}
	return texts
}

// onInstallation returns who is on an installation: its lease holder and the
// sessions running a command against its context or name.
func (a *app) onInstallation() func(alerts.Target) []string {
	var sessions []*claude.Session
	if t, err := proc.Read(); err == nil {
		sessions = claude.Discover(a.cfg, t, time.Now())
	}
	holders, _ := lease.Dir(a.cfg.LeaseDir).List()
	return func(t alerts.Target) []string {
		var parts []string
		holder := ""
		for _, h := range holders {
			if h.Env == t.Name {
				holder = a.leaseView(sessions, h).Name
				parts = append(parts, fmt.Sprintf("lease %q", holder))
			}
		}
		var names []string
		for _, s := range sessions {
			if s.Name != holder && !slices.Contains(names, fmt.Sprintf("%q", s.Name)) && runsAgainst(s, t) {
				names = append(names, fmt.Sprintf("%q", s.Name))
			}
		}
		if len(names) > 0 {
			slices.Sort(names)
			parts = append(parts, "sessions "+strings.Join(names, ", "))
		}
		return parts
	}
}

// runsAgainst reports whether a session runs a command naming the
// installation's context or the installation as a word.
func runsAgainst(s *claude.Session, t alerts.Target) bool {
	for _, c := range s.Commands {
		if t.Context != "" && strings.Contains(c.Args, t.Context) {
			return true
		}
		if slices.Contains(strings.Fields(c.Args), t.Name) {
			return true
		}
	}
	return false
}

// alertSnapshot is every installation's current set, grouped.
func (a *app) alertSnapshot(ctx context.Context) []string {
	targets := a.alertTargets(ctx)
	answers := a.alertReader().Read(ctx, targets)
	rules, now := a.alertRules(), time.Now()
	var lines []string
	for i, t := range targets {
		lines = append(lines, rules.SnapshotLines(t.Name, answers[i], now)...)
	}
	return lines
}

// alertsView is what the alert watch reads, for the hand-over.
type alertsView struct {
	Every     string                          `json:"every"`
	Owner     *alerts.Owner                   `json:"owner,omitempty"`
	Live      bool                            `json:"live"`
	Targets   []alerts.Target                 `json:"installations"`
	Ignore    []string                        `json:"ignore"`
	Team      string                          `json:"team,omitempty"`
	Collapse  int                             `json:"collapse"`
	Floors    map[string]string               `json:"floors,omitempty"`
	Flap      flapView                        `json:"flap"`
	Baselines map[string]*alerts.Installation `json:"-"`
}

// flapView is the damper's setting.
type flapView struct {
	Changes int    `json:"changes"`
	Window  string `json:"window"`
}

func (a *app) alertsHandover(ctx context.Context) (*alertsView, error) {
	st, err := alerts.NewStore(a.cfg.StateDir).Load()
	if err != nil {
		return nil, err
	}
	al := a.cfg.Alerts
	return &alertsView{
		Every: dur(al.Every.Duration), Owner: st.Owner, Live: st.Owner != nil && proc.Alive(st.Owner.PID), Targets: a.alertTargets(ctx),
		Ignore: al.Ignore, Team: al.Team, Collapse: al.Collapse, Floors: a.alertRules().Floors,
		Flap: flapView{Changes: al.Flap.Changes, Window: dur(al.Flap.Window.Duration)}, Baselines: st.Installations,
	}, nil
}

func (a *app) printAlerts(v *alertsView) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format+"\n", args...) }
	if len(v.Targets) == 0 {
		p("No installation is watched (alerts.installations is empty and no leased one has a kube context).")
	}
	for _, t := range v.Targets {
		b := v.Baselines[t.Name]
		base := "no baseline yet"
		switch {
		case b != nil && b.Alerts != nil && b.Reachable:
			base = fmt.Sprintf("%d active", len(b.Alerts))
		case b != nil && b.Alerts != nil:
			base = fmt.Sprintf("unreachable, %d active when last read%s", len(b.Alerts), lastRead(a.now, b))
		case b != nil:
			base = "unreachable, never read"
		}
		floor := ""
		if f := v.Floors[t.Name]; f != "" {
			floor = "; floor " + f
		}
		p("- %s (%s; context %s%s): %s", t.Name, t.Why, cmp.Or(t.Context, "none"), floor, base)
	}
	switch {
	case v.Live:
		p("\nRead every %s by the watch with pid %d since %s.", v.Every, v.Owner.PID, clock(a.now, v.Owner.Since))
	case v.Owner != nil:
		p("\nNot read now: the last watch (pid %d, since %s) has ended; the next beekeeper watch takes over.", v.Owner.PID, clock(a.now, v.Owner.Since))
	default:
		p("\nNot read yet: beekeeper watch reads them every %s.", v.Every)
	}
	p("Ignored alert names: %s.", strings.Join(v.Ignore, ", "))
	if v.Team != "" {
		p("Marked team: %s; more than %d changes of one alertname are one line.", v.Team, v.Collapse)
	}
	p("Flapping: an alert's %d changes within %s are one FLAPPING line, then nothing until it has been stable for %s.", v.Flap.Changes, v.Flap.Window, v.Flap.Window)
}
