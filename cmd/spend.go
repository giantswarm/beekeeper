package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/omp"
)

func (a *app) spendCmd() *cobra.Command {
	var since, until, tz string
	c := &cobra.Command{
		Use:   "spend",
		Short: "What the Claude Code and omp sessions' API requests of a window cost, from the whole transcripts",
		Long: `spend prices every API request from --since up to --until in the
transcripts under claude.projectsDir and the session files under
omp.sessionsDir: the sessions' and their subagents' .jsonl files, read
whole, of running, paused and archived sessions alike. A request counts
once by its id, however many transcripts carry it. Prices come from
metrics.models; an omp model is named provider/model and takes the price
of that name or else of its model. A model without one is named as
unpriced, its requests and tokens counted and left out of the total.

A bare date (2026-10-08) is that day's 00:00 in --tz, for --since and
--until alike: --until 2026-10-09 ends the window as October 9 begins, so
the day it names is left out.

sessions shows the cost of each live session's last 512 KiB of transcript
only; spend is the figure for a day or any other window.`,
		Example: `  beekeeper spend                                  # today since 00:00
  beekeeper spend --since 2026-10-08 --until 2026-10-09   # the whole of October 8
  beekeeper spend --since 2026-10-08T00:00:00+02:00 --until 00:00`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			zone, err := reportZone(tz, a.zone)
			if err != nil {
				return usageErr("--tz: %v", err)
			}
			to := a.now
			if until != "" {
				if to, err = reportTime(a.now, until, zone); err != nil {
					return err
				}
			}
			from, err := reportTime(to, since, zone)
			if err != nil {
				return err
			}
			if !from.Before(to) {
				return usageErr("--since %s is not before --until %s", from.In(zone).Format(time.RFC3339), to.In(zone).Format(time.RFC3339))
			}
			ts, err := claude.ReadSpendTranscripts(a.cfg.Claude.ProjectsDir, from, to)
			if err != nil {
				return err
			}
			s := claude.PriceSpend(append(ts, omp.SpendTranscripts(a.cfg.Omp.SessionsDir, from, to)...), from, to, a.cfg.Metrics)
			if a.json {
				return a.printJSON(s)
			}
			_, err = fmt.Fprint(a.out, spendText(s, zone))
			return err
		},
	}
	c.Flags().StringVar(&since, "since", "00:00", "the window's start: a duration before --until (24h), a time (15:04), a date (2006-01-02: its 00:00) or RFC 3339")
	c.Flags().StringVar(&until, "until", "", "the window's end: a time (15:04), a date (2006-01-02: its 00:00, the day itself left out) or RFC 3339; default now")
	c.Flags().StringVar(&tz, "tz", "", "the time zone of 15:04, of a date and of the output (IANA, Europe/Berlin); default the machine's")
	return c
}

// spendText renders a spend: the total, then one line per model.
func spendText(s claude.Spend, zone *time.Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s to %s: $%.2f, %d requests in %d transcripts of %d sessions, %s tokens\n",
		s.From.In(zone).Format(time.RFC3339), s.To.In(zone).Format(time.RFC3339), s.USD, s.Requests, s.Transcripts, s.Sessions, tokensText(s.Tokens.Sum()))
	for _, m := range s.Models {
		cost := fmt.Sprintf("$%.2f", m.USD)
		if !m.Priced {
			cost = "unpriced"
		}
		fmt.Fprintf(&b, "  %s: %s, %d requests, %s in, %s cache write, %s cache read, %s out\n", m.Model, cost, m.Requests,
			tokensText(m.Tokens.Input), tokensText(m.Tokens.CacheWrite5m+m.Tokens.CacheWrite1h), tokensText(m.Tokens.CacheRead), tokensText(m.Tokens.Output))
	}
	if len(s.Unpriced) > 0 {
		fmt.Fprintf(&b, "unpriced, left out of the total (metrics.models): %s\n", strings.Join(s.Unpriced, ", "))
	}
	return b.String()
}
