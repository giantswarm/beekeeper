package cmd

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/runrecord"
	"github.com/giantswarm/beekeeper/internal/state"
)

// runStat is one repository's share of the window: what ran where, how long
// it took, how often it failed, and how often the remote attempt fell back
// to the local slot. A builder pod's warm cache is counted too: it is what
// the shared cache (issue #1782) must measurably beat.
type runStat struct {
	Repo      string  `json:"repo"`
	Local     int     `json:"local"`
	Remote    int     `json:"remote"`
	Fallback  int     `json:"fallback"`
	Failed    int     `json:"failed"`
	Minutes   float64 `json:"minutes"`
	WarmCache int     `json:"warmCache"`
}

func (r runStat) rate() string {
	if r.Local+r.Remote == 0 {
		return "-"
	}
	return fmt.Sprintf("%d%%", 100*r.Failed/(r.Local+r.Remote))
}

// runStatsCmd answers "how many builds ran remotely vs locally, how long did
// they take, did the cache help" from the event log alone (issue #1937):
// every beekeeper run records run.start / run.end, local and remote alike.
func (a *app) runStatsCmd() *cobra.Command {
	var since string
	c := &cobra.Command{
		Use:   "stats [--since WHEN]",
		Short: "Local vs remote build runs from the event log: counts, minutes, failures, cache",
		Long: `stats reads the machine's run records (beekeeper log --verb run.) of a
window and counts, per repository: the runs that took a local slot, the runs
that ran in a builder pod, the remote attempts that fell back to the local
slot before starting (their reasons read from the log), the failed runs and
the minutes the runs took. warm names the remote runs whose builder pod was
already there with its build caches, cold the ones that placed a fresh pod:
the shared cache (issue #1782) must push cold toward zero.

A run of an older release, or one outside a GitHub worktree, has no
repository in its record and counts under "-".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// `run` loads the configuration only, so the store may be
			// unopened here: load fully, like the log command's own
			// pre-run does.
			st := a.store
			if st == nil {
				if err := a.load(); err != nil {
					return err
				}
				st = a.store
			}
			from := time.Time{}
			var err error
			if since != "" {
				if from, err = reportTime(a.now, since, time.Local); err != nil {
					return err
				}
			}
			// The whole log, not only the window: an end names its repository
			// through the start that opened it, and a long run started before
			// --since still belongs to the window it ended in.
			evs, err := st.Events(0, func(e state.Event) bool { return strings.HasPrefix(e.Verb, "run.") })
			if err != nil {
				return err
			}
			by := map[string]*runStat{}
			// owner and cache carry a start's facts to its end: the run's
			// repository, and whether the builder pod it ran in was warm.
			// Both are read on the start of any age and counted when the
			// end is: a long build crossing --since counts once, where it
			// completed, and a run still going counts nowhere. The cache
			// state reads from the start's facts, never the command: test
			// names are output, not cache state.
			owner := map[string]string{}
			cache := map[string]bool{}
			at := func(repo string) *runStat {
				if by[repo] == nil {
					by[repo] = &runStat{Repo: repo}
				}
				return by[repo]
			}
			for _, e := range evs {
				repo := runrecord.Repository(e.Detail)
				switch e.Verb {
				case runrecord.Start:
					id := runrecord.ID(e.Detail)
					if repo != "" {
						owner[id] = repo
					}
					if warm, ok := runrecord.Warm(e.Detail); ok {
						cache[id] = warm
					}
				case runrecord.Fallback:
					if !from.IsZero() && e.At.Before(from) {
						continue
					}
					at(repo).Fallback++
				case runrecord.End:
					if !from.IsZero() && e.At.Before(from) {
						continue
					}
					id := runrecord.ID(e.Detail)
					if owner[id] != "" {
						repo = owner[id]
					}
					rc, took, ok := runrecord.ParseEnd(e.Detail)
					if !ok {
						continue
					}
					r := at(repo)
					if runrecord.Remote(e.Detail) {
						r.Remote++
						if cache[id] {
							r.WarmCache++
						}
					} else {
						r.Local++
					}
					r.Minutes += took.Minutes()
					if rc != 0 {
						r.Failed++
					}
				}
			}
			rows := make([]runStat, 0, len(by))
			total := runStat{Repo: "TOTAL"}
			for _, r := range by {
				rows = append(rows, *r)
				total.Local += r.Local
				total.Remote += r.Remote
				total.Fallback += r.Fallback
				total.Failed += r.Failed
				total.Minutes += r.Minutes
				total.WarmCache += r.WarmCache
			}
			sort.Slice(rows, func(i, j int) bool {
				if rows[i].Repo == "" {
					return false
				}
				if rows[j].Repo == "" {
					return true
				}
				return rows[i].Repo < rows[j].Repo
			})
			if a.json {
				return a.printJSON(map[string]any{"since": from, "repositories": rows, "total": total})
			}
			w := a.table()
			_, _ = fmt.Fprintln(w, "repository\tlocal\tremote\tfallback\tfailed\trate\tminutes\twarm cache")
			name := func(repo string) string {
				if repo == "" {
					return "-"
				}
				return repo
			}
			for _, r := range rows {
				_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%s\t%.1f\t%d\n", name(r.Repo), r.Local, r.Remote, r.Fallback, r.Failed, r.rate(), r.Minutes, r.WarmCache)
			}
			_, _ = fmt.Fprintf(w, "TOTAL\t%d\t%d\t%d\t%d\t%s\t%.1f\t%d\n", total.Local, total.Remote, total.Fallback, total.Failed, total.rate(), total.Minutes, total.WarmCache)
			return w.Flush()
		},
	}
	c.Flags().StringVar(&since, "since", "", "only the runs from this on: a duration before now (1h, 30d), a time (15:04), a date (2006-01-02) or RFC 3339 (default: the whole log)")
	return c
}
