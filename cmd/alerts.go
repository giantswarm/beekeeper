package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/proc"
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
uninhibited alerts, the names in alerts.ignore left out. The installations
are alerts.installations plus every held lease whose name resolves to a kube
context, read in parallel, each within alerts.timeout.

beekeeper watch runs the watch every alerts.every; these commands run it
once, for a look or a script.`,
		Args: cobra.NoArgs,
	}
	watch := &cobra.Command{
		Use:   "watch",
		Short: "One line per alert that is NEW or RESOLVED since the last reading",
		Long: `Read once, print what changed since the baseline and keep the new one:

  HH:MM:SS ALERT NEW|RESOLVED <installation> <severity> <team> <alertname> <namespace/object[@cluster]> since <start>

A page says PAGE and alerts.team's alerts carry the team in capitals. More
than alerts.collapse changes of one alertname are one line with a count. An
installation's first reading prints its set as OPEN lines; one that does not
answer is one unreachable line (its set is kept, nothing reads as resolved)
and one reachable-again line when it is back. The lease holder and the
sessions running commands against the installation follow in brackets.

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
	c.AddCommand(watch, snapshot, imp)
	return c
}

func (a *app) alertRules() alerts.Rules {
	al := a.cfg.Alerts
	return alerts.Rules{Ignore: al.Ignore, Team: al.Team, Collapse: al.Collapse}
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
	rules, now := a.alertRules(), time.Now()
	var lines []string
	var on func(alerts.Target) string
	for i, t := range targets {
		changed, next := rules.Step(t.Name, st.Installations[t.Name], answers[i], now)
		st.Installations[t.Name] = next
		if len(changed) == 0 {
			continue
		}
		if on == nil {
			on = a.onInstallation()
		}
		if who := on(t); who != "" {
			for j := range changed {
				changed[j] += " " + who
			}
		}
		lines = append(lines, changed...)
	}
	if err := store.Save(st); err != nil {
		lines = append(lines, "ALERTS baseline not saved: "+err.Error())
	}
	return lines
}

// onInstallation returns who is on an installation, in brackets: its lease
// holder and the sessions running a command against its context or name.
func (a *app) onInstallation() func(alerts.Target) string {
	var sessions []*claude.Session
	if t, err := proc.Read(); err == nil {
		sessions = claude.Discover(a.cfg, t, time.Now())
	}
	holders, _ := lease.Dir(a.cfg.LeaseDir).List()
	return func(t alerts.Target) string {
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
		if len(parts) == 0 {
			return ""
		}
		return "[" + strings.Join(parts, "; ") + "]"
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
	Baselines map[string]*alerts.Installation `json:"-"`
}

func (a *app) alertsHandover(ctx context.Context) (*alertsView, error) {
	st, err := alerts.NewStore(a.cfg.StateDir).Load()
	if err != nil {
		return nil, err
	}
	al := a.cfg.Alerts
	return &alertsView{
		Every: dur(al.Every.Duration), Owner: st.Owner, Live: st.Owner != nil && proc.Alive(st.Owner.PID), Targets: a.alertTargets(ctx),
		Ignore: al.Ignore, Team: al.Team, Collapse: al.Collapse, Baselines: st.Installations,
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
			base = fmt.Sprintf("unreachable, %d active when last read", len(b.Alerts))
		case b != nil:
			base = "unreachable, never read"
		}
		p("- %s (%s; context %s): %s", t.Name, t.Why, cmp.Or(t.Context, "none"), base)
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
}
