package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/free"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/proc"
)

func (a *app) freeCmd() *cobra.Command {
	var (
		o                                  free.Options
		only                               []string
		staleH, orphanM, runawayM, runaway int
	)
	c := &cobra.Command{
		Use:   "free [--apply] [--only SECTIONS] [--summary]",
		Short: "Report what holds the memory and free what is stale",
		Long: `free shows where the memory is and, with --apply, frees what can be
freed without touching running work (the sections --only picks from):

  session-dirs  the dirs of dead Claude Code sessions in the tmpfs /tmp
                (<tmp>/claude-<uid>/<project>/<session>): the session's CLI
                is gone, transcript and files untouched for --stale-hours,
                no process inside. A live session's dirs stay however idle
                it is: it still reads them when it resumes.
  tmp-dirs      throwaway temp dirs in /tmp (vm-manager e2e, go build,
                mktemp, pytest, jest's cache) untouched for --stale-hours
  orphans       jest workers and Claude CLIs whose parent died, older than
                --orphan-minutes (TERM, then KILL)
  swap          the swap reset, when RAM can take the swapped pages back

What only a person decides on is reported with the memory the action would
return, never touched: kind clusters, idle Claude CLIs, heavy or runaway
processes and Chrome renderers.

free never runs as root. What needs root, the swap reset and root-owned
leftovers, is printed at the end as the commands to run (` + free.SwapResetCmd + `).

--summary prints one tab-separated row per candidate instead of the report,
for a front end that lets the user pick (a dry run):

  section <key> <MiB> <count> <label>    what --apply --only <key> frees
                                         (swap: what the reset pulls back)
  kind    <name> <MiB> <up since>        a kind cluster
  cli     <pid> <MiB> <idle min> <cwd>   an idle Claude CLI
  proc    <pid> <MiB> <up> <comm> <unit:NAME | cmdline> <why>
                                         a heavy or runaway process; MiB is
                                         its anonymous RSS, what a kill
                                         returns; unit:NAME is a systemd user
                                         service (stop the unit: Restart=
                                         undoes a kill); why is heavy,
                                         runaway:<cpu %> or heavy,runaway:<cpu %>
  tab     <pid> <MiB> <up> renderer      a Chrome renderer above --tab-mib`,
		Args: cobra.NoArgs,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return a.loadConfig()
		},
		RunE: func(*cobra.Command, []string) error {
			if os.Geteuid() == 0 {
				return refused("free never runs as root: run it as yourself, it prints the commands that need root")
			}
			var err error
			if o.Only, err = free.ParseOnly(only); err != nil {
				return usageErr("--only: %v", err)
			}
			if o.Summary {
				o.Apply = false
			}
			o.Stale = time.Duration(staleH) * time.Hour
			o.Orphan = time.Duration(orphanM) * time.Minute
			o.Runaway = time.Duration(runawayM) * time.Minute
			o.RunawayCPU = runaway
			m, err := a.freeMachine()
			if err != nil {
				return err
			}
			(&free.Run{Options: o, Machine: m, Out: a.out}).Do()
			return nil
		},
	}
	f := c.Flags()
	f.BoolVar(&o.Apply, "apply", false, "delete and kill; without it free only reports")
	f.StringSliceVar(&only, "only", nil, "act only on these sections (comma-separated: session-dirs, tmp-dirs, orphans, swap)")
	f.BoolVar(&o.Summary, "summary", false, "print the candidates as TSV instead of the report; a dry run")
	f.IntVar(&staleH, "stale-hours", 3, "hours a session's dirs or a temp dir must be untouched to count as stale")
	f.IntVar(&orphanM, "orphan-minutes", 30, "age after which an orphaned worker is killed")
	f.IntVar(&o.HeavyMiB, "heavy-mib", 500, "anonymous RSS from which a process is reported as heavy (0 turns it off)")
	f.IntVar(&runaway, "runaway-cpu", 50, "share of its lifetime (percent) a runaway process spent on the CPU")
	f.IntVar(&runawayM, "runaway-minutes", 10, "CPU minutes a runaway process has burned")
	f.IntVar(&o.TabMiB, "tab-mib", 250, "anonymous RSS from which a Chrome renderer is reported (0 turns it off)")
	return c
}

// freeMachine gathers what free reads: the process table, the sessions and
// the kind clusters.
func (a *app) freeMachine() (free.Machine, error) {
	a.now = time.Now()
	t, err := proc.Read()
	if err != nil {
		return free.Machine{}, err
	}
	home, _ := os.UserHomeDir()
	uid := os.Getuid()
	tmp := os.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	clusters, cerr := machine.KindClusters(ctx)
	return free.Machine{
		UID:         uid,
		Home:        home,
		TmpDir:      tmp,
		Scratch:     filepath.Join(tmp, "claude-"+strconv.Itoa(uid)),
		Projects:    a.cfg.Claude.ProjectsDir,
		Table:       t,
		Sessions:    claude.Discover(a.cfg, t, a.now),
		Titles:      func() map[string]string { return claude.Titles(a.cfg) },
		Clusters:    clusters,
		ClustersErr: cerr,
		SlotDir:     a.cfg.Memcap.SlotDir,
		Slots:       a.cfg.Memcap.Slots,
		Now:         a.now,
	}, nil
}
