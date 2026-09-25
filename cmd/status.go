package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) statusCmd() *cobra.Command {
	var bar bool
	c := &cobra.Command{
		Use:   "status [--bar]",
		Short: "One line: the supervisor, the leases held, the holds and what is due",
		Long: `Print the machine's coordination state in one line: who supervises, the
leases held, the holds in force and the notes and timers that are due.

--bar prints it for a desktop bar as one tab-separated row, the same shape
as the rows of ` + "`free --summary`" + `, so one bar module reads both. The
contract is fixed: five fields, always present, in this order:

  beekeeper  <supervisor>  <leases>  <holds>  <due>

  supervisor  the supervising session's name; - when none is recorded;
              ~<name> while its CLI restarts (the grant rule holds);
              !<name> when the recorded one's session no longer runs
  leases      the number of leases held
  holds       the number of holds in force
  due         the number of notes and timers whose time has come

The exit code is 0 whenever the state can be read.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			s, err := a.status()
			if err != nil {
				return err
			}
			switch {
			case a.json:
				return a.printJSON(s)
			case bar:
				_, err = fmt.Fprintln(a.out, s.bar())
			default:
				_, err = fmt.Fprintln(a.out, s.line())
			}
			return err
		},
	}
	c.Flags().BoolVar(&bar, "bar", false, "print the fixed tab-separated row for a desktop bar")
	return c
}

// statusView is what status shows.
type statusView struct {
	// Supervisor is the recorded supervisor's name, empty when none is.
	Supervisor string `json:"supervisor"`
	// SupervisorLive is whether its session runs.
	SupervisorLive bool `json:"supervisorLive"`
	// RestartUntil is set while its CLI restarts: the grant rule holds
	// until then.
	RestartUntil time.Time `json:"restartUntil,omitzero"`
	Leases       []string  `json:"leases"`
	Holds        []string  `json:"holds"`
	Due          int       `json:"due"`
}

func (a *app) status() (*statusView, error) {
	st, err := a.store.Read()
	if err != nil {
		return nil, err
	}
	holders, err := lease.Dir(a.cfg.LeaseDir).List()
	if err != nil {
		return nil, err
	}
	v := &statusView{Leases: []string{}, Holds: []string{}, Due: dueCount(st, a.now)}
	if st.Supervisor != nil {
		sessions, _, err := a.sessions()
		if err != nil {
			return nil, err
		}
		sv := a.supervision(st, sessions)
		v.Supervisor, v.SupervisorLive, v.RestartUntil = st.Supervisor.Name, sv.live, sv.until
	}
	for _, h := range holders {
		v.Leases = append(v.Leases, h.Env)
	}
	for _, h := range st.Holds {
		if h.Active(a.now) {
			v.Holds = append(v.Holds, h.Target)
		}
	}
	return v, nil
}

// dueCount is the number of notes and timers whose time has come.
func dueCount(st *state.State, now time.Time) int {
	n := 0
	for _, x := range st.Notes {
		if !x.Due.IsZero() && !x.Due.After(now) {
			n++
		}
	}
	for _, t := range st.Timers {
		if !t.Due.After(now) {
			n++
		}
	}
	return n
}

// bar is the fixed row of status --bar.
func (v *statusView) bar() string {
	sup := "-"
	switch {
	case v.Supervisor != "" && v.SupervisorLive:
		sup = v.Supervisor
	case v.Supervisor != "" && !v.RestartUntil.IsZero():
		sup = "~" + v.Supervisor
	case v.Supervisor != "":
		sup = "!" + v.Supervisor
	}
	fields := []string{"beekeeper", sup, fmt.Sprint(len(v.Leases)), fmt.Sprint(len(v.Holds)), fmt.Sprint(v.Due)}
	for i, f := range fields {
		fields[i] = strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(f)
	}
	return strings.Join(fields, "\t")
}

// line is status in words.
func (v *statusView) line() string {
	sup := "no supervisor"
	switch {
	case v.Supervisor != "" && v.SupervisorLive:
		sup = fmt.Sprintf("%q supervises", v.Supervisor)
	case v.Supervisor != "" && !v.RestartUntil.IsZero():
		sup = fmt.Sprintf("%q supervises (its CLI is restarting; the grant rule holds until %s)", v.Supervisor, stamp(v.RestartUntil))
	case v.Supervisor != "":
		sup = fmt.Sprintf("no supervisor (%q's session is gone)", v.Supervisor)
	}
	list := func(n int, one, many string, names []string) string {
		s := fmt.Sprintf("%d %s", n, many)
		if n == 1 {
			s = "1 " + one
		}
		if n > 0 {
			s += " (" + strings.Join(names, ", ") + ")"
		}
		return s
	}
	return fmt.Sprintf("%s; %s; %s; %d due", sup, list(len(v.Leases), "lease held", "leases held", v.Leases), list(len(v.Holds), "hold", "holds", v.Holds), v.Due)
}
