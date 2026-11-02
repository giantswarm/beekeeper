package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) statusCmd() *cobra.Command {
	var bar bool
	c := &cobra.Command{
		Use:   "status [--bar]",
		Short: "One line: the supervisor, the leases held, the holds, what is due and the busy agents",
		Long: `Print the machine's coordination state in one line: whether the
supervisor loop runs, the
leases held, the holds in force, the notes and timers that are due and the
busy agents against capacity.floor and capacity.ceiling; while beekeeper
stop stands, the line starts with STOPPED and the stop's reason.

--bar prints it for a desktop bar as one tab-separated row, the same shape
as the rows of ` + "`free --summary`" + `, so one bar module reads both. The
contract is fixed: six fields, always present, in this order:

  beekeeper  <supervisor>  <leases>  <holds>  <due>  <busy>

  supervisor  loop while the supervisor loop runs; !loop when it does
              not
  leases      the number of leases held
  holds       the number of holds in force
  due         the number of notes and timers whose time has come
  busy        the busy agents against capacity.floor, <n>/<floor>
              (beekeeper capacity)

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
	// SupervisorLoop is the PID of the running supervisor loop, 0 when it
	// does not run.
	SupervisorLoop int      `json:"supervisorLoopPID"`
	Leases         []string `json:"leases"`
	Holds          []string `json:"holds"`
	// Lifted are the upgrade holds lifted while their upgrades run, with
	// who lifted them.
	Lifted []string `json:"lifted,omitempty"`
	Due    int      `json:"due"`
	// Busy are the busy agents, against Floor and Ceiling (capacity).
	Busy    int `json:"busy"`
	Floor   int `json:"floor"`
	Ceiling int `json:"ceiling"`
	// VaultWaits are the sessions whose calls wait on the person's unlock
	// of the vault.
	VaultWaits []string `json:"vaultWaits,omitempty"`
	// Stopped says the standing beekeeper stop, with its reason; empty
	// without one.
	Stopped string `json:"stopped,omitempty"`
	// Serving is the serving modes of the self-hosted providers: the mode
	// each runs in with the sessions it sustains, and each node
	// constraint held with the mode that stands while it holds.
	Serving string `json:"serving,omitempty"`
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
	k, _ := a.cachedUsageCapacity(a.liveCapacity())
	v := &statusView{Leases: []string{}, Holds: []string{}, Due: dueCount(st, a.now), Floor: k.Floor, Ceiling: k.Ceiling}
	if st.Stop != nil {
		v.Stopped = a.stopText(st.Stop)
	}
	sessions, _, err := a.sessions()
	missing := platform.Missing(err)
	if err != nil && !missing {
		return nil, err
	}
	v.Busy = len(countAgents(st, sessions, a.now).Busy)
	v.Serving = a.serveModeStatusLine()
	v.SupervisorLoop = a.loopPID()
	for _, h := range holders {
		v.Leases = append(v.Leases, h.Env)
	}
	for _, h := range st.Holds {
		if h.Active(a.now) {
			v.Holds = append(v.Holds, h.Target)
		}
	}
	for _, h := range liftedHolds(st, a.now) {
		v.Lifted = append(v.Lifted, h.Target+" by "+h.LiftedBy.Name)
	}
	if path, err := secret.WaitsPath(); err == nil && a.cfg.Secret.Session {
		ws, _ := secret.ReadWaits(path)
		for _, w := range ws {
			v.VaultWaits = append(v.VaultWaits, w.Who)
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
		if t.Looked(now) {
			n++
		}
	}
	return n
}

// bar is the fixed row of status --bar.
func (v *statusView) bar() string {
	sup := "!loop"
	if v.SupervisorLoop > 0 {
		sup = "loop"
	}
	fields := []string{"beekeeper", sup, fmt.Sprint(len(v.Leases)), fmt.Sprint(len(v.Holds)), fmt.Sprint(v.Due), fmt.Sprintf("%d/%d", v.Busy, v.Floor)}
	for i, f := range fields {
		fields[i] = strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(f)
	}
	return strings.Join(fields, "\t")
}

// line is status in words.
func (v *statusView) line() string {
	sup := "the supervisor loop does not run"
	if v.SupervisorLoop > 0 {
		sup = "the supervisor loop runs"
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
	holds := list(len(v.Holds), "hold", "holds", v.Holds)
	if len(v.Lifted) > 0 {
		holds += "; " + list(len(v.Lifted), "lifted", "lifted", v.Lifted)
	}
	line := fmt.Sprintf("%s; %s; %s; %d due; busy %d/%d-%d", sup, list(len(v.Leases), "lease held", "leases held", v.Leases), holds, v.Due, v.Busy, v.Floor, v.Ceiling)
	if v.Serving != "" {
		line += "; " + v.Serving
	}
	if len(v.VaultWaits) > 0 {
		line += "; vault locked: " + list(len(v.VaultWaits), "call waits", "calls wait", v.VaultWaits)
	}
	if v.Stopped != "" {
		line = "STOPPED: " + v.Stopped + "; " + line
	}
	return line
}
