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

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// Exit codes: 0 done, 1 error, 2 usage, 3 refused (a lease held or not
// granted, a hold set, the budget under its floor), 125 outdated (a newer
// release exists: self-update --check, the status devctl's version check and
// muster's self-update --check use).
const (
	ExitError    = 1
	ExitUsage    = 2
	ExitRefused  = 3
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

	cfg   *config.Config
	store *state.Store
	now   time.Time
	out   io.Writer
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

Its state (the supervisor, grants, holds, registered agents, notes) lives
on disk and survives restarts: a supervisor's successor reads it instead of
rebuilding it from a hand-over prompt.

Exit codes: 0 done, 1 error, 2 usage, 3 refused, 125 a newer release
(self-update --check).`,
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
		&cobra.Group{ID: "supervising", Title: "Supervising:"},
		&cobra.Group{ID: "guarding", Title: "Guarding:"},
	)
	for _, c := range []*cobra.Command{a.sessionsCmd(), a.tailCmd(), a.snapshotCmd(), a.watchCmd(), a.budgetCmd()} {
		c.GroupID = "watching"
		root.AddCommand(c)
	}
	for _, c := range []*cobra.Command{a.leaseCmd(), a.holdCmd()} {
		c.GroupID = "sharing"
		root.AddCommand(c)
	}
	for _, c := range []*cobra.Command{a.supervisorCmd(), a.agentsCmd(), a.noteCmd(), a.handoverCmd(), a.logCmd()} {
		c.GroupID = "supervising"
		root.AddCommand(c)
	}
	for _, c := range []*cobra.Command{a.runCmd(), a.hookCmd(), a.freeCmd()} {
		c.GroupID = "guarding"
		root.AddCommand(c)
	}
	root.AddCommand(a.selfUpdateCmd(), a.versionCmd())
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
// the --as name.
func (a *app) caller() (state.Party, error) {
	if a.as != "" {
		return state.Party{Name: a.as}, nil
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
		p.Name = p.Session
	}
	return p, nil
}

// sessions reads the process table and the running sessions.
func (a *app) sessions() ([]*claude.Session, *proc.Table, error) {
	t, err := proc.Read()
	if err != nil {
		return nil, nil, err
	}
	return claude.Discover(a.cfg, t, a.now), t, nil
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
