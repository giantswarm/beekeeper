package cmd

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/proc"
)

// psRow is one process as ps shows it, its command line masked.
type psRow struct {
	PID     int    `json:"pid"`
	PPID    int    `json:"ppid"`
	Age     string `json:"age"`
	CPU     string `json:"cpu"`
	RSSMiB  int    `json:"rssMiB"`
	Command string `json:"command"`
	// Masked says the command line lost words: values, not an empty line.
	Masked bool `json:"masked"`
}

func (a *app) psCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ps [name|pid...]",
		Short: "The process table with every command line masked",
		Long: `ps lists the machine's processes: PID, parent, age, CPU time, resident
memory and the command line, masked like the watch's STACKED line. A
command line keeps the program, the words before its first flag (its
subcommands) and the flag names; flag values, key=value words, URLs and
what follows a short option's letter are left out, and a masked line ends
in " (masked)" so that a left-out value is never read as an empty one.

A process's command line is readable by every process, and a program
started with -e PASSWORD=…, --token … or -p … carries the credential there.
The PreToolUse hook refuses the reads that print it whole (ps with its args
column, pgrep -a, pstree -a, /proc/<pid>/cmdline and environ, docker inspect)
and names this command instead.

Arguments narrow the list: a number is a PID, any other word matches the
masked command line or the process name. The match never sees an unmasked
value.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			t, err := plat.Machine.Processes()
			if err != nil {
				return err
			}
			rows := psRows(t, time.Now(), args)
			if a.json {
				return a.printJSON(rows)
			}
			w := a.table()
			_, _ = fmt.Fprintln(w, "PID\tPPID\tAGE\tCPU\tRSS\tCOMMAND")
			for _, r := range rows {
				cmd := r.Command
				if r.Masked {
					cmd += " (masked)"
				}
				_, _ = fmt.Fprintf(w, "%d\t%d\t%s\t%s\t%dM\t%s\n", r.PID, r.PPID, r.Age, r.CPU, r.RSSMiB, cmd)
			}
			return w.Flush()
		},
	}
}

// psRows are t's processes that match one of filters (all of them when
// there is none), by PID.
func psRows(t *proc.Table, now time.Time, filters []string) []psRow {
	var rows []psRow
	for _, pid := range slices.Sorted(maps.Keys(t.ByPID)) {
		p := t.ByPID[pid]
		cmd, cut := masked(p.Args)
		if cmd == "" {
			cmd = "[" + p.Comm + "]"
		}
		if !psMatch(p, cmd, filters) {
			continue
		}
		rows = append(rows, psRow{PID: p.PID, PPID: p.PPID, Age: dur(p.Elapsed(now)), CPU: dur(p.CPU),
			RSSMiB: p.RSSKiB / 1024, Command: cmd, Masked: cut})
	}
	return rows
}

func psMatch(p *proc.Process, cmd string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	for _, f := range filters {
		if pid, err := strconv.Atoi(f); err == nil {
			if pid == p.PID {
				return true
			}
			continue
		}
		if strings.Contains(cmd, f) || strings.Contains(p.Comm, f) {
			return true
		}
	}
	return false
}
