// Package cmd is beekeeper's command line.
package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/central"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// Exit codes: 0 done, 1 error, 2 usage, 3 refused (a lease held or not
// granted, a hold set, the budget under its floor, a platform part this
// build does not have), 4 relieved (supervisor status in the session a
// relay relieved), 69 central unreachable (ExitCentral: a central verb
// whose instance did not answer), 78 vault (ExitVault: beekeeper secret
// could not read the shared vault: none configured, no token, op failing
// or silent), 125 outdated (a newer release exists: self-update --check, the status devctl's version check and
// muster's self-update --check use).
const (
	ExitError    = 1
	ExitUsage    = 2
	ExitRefused  = 3
	ExitRelieved = 4
	ExitVault    = 78
	ExitOutdated = 125
)

// exitError carries the process exit code of a failed command.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// Code returns the exit code err asks for.
func Code(err error) int {
	var e *exitError
	if errors.As(err, &e) {
		return e.code
	}
	if platform.Missing(err) {
		return ExitRefused
	}
	return ExitError
}

func refused(format string, a ...any) error {
	return &exitError{code: ExitRefused, msg: fmt.Sprintf(format, a...)}
}

func usageErr(format string, a ...any) error {
	return &exitError{code: ExitUsage, msg: fmt.Sprintf(format, a...)}
}

// Main runs the command line and returns the process exit code. An error
// without a message (a wrapped command's exit code) prints nothing.
func Main() int {
	err := New().Execute()
	if err == nil {
		return 0
	}
	if msg := err.Error(); msg != "" {
		fmt.Fprintln(os.Stderr, project.Name+":", msg)
	}
	return Code(err)
}

type app struct {
	cfgPath string
	as      string
	json    bool
	// hook says a hook runs, for a session whose environment it keeps.
	hook bool

	cfg   *config.Config
	store state.Store
	now   time.Time
	out   io.Writer

	// kindClusters lists the running kind clusters; nil asks docker.
	kindClusters func() ([]string, error)
	// zone reads the machine's time zone; nil is machine.Zone.
	zone func() (*time.Location, error)
	// central is an app of beekeeper serve: its notes' filers run on other
	// machines and learn of an outcome from its feed.
	central bool
	// centralClient calls the central instance (hub); nil until first used.
	centralClient *central.Client
	// guideDues are the guide's relay dues its guide watch said; nil in a
	// guide watch --once.
	guideDues relayDues
}

// New returns the root command.
func New() *cobra.Command {
	a := &app{out: os.Stdout}
	cobra.EnableCommandSorting = false
	root := &cobra.Command{
		Use:   project.Name,
		Short: "Keeps the Claude Code sessions sharing one machine working together",
		Long: `beekeeper keeps the Claude Code sessions sharing one machine working
together. It shows every running session and what it does, watches the
machine's memory, swap, disk and OOM kills, reads the GitHub budget all
sessions draw from, and holds the shared resources one session at a time:
kind labs, installations, the browser, merges into a repository.

Its state (the supervisor, grants, holds, registered agents, notes, timers,
session records) lives on disk and survives restarts: a supervisor's
successor starts from beekeeper handover --prompt instead of a prose brief.

Exit codes: 0 done, 1 error, 2 usage, 3 refused, 4 relieved (supervisor
status after a relay), 69 the central instance unreachable (a central verb,
with central configured), 125 a newer release (self-update --check).`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       project.VersionLine(),
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return a.load()
		},
	}
	root.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	pf := root.PersistentFlags()
	pf.StringVar(&a.cfgPath, "config", "", "configuration file (default $XDG_CONFIG_HOME/beekeeper/config.yaml, or $BEEKEEPER_CONFIG)")
	pf.StringVar(&a.as, "as", "", "act as this person or script instead of the calling Claude Code session")
	pf.BoolVar(&a.json, "json", false, "print JSON")

	root.AddGroup(
		&cobra.Group{ID: "watching", Title: "Watching:"},
		&cobra.Group{ID: "sharing", Title: "Sharing:"},
		&cobra.Group{ID: supervisorRole.ing, Title: "Supervising:"},
		&cobra.Group{ID: "guarding", Title: "Guarding:"},
	)
	for _, c := range []*cobra.Command{a.statusCmd(), a.capacityCmd(), a.sessionsCmd(), a.tailCmd(), a.snapshotCmd(), a.uiCmd(), a.onHost(a.watchCmd(), sandbox.OpWatch, 0), a.alertsCmd(), a.budgetCmd(), a.teleportCmd(), a.psCmd()} {
		c.GroupID = "watching"
		root.AddCommand(c)
	}
	for _, c := range []*cobra.Command{a.leaseCmd(), a.holdCmd(), a.lanesCmd(), a.boardCmd()} {
		c.GroupID = "sharing"
		root.AddCommand(c)
	}
	for _, c := range []*cobra.Command{a.supervisorCmd(), a.guideCmd(), a.agentsCmd(), a.doctorCmd(), a.noteCmd(), a.timerCmd(), a.reporterCmd(), a.reportCmd(), a.handoverCmd(), a.logCmd(), a.lintCmd()} {
		c.GroupID = "supervising"
		root.AddCommand(c)
	}
	for _, c := range []*cobra.Command{a.runCmd(), a.hookCmd(), a.freeCmd(), a.gateCmd(), a.scanCmd(), a.secretCmd(), a.sandboxCmd()} {
		c.GroupID = "guarding"
		root.AddCommand(c)
	}
	root.AddCommand(a.installCmd(), a.uninstallCmd(), a.selfUpdateCmd(), a.versionCmd(), a.mergeChildCmd(), a.followRunCmd(), a.squashMergeCmd(), a.centralCmd())
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &exitError{code: ExitUsage, msg: err.Error()}
	})
	// Runnable, so that an unknown command reaches the argument check below
	// instead of printing the help with exit 0.
	root.Args = cobra.NoArgs
	root.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
	usageArgs(root)
	return root
}

// usageArgs makes every command's argument check fail with the usage exit
// code: cobra returns those errors untyped.
func usageArgs(c *cobra.Command) {
	if check := c.Args; check != nil {
		c.Args = func(cmd *cobra.Command, args []string) error {
			if err := check(cmd, args); err != nil {
				return &exitError{code: ExitUsage, msg: err.Error()}
			}
			return nil
		}
	}
	for _, sub := range c.Commands() {
		usageArgs(sub)
	}
}

func (a *app) load() error {
	err := a.loadConfig()
	if err != nil {
		return err
	}
	a.store, err = state.Open(a.cfg.StateDir)
	if err != nil {
		return err
	}
	a.now = time.Now()
	return nil
}

// caller is who runs this command: the Claude Code session it runs in, or
// the --as name, with the configured person, team and host.
func (a *app) caller() (state.Party, error) {
	p, err := a.callerSession()
	p.Person, p.Team, p.Host = a.cfg.Identity.Person, a.cfg.Identity.Team, a.cfg.Identity.Host
	return p, err
}

// callerSession is the session or --as name that runs this command. The
// session's name is its desktop title, else the name in the environment,
// else the name its CLI's record holds (a `claude --bg` worker's tool
// commands inherit neither), else its id.
func (a *app) callerSession() (state.Party, error) {
	if a.as != "" {
		return state.Party{Name: a.as}, nil
	}
	// An omp agent beekeeper started is its roster entry, whatever session
	// variables its tool shell carries.
	if id := os.Getenv(omp.EnvAgent); id != "" {
		return state.Party{HostSession: omp.HostPrefix + id, Name: os.Getenv(omp.EnvName)}, nil
	}
	p := state.Party{
		Session:     os.Getenv("CLAUDE_CODE_SESSION_ID"),
		HostSession: os.Getenv("CLAUDE_CODE_HOST_SESSION_ID"),
		Name:        os.Getenv("CLAUDE_CODE_SESSION_NAME"),
	}
	if p.Session == "" {
		return p, &exitError{code: ExitUsage, msg: "not inside a Claude Code session: pass --as <name>"}
	}
	if p.HostSession != "" {
		if r, ok := claude.ReadRecord(a.cfg, p.HostSession); ok && r.Title != "" {
			p.Name = r.Title
		}
	}
	if p.Name == "" {
		if t, err := plat.Machine.Processes(); err == nil {
			p.Name = claude.RecordName(a.cfg, t, p.Session)
		}
	}
	if p.Name == "" {
		p.Name = p.Session
	}
	return p, nil
}

// sessions reads the process table and the running sessions.
func (a *app) sessions() ([]*claude.Session, *proc.Table, error) {
	t, err := plat.Machine.Processes()
	if err != nil {
		return nil, nil, err
	}
	return discover(a.cfg, t, a.now), t, nil
}

// discover returns the running sessions of every harness: Claude Code's,
// then omp's.
func discover(cfg *config.Config, t *proc.Table, now time.Time) []*claude.Session {
	return append(claude.Discover(cfg, t, now), omp.Discover(cfg.Omp.SessionsDir, t, now)...)
}

func (a *app) printJSON(v any) error {
	enc := json.NewEncoder(a.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (a *app) table() *tabwriter.Writer {
	return tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
}

// ago renders how long ago t was: "4s", "12m", "3h05m", "2d".
func ago(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return dur(now.Sub(t))
}

func dur(d time.Duration) string {
	switch {
	case d < 0:
		return "-" + dur(-d)
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours())/24)
}

// clock renders t as local "15:04", with the date when it is not today.
func clock(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	t = t.Local()
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

// untilTime parses "15:30" (the next such local time) or a duration ("2h").
func untilTime(now time.Time, s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(d), nil
	}
	for _, layout := range []string{"15:04", "2006-01-02 15:04", time.RFC3339} {
		t, err := time.ParseInLocation(layout, s, time.Local)
		if err != nil {
			continue
		}
		if layout == "15:04" {
			t = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.Local)
			if !t.After(now) {
				t = t.Add(24 * time.Hour)
			}
		}
		return t, nil
	}
	return time.Time{}, &exitError{code: ExitUsage, msg: fmt.Sprintf("%q is neither a time (15:30, 2006-01-02 15:04) nor a duration (2h)", s)}
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// withOwner is s followed by whose agent p is, of which team, on which
// host, when the state knows it.
func withOwner(s string, p state.Party) string {
	if o := p.Owner(); o != "" {
		return s + " (" + o + ")"
	}
	return s
}

// quotedOwner is p's quoted name with whose agent it is, as withOwner.
func quotedOwner(p state.Party) string { return withOwner(fmt.Sprintf("%q", p.Name), p) }

func event(by state.Party, verb, format string, a ...any) state.Event {
	return state.Event{At: time.Now().UTC(), By: by, Verb: verb, Detail: fmt.Sprintf(format, a...)}
}

// listCmd is the `list` subcommand of a noun, which the noun alone runs too.
func listCmd(short string, run func() error) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: short,
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return run() },
	}
}

func (a *app) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return nil
		},
		RunE: func(*cobra.Command, []string) error {
			_, err := fmt.Fprintln(a.out, project.Name, project.VersionLine())
			return err
		},
	}
}
