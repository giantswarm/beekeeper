package cmd

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/gate"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

type poller struct {
	PID     int           `json:"pid"`
	Session string        `json:"session,omitempty"`
	Args    string        `json:"args"`
	Elapsed time.Duration `json:"elapsed"`
}

func (a *app) budgetCmd() *cobra.Command {
	var gateFlag bool
	var floor int
	var buckets []string
	c := &cobra.Command{
		Use:   fatalBudgetCommand,
		Short: "The GitHub REST, GraphQL and search budget every session draws from, and who is drawing",
		Long: `Read the stored gh login and the configured devctl App login explicitly,
independent of the working directory or a gh link. Name both identities and
their account logins, counters and reset times (JSON identities). Cache each
identity separately; the top-level REST and GraphQL counters each select the
lowest balance. Unreadable App provider metadata falls back to the stored gh
login with a warning, retaining known unexpired App observations. Unreadable
credentials or GitHub probes hold the gate, as does any refusal.

Read each identity's GitHub core budget from the rate-limit headers of a real,
conditional request, shared for 30 seconds across loop samples and commands
(a 304 costs nothing; /rate_limit is exempt and lies),
and the GraphQL limit from a real rateLimit query (one point per identity, read again
after merge.budgetFresh), the search and code-search limits from
/rate_limit (free; a spent one names its reset), and list the gh and devctl processes on the
machine with the session each runs under: the callers drawing on it. REST
window callers include admitted worker shell calls and native beekeeper calls
by source function, retained after processes exit. Counts are invocations,
not measured requests (pagination and retries can spend more); arguments,
endpoints and credentials are never recorded. Recorded
background invocations also share a durable allowance, capped at 2000 or
less than half the observed REST limit. Unknown windows admit only 100
bootstrap calls. Background reads stop at the shared floor or half the
window's limit, whichever protects more requests. Worker calls keep their
existing gate. The background_rest ledger lists each source's calls;
pagination and retries mean invocation counts are not measured REST points.
Repository default-branch commits are cached for an hour in each process;
file reads cache for a minute, then refresh using ETags. Missing files cache
for five minutes. Caches are bounded and scoped by repository, ref, path and
credential routing metadata. Recorded
GraphQL invocations name sessions and command classes in the current limit
window (reset minus one hour), heavy calls first, even after a process exits.
With no active reset known, show the last hour. Counts are invocations, not
GraphQL points. A
GraphQL refusal shows even while its counter looks healthy (a secondary
limit), with the time it ends when GitHub names one. The budget is shared
with the person's own logins, the developer portal among them: at zero it
signs them out. The urgent merge of the reset window (lanes urgent), the
one that ran under the floor or the one marked and waiting, is a line with
who asked for it and what its run drew against its own bound. Each session's
gated merges are a line: those whose devctl polls GitHub (at most
merge.sessionCap) and those queued in their lanes, which read nothing.

For merge.resetFirst (5m) after the budget's reset, the merges queued under
the floor for it go first: a line names them, and the gated waits and the
watch's GitHub timer checks start after them.

--gate bounds probes and credential discovery to seconds and skips plugin
synchronization and caller diagnostics. A busy shared probe lock falls back
to direct REST and GraphQL probes after 500 ms, with a warning; that reading
is not saved out of order. Known unexpired low windows still apply. It exits 3 when GitHub refuses
GraphQL calls, the budget is under the
floor (github.floor, default 2500), merges queued for the reset go first or
a "github" hold is set:
` + "`beekeeper budget --gate && gh …`" + `. --bucket names the search
bucket a step spends (code_search, search; repeatable or comma-separated):
--gate then also exits 3 while that bucket is spent, naming its reset, and
fails (exit 1) when the search limits cannot be read, as it does for an
unread core budget:
` + "`beekeeper budget --gate --bucket code_search && gh search code …`" + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			if a.managedExecutionID() != "" {
				return a.remoteGitHubBudget(ctx, workerBudgetRequest{Gate: gateFlag, Buckets: buckets, JSON: a.json})
			}
			if floor > 0 {
				a.cfg.GitHub.Floor = floor
			}
			if err := checkBuckets(gateFlag, buckets); err != nil {
				return err
			}
			if gateFlag {
				ctx = context.WithValue(ctx, budgetGateKey{}, true)
			}
			b, err := readBudget(a, ctx)
			if err = gate.UnsavedBudget(err); err != nil {
				if !a.json {
					for _, warning := range b.Warnings {
						_, _ = fmt.Fprintf(a.out, "WARNING: %s\n", warning)
					}
				}
				if a.json {
					_ = a.printJSON(b)
				}
				return err
			}
			if !gateFlag || len(buckets) > 0 {
				b.Search = probeSearch(ctx)
			}
			st, err := a.store.Peek()
			if err != nil {
				return err
			}
			var pollers []poller
			var callersErr error
			if !gateFlag {
				sessions, t, err := a.sessions()
				callersErr = err
				if err == nil {
					pollers = githubCallers(a, t, sessions)
				}
			}
			var callers, restCallers []githubCaller
			var callsErr, restErr error
			if !gateFlag {
				callers, callsErr = githubWindowCallers(a, b.GraphQL)
				restCallers, restErr = githubResourceCallers(a, b.Reset, verbGitHubRESTCall)
			}
			hold, held := activeHold(st, a, "github")
			urgent := urgentLines(a, st)
			gated := gatedMerges(st, proc.Alive)
			window := a.cfg.Merge.ResetFirst.Duration
			first := merge.ResetFirst(st, a.now, window, proc.Alive)
			firstLine := ""
			if len(first) > 0 {
				firstLine = fmt.Sprintf("merges first: %s, queued for the reset, go before the waits until they start, at %s at the latest (merge.resetFirst)",
					merge.Keys(first), clock(a.now, merge.ResetFirstUntil(first, window)))
			}
			var loopSpend loopGraphQLSpend
			_, _ = a.store.ReadFile(loopGraphQLFile, &loopSpend)
			var restSpend loopRESTSpend
			_, _ = a.store.ReadFile(loopRESTFile, &restSpend)
			var loopRESTPtr *loopRESTSpend
			var backgroundSpend backgroundRESTSpend
			_, _ = a.store.ReadFile(backgroundRESTFile, &backgroundSpend)
			var backgroundPtr *backgroundRESTSpend
			if a.now.Before(backgroundSpend.Reset) {
				backgroundPtr = &backgroundSpend
			}
			if !restSpend.Reset.IsZero() && a.now.Before(restSpend.Reset) && (restSpend.Spent > 0 || len(restSpend.Steps) > 0) {
				loopRESTPtr = &restSpend
			}
			if a.json {
				_ = a.printJSON(struct {
					github.Budget
					Callers        []githubCaller       `json:"callers"`
					RESTCallers    []githubCaller       `json:"rest_callers"`
					Loop           *loopGraphQLSpend    `json:"loop,omitempty"`
					LoopREST       *loopRESTSpend       `json:"loop_rest,omitempty"`
					BackgroundREST *backgroundRESTSpend `json:"background_rest,omitempty"`
					Floor          int                  `json:"floor"`
					Held           bool                 `json:"held"`
					Urgent         []state.Urgent       `json:"urgent,omitempty"`
					Gated          []gatedParty         `json:"gated,omitempty"`
					Pollers        []poller             `json:"pollers"`
				}{b, callers, restCallers, &loopSpend, loopRESTPtr, backgroundPtr, a.cfg.GitHub.Floor, held, st.Urgent, gated, pollers})
			} else {
				for _, warning := range b.Warnings {
					_, _ = fmt.Fprintf(a.out, "WARNING: %s\n", warning)
				}
				_, _ = fmt.Fprintln(a.out, budgetLine(a, b))
				var identities []string
				for id := range b.Identities {
					identities = append(identities, id)
				}
				slices.Sort(identities)
				for _, id := range identities {
					profile := b.Identities[id]
					_, _ = fmt.Fprintf(a.out, "identity %s (%s): %s\n", id, profile.Login, budgetLine(a, profile))
				}
				for _, l := range searchLines(a, b.Search) {
					_, _ = fmt.Fprintln(a.out, l)
				}
				if l := loopSpendLine(a, loopSpend); l != "" {
					_, _ = fmt.Fprintln(a.out, l)
				}
				if l := loopRESTSpendLine(a, restSpend); l != "" {
					_, _ = fmt.Fprintln(a.out, l)
				}
				if backgroundPtr != nil {
					_, _ = fmt.Fprintf(a.out, "background REST invocations: %d of %d, resets %s\n", backgroundSpend.Calls, backgroundSpend.Limit, clock(a.now, backgroundSpend.Reset))
				}
				if l := a.heldLine(loopSpend); l != "" {
					_, _ = fmt.Fprintln(a.out, l)
				}
				for _, l := range urgent {
					_, _ = fmt.Fprintln(a.out, l)
				}
				for _, g := range gated {
					_, _ = fmt.Fprintln(a.out, g.line(a.cfg.Merge.SessionCap))
				}
				if firstLine != "" {
					_, _ = fmt.Fprintln(a.out, firstLine)
				}
				if held {
					_, _ = fmt.Fprintf(a.out, "GitHub is held by %q until %s: %s\n", hold.By.Name, untilText(a, hold), hold.Reason)
				}
				if restErr != nil {
					_, _ = fmt.Fprintf(a.out, "REST window callers unknown: %v\n", restErr)
				} else if len(restCallers) > 0 {
					_, _ = fmt.Fprintf(a.out, "REST window callers (invocations, not measured requests): %s\n", githubCallersText(restCallers))
				}
				if callsErr != nil {
					_, _ = fmt.Fprintf(a.out, "GraphQL window callers unknown: %v\n", callsErr)
				} else if len(callers) > 0 {
					_, _ = fmt.Fprintf(a.out, "GraphQL window callers: %s\n", githubCallersText(callers))
				}
				if callersErr != nil {
					_, _ = fmt.Fprintf(a.out, "callers unknown: %v\n", callersErr)
				}
				if len(pollers) > 0 {
					_, _ = fmt.Fprintf(a.out, "%d gh/devctl processes:\n", len(pollers))
					w := a.table()
					for _, p := range pollers {
						owner := p.Session
						if owner == "" {
							owner = noSession
						}
						_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\n", dur(p.Elapsed), truncate(p.Args, 70), truncate(owner, 40))
					}
					_ = w.Flush()
				}
			}
			if gateFlag && held {
				return refused("GitHub is held: %s", hold.Reason)
			}
			if gateFlag && b.GraphQL.Blocks(a.now) {
				return refused("GitHub refuses GraphQL calls: %s", graphqlText(a, b.GraphQL))
			}
			if gateFlag && b.GraphQL != nil && b.GraphQL.Remaining < a.cfg.GitHub.Floor {
				return refused("GraphQL identity %s budget %d is under the floor %d: wait for its reset at %s", b.GraphQL.Identity, b.GraphQL.Remaining, a.cfg.GitHub.Floor, clock(a.now, b.GraphQL.Reset))
			}
			if gateFlag && b.Remaining < a.cfg.GitHub.Floor {
				return refused("GitHub budget %d is under the floor %d: wait for the reset at %s", b.Remaining, a.cfg.GitHub.Floor, clock(a.now, b.Reset))
			}
			if gateFlag && firstLine != "" {
				return refused("%s", firstLine)
			}
			if gateFlag {
				return gateBuckets(a, b.Search, buckets)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&gateFlag, "gate", false, "exit 3 when GraphQL is refused, under the floor or held")
	c.Flags().IntVar(&floor, "floor", 0, "the floor for this call (default github.floor)")
	c.Flags().StringSliceVar(&buckets, "bucket", nil, "with --gate, also exit 3 while this search bucket is spent (repeatable or comma-separated: "+strings.Join(searchBuckets, ", ")+")")
	return c
}

// searchBuckets are the names --bucket takes, those of /rate_limit.
var searchBuckets = []string{"code_search", "search"}

// searchBucket is the named bucket of s: nil when GitHub sent none.
func searchBucket(s *github.Searches, name string) *github.Limit {
	if name == "code_search" {
		return s.CodeSearch
	}
	return s.Search
}

func checkBuckets(gate bool, buckets []string) error {
	if len(buckets) > 0 && !gate {
		return usageErr("--bucket needs --gate")
	}
	for _, n := range buckets {
		if !slices.Contains(searchBuckets, n) {
			return usageErr("unknown --bucket %q (want %s)", n, strings.Join(searchBuckets, ", "))
		}
	}
	return nil
}

// gateBuckets refuses while a named bucket is spent. Limits that could not
// be read fail the gate as an unread core budget does: the step does not run.
func gateBuckets(a *app, s *github.Searches, buckets []string) error {
	if len(buckets) == 0 {
		return nil
	}
	if s == nil || s.Err != "" {
		why := "not read"
		if s != nil {
			why = s.Err
		}
		return fmt.Errorf("search limits unknown, the gate cannot judge --bucket: %s", why)
	}
	for _, n := range buckets {
		l := searchBucket(s, n)
		if l == nil {
			return fmt.Errorf("GitHub sent no %s limit, the gate cannot judge it", n)
		}
		if l.Spent(a.now) {
			return refused("GitHub %s bucket is spent, 0 of %d left: wait for the reset at %s (in %s)",
				n, l.Limit, clock(a.now, l.Reset), dur(l.Reset.Sub(a.now)))
		}
	}
	return nil
}

// probeSearch reads the search limits; a failure is their Err.
func probeSearch(ctx context.Context) *github.Searches {
	result := &github.Searches{}
	minimum := func(old, next *github.Limit) *github.Limit {
		if old == nil || next != nil && next.Remaining < old.Remaining {
			return next
		}
		return old
	}
	for _, credential := range budgetCredentials(ctx) {
		if credential.Skip {
			continue
		}
		if credential.Err != nil {
			return &github.Searches{Err: fmt.Sprintf("identity %s: %v", credential.Identity, credential.Err)}
		}
		s := github.ProbeSearch(ctx, &http.Client{Timeout: 20 * time.Second}, credential.Value)
		if s.Err != "" {
			return &github.Searches{Err: s.Err + " [identity " + credential.Identity + "]"}
		}
		result.Search, result.CodeSearch = minimum(result.Search, s.Search), minimum(result.CodeSearch, s.CodeSearch)
	}
	return result
}

func loopSpendLine(a *app, s loopGraphQLSpend) string {
	if s.Spent == 0 && len(s.Steps) == 0 {
		return ""
	}
	var steps []string
	for step, points := range s.Steps {
		steps = append(steps, fmt.Sprintf("%s %d", step, points))
	}
	slices.Sort(steps)
	line := fmt.Sprintf("loop GraphQL spend: %d of %d", s.Spent, a.loopGraphQLCap())
	if len(steps) > 0 {
		line += " (" + strings.Join(steps, ", ") + ")"
	}
	if !s.Reset.IsZero() {
		line += ", resets " + clock(a.now, s.Reset)
	}
	return line
}

func loopRESTSpendLine(a *app, s loopRESTSpend) string {
	if s.Reset.IsZero() || !a.now.Before(s.Reset) || (s.Spent == 0 && len(s.Steps) == 0) {
		return ""
	}
	var steps []string
	for step, requests := range s.Steps {
		if requests > 0 {
			steps = append(steps, fmt.Sprintf("%s %d", step, requests))
		}
	}
	slices.Sort(steps)
	line := fmt.Sprintf("loop REST spend: %d of %d", s.Spent, s.allowance(a.cfg.GitHub.Floor))
	if len(steps) > 0 {
		line += " (" + strings.Join(steps, ", ") + ")"
	}
	line += ", resets " + clock(a.now, s.Reset)
	return line
}

// searchLines are a row per search limit: its figures and reset, or that it
// is spent and until when.
func searchLines(a *app, s *github.Searches) []string {
	if s == nil {
		return nil
	}
	if s.Err != "" {
		return []string{"search limits unknown (" + truncate(s.Err, 60) + ")"}
	}
	var out []string
	for _, r := range []struct {
		name string
		l    *github.Limit
	}{{"code search", s.CodeSearch}, {"search", s.Search}} {
		if r.l == nil {
			continue
		}
		reset := fmt.Sprintf("%s (in %s)", clock(a.now, r.l.Reset), dur(r.l.Reset.Sub(a.now)))
		if r.l.Spent(a.now) {
			out = append(out, fmt.Sprintf("%s: SPENT, 0 of %d left until %s", r.name, r.l.Limit, reset))
			continue
		}
		out = append(out, fmt.Sprintf("%s: %d of %d left, resets %s", r.name, r.l.Remaining, r.l.Limit, reset))
	}
	return out
}

// gatedParty is one party's gated merges: those whose devctl runs and polls
// GitHub, and those waiting in their lanes, which read nothing.
type gatedParty struct {
	Party   string   `json:"party"`
	Polling []string `json:"polling,omitempty"`
	Waiting []string `json:"waiting,omitempty"`
}

// line says what the party's gated merges draw on the budget.
func (g gatedParty) line(most int) string {
	l := fmt.Sprintf("gated merges of %q: %d polling GitHub", g.Party, len(g.Polling))
	if len(g.Polling) > 0 {
		l += " (" + strings.Join(g.Polling, ", ") + ")"
	}
	if len(g.Waiting) > 0 {
		l += fmt.Sprintf(", %d queued without reading it (%s)", len(g.Waiting), strings.Join(g.Waiting, ", "))
	}
	if most > 0 {
		l += fmt.Sprintf(", merge.sessionCap %d", most)
	}
	return l
}

// gatedMerges groups the gate's merges by party: the running ones whose
// gate or devctl is alive poll, the waiting ones whose gate is alive do not.
func gatedMerges(st *state.State, alive func(pid int) bool) []gatedParty {
	var out []gatedParty
	for _, m := range st.Merges {
		polling := m.Phase == state.Running && merge.Runs(m, alive)
		if !polling && (m.Phase != state.Waiting || !alive(m.PID)) {
			continue
		}
		i := slices.IndexFunc(out, func(g gatedParty) bool { return g.Party == m.By.Name })
		if i < 0 {
			out = append(out, gatedParty{Party: m.By.Name})
			i = len(out) - 1
		}
		if polling {
			out[i].Polling = append(out[i].Polling, m.Key())
		} else {
			out[i].Waiting = append(out[i].Waiting, m.Key())
		}
	}
	return out
}

// githubCallers are the gh and devctl processes drawing on the budget, with
// the session each runs under: longest-running first.
func githubCallers(a *app, t *proc.Table, sessions []*claude.Session) []poller {
	var out []poller
	for _, p := range t.ByPID {
		if p.Comm != "gh" && p.Comm != merge.Tool {
			continue
		}
		pl := poller{PID: p.PID, Args: p.Cmdline(), Elapsed: p.Elapsed(a.now).Round(time.Second)}
		if s, ok := claude.OwnerOf(sessions, p.PID); ok {
			pl.Session = s.Name
		}
		out = append(out, pl)
	}
	slices.SortFunc(out, func(x, y poller) int { return int(y.Elapsed - x.Elapsed) })
	return out
}
