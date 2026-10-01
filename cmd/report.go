package cmd

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/post"
	"github.com/giantswarm/beekeeper/internal/state"
)

// reportFacts are what a status report says, read from beekeeper's state,
// its event log, the machine and one GitHub request.
type reportFacts struct {
	From, To time.Time
	Zone     *time.Location
	Person   string
	Merged   []reportMerge
	Workers  []reportWorker
	// Notes are the open notes for the person per owning session, in filing
	// order; Asking the sessions whose record waits on the person.
	Notes  []noteCount
	Asking []string
	Drafts []github.PR
	Lanes  []state.Merge
	Timers []state.Timer
	Holds  []state.Hold
	// Machine is the snapshot, its OOM kills those of the window.
	Machine *snapshot
	Alerts  []alertCount
	// Owners maps the repository names the report knows to their owners,
	// for the links of free text.
	Owners map[string]string
}

// reportMerge is a merge of the window: a pull request, or a promotion
// (N 0), with its release and rollout.
type reportMerge struct {
	github.PR
	Title, Release, Rollout string
}

// reportWorker is a running session: a role, a busy agent or one serving an
// issue.
type reportWorker struct {
	Name, Issue, Doing string
	Leases             []string
}

// noteCount is the open notes of one session for the person.
type noteCount struct {
	Owner, Issue string
	N            int
}

// alertCount is an installation's alerts: active, paging, or why it did not
// answer.
type alertCount struct {
	Installation   string
	Active, Paging int
	Unreachable    string
}

// sensitive is a name or text about credentials: the report names such a
// session by its issue only and leaves such a text out.
var sensitive = regexp.MustCompile(`(?i)secret|credential|password|passphrase|token|\bkeys?\b|rotat|keepass|1password|vault`)

// releaseWord is the release a merged event names.
var releaseWord = regexp.MustCompile(`\brelease (v?\d[\w.+-]*)`)

// conventional is a conventional commit subject's type and scope.
var conventional = regexp.MustCompile(`^\w+(\([^)]*\))?!?:\s*`)

// reportZone is the report's time zone: tz, else the machine's.
func reportZone(tz string, machineZone func() (*time.Location, error)) (*time.Location, error) {
	if tz != "" {
		return time.LoadLocation(tz)
	}
	if machineZone == nil {
		machineZone = machine.Zone
	}
	return machineZone()
}

func (a *app) reportCmd() *cobra.Command {
	var since, until, tz string
	c := &cobra.Command{
		Use:   "report",
		Short: "Render the status report: merges, running work, the person's queue, the machine",
		Long: `report renders the status report the scheduled reporter posts, from
beekeeper's own state, as Markdown that passes beekeeper reporter check:

- Merged & shipped: the pull requests and promotions the gate merged in the
  window (the merged events), with their title (one GitHub request), their
  release and their rollout (rolled, rolling, not rolled);
- Running: the supervisor, the guide, the busy agents and the sessions with
  a record, each with the issue it serves, what it waits on and its leases;
- Waiting on <person>: the open notes for reporter.person per owning
  session (counts, never their text), the sessions that wait on the person,
  the open draft pull requests of reporter.reviews;
- Queue: the merge lanes, the open timers and the holds;
- Machine: RAM and disk with a bar, load and pressure, swap, the kind labs,
  the window's OOM kills, the installations' alerts, the GitHub budget.

Every pull request and issue is a link to its repository, every time in
reporter.tz (--tz; default the machine's zone). A session whose name is
about credentials is named by its issue only.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			zone, err := reportZone(cmp.Or(tz, a.cfg.Reporter.TZ), nil)
			if err != nil {
				return usageErr("--tz: %v", err)
			}
			to := a.now
			if until != "" {
				if to, err = reportTime(a.now, until, zone); err != nil {
					return err
				}
			}
			window := cmp.Or(since, dur(cmp.Or(a.cfg.Reporter.Every.Duration, time.Hour)))
			from, err := reportTime(to, window, zone)
			if err != nil {
				return err
			}
			if !from.Before(to) {
				return usageErr("--since %s is not before --until %s", from.In(zone).Format(time.RFC3339), to.In(zone).Format(time.RFC3339))
			}
			f, err := a.gatherReport(cmd.Context(), from, to, zone, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			_, err = io.WriteString(a.out, renderReport(f))
			return err
		},
	}
	c.Flags().StringVar(&since, "since", "", "the window's start: a duration before --until (1h), a time (15:04) or RFC 3339; default reporter.every")
	c.Flags().StringVar(&until, "until", "", "the window's end: a time (15:04) or RFC 3339; default now")
	c.Flags().StringVar(&tz, "tz", "", "the time zone of the report's times (IANA, Europe/Berlin); default reporter.tz, else the machine's")
	return c
}

// reportTime parses a window bound in zone: RFC 3339, "15:04" (the last
// such time at or before ref) or, for --since, a duration before ref.
func reportTime(ref time.Time, s string, zone *time.Location) (time.Time, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return ref.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	t, err := time.ParseInLocation("15:04", s, zone)
	if err != nil {
		return time.Time{}, usageErr("%q is neither a time (15:04, RFC 3339) nor a duration (1h)", s)
	}
	r := ref.In(zone)
	t = time.Date(r.Year(), r.Month(), r.Day(), t.Hour(), t.Minute(), 0, 0, zone)
	if t.After(ref) {
		t = t.AddDate(0, 0, -1)
	}
	return t, nil
}

// gatherReport reads the facts of the window from to to; what GitHub does
// not answer leaves the titles and drafts out, said on warn.
func (a *app) gatherReport(ctx context.Context, from, to time.Time, zone *time.Location, warn io.Writer) (*reportFacts, error) {
	rc := a.cfg.Reporter
	f := &reportFacts{From: from, To: to, Zone: zone, Person: rc.Person}
	v, err := a.collect(false)
	if err != nil {
		return nil, err
	}
	st := v.st
	events, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == verbMerged || e.Verb == verbLaneSettled })
	if err != nil {
		return nil, err
	}
	f.Merged = windowMerges(events, st.Merges, from, to)
	repos := slices.Clone(rc.Reviews)
	for _, m := range f.Merged {
		repos = append(repos, m.Repo)
	}
	for _, r := range st.Records {
		repo, _, _ := strings.Cut(r.Issue, "#")
		repos = append(repos, repo)
	}
	f.Owners = post.Owners(repos...)
	for _, sv := range v.Sessions {
		if sv.Aside() != "" || st.Report.Running() && st.Report.Is(sv.Party()) {
			continue
		}
		// A session runs work when it holds a role, is a busy agent, or
		// serves an issue it has not recorded done and was active in the
		// window; an idle agent is finished.
		w := reportWorker{Name: sv.Name, Leases: sv.Leases}
		task, busy := strings.CutPrefix(sv.Role, "agent: ")
		role := sv.Role != "" && !strings.HasPrefix(sv.Role, "agent")
		if r := sv.Serves; r != nil && r.Ended.IsZero() && !strings.HasPrefix(strings.ToLower(r.Waits), "done") {
			w.Issue, w.Doing = r.Issue, r.Waits
		}
		switch {
		case w.Issue == "" && busy:
			w.Doing = task
		case w.Issue == "" && role:
			w.Doing = sv.Role
		case role || busy:
		case w.Issue == "" || sv.Role != "" || sv.LastActive.Before(from):
			continue
		}
		f.Workers = append(f.Workers, w)
	}
	for _, q := range guideQueue(st, v.raw, rc.Person) {
		if q.Note == nil {
			f.Asking = append(f.Asking, q.Owner)
			continue
		}
		if i := slices.IndexFunc(f.Notes, func(n noteCount) bool { return n.Owner == q.Owner }); i >= 0 {
			f.Notes[i].N++
			continue
		}
		n := noteCount{Owner: q.Owner, N: 1}
		if i := slices.IndexFunc(st.Records, func(r state.Record) bool { return r.Session.Is(q.Note.By) }); i >= 0 {
			n.Issue = st.Records[i].Issue
		}
		f.Notes = append(f.Notes, n)
	}
	f.Lanes = st.Merges
	for _, t := range st.Timers {
		if t.Fired.IsZero() && t.Due.Before(to.Add(to.Sub(from))) {
			f.Timers = append(f.Timers, t)
		}
	}
	f.Holds = a.activeHolds(st)
	if f.Machine, err = a.takeSnapshot(ctx, from, true, false); err != nil {
		return nil, err
	}
	targets := a.alertTargets(ctx)
	answers, rules := a.alertReader().Read(ctx, targets), a.alertRules()
	for i, t := range targets {
		c := alertCount{Installation: t.Name, Unreachable: answers[i].Why}
		if answers[i].OK {
			c.Active, c.Paging = rules.Count(t.Name, answers[i])
			c.Unreachable = ""
		}
		f.Alerts = append(f.Alerts, c)
	}
	var pulls []github.PR
	for _, m := range f.Merged {
		if m.N != 0 {
			pulls = append(pulls, m.PR)
		}
	}
	titles, drafts, err := github.Lookup(ctx, github.RunGH, pulls, rc.Reviews)
	if err != nil {
		_, _ = fmt.Fprintf(warn, "beekeeper report: the titles and drafts are left out, GitHub did not answer: %v\n", err)
	}
	for i := range f.Merged {
		f.Merged[i].Title = titles[f.Merged[i].PR]
	}
	f.Drafts = drafts
	return f, nil
}

// windowMerges are the merges whose merged event falls in [from, to), the
// last event of each, in merge order, with the rollout the lane's settle
// event says, or "rolling" while it settles.
func windowMerges(events []state.Event, lanes []state.Merge, from, to time.Time) []reportMerge {
	var out []reportMerge
	key := func(m reportMerge) string { return state.Merge{Repo: m.Repo, PR: m.N}.Key() }
	for _, e := range events {
		if e.Verb != verbMerged || e.At.Before(from) || !e.At.Before(to) {
			continue
		}
		head, rest, _ := strings.Cut(e.Detail, " ")
		m := reportMerge{PR: github.PR{Repo: head}}
		if repo, n, ok := strings.Cut(head, "#"); ok {
			m.Repo = repo
			m.N, _ = strconv.Atoi(n)
		} else if !strings.HasPrefix(rest, "promote") {
			continue
		}
		if r := releaseWord.FindStringSubmatch(e.Detail); r != nil {
			m.Release = strings.TrimRight(r[1], ".,")
		}
		out = slices.DeleteFunc(out, func(o reportMerge) bool { return key(o) == key(m) })
		out = append(out, m)
	}
	for i := range out {
		k := key(out[i])
		for _, e := range events {
			if _, rest, _ := strings.Cut(e.Detail, ": "); e.Verb == verbLaneSettled && !e.At.Before(from) && strings.HasPrefix(rest, k+" ") {
				switch why := strings.TrimPrefix(rest, k+" "); {
				case strings.HasPrefix(why, "rolled"):
					out[i].Rollout = "rolled"
				case strings.HasPrefix(why, "settled without a roll"):
					out[i].Rollout = "not rolled"
				}
			}
		}
		if slices.ContainsFunc(lanes, func(l state.Merge) bool { return l.Key() == k && l.Phase == state.Settling }) {
			out[i].Rollout = "rolling"
		}
	}
	return out
}

// renderReport is the report as Markdown: a bold first line with the
// window in its zone, then one bold heading per section; an empty section
// says "none".
func renderReport(f *reportFacts) string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	text := func(s, here string) string { return post.Linked(strings.Join(strings.Fields(s), " "), f.Owners, here) }
	item := func(lines []string) {
		if len(lines) == 0 {
			lines = []string{"none"}
		}
		for _, l := range lines {
			p("- %s", l)
		}
	}
	from, to := f.From.In(f.Zone), f.To.In(f.Zone)
	p("**%s–%s %s**", from.Format("15:04"), to.Format("15:04"), to.Format("MST"))

	p("\n**Merged & shipped**\n")
	if len(f.Merged) == 0 {
		item(nil)
	} else {
		p("| PR | change | release | rollout |")
		p("|---|---|---|---|")
		for _, m := range f.Merged {
			_, name, _ := strings.Cut(m.Repo, "/")
			pr, change := fmt.Sprintf("[%s#%d](https://github.com/%s/pull/%d)", name, m.N, m.Repo, m.N), conventional.ReplaceAllString(m.Title, "")
			if m.N == 0 {
				pr = fmt.Sprintf("[%s %s](https://github.com/%s/releases/tag/%s)", name, m.Release, m.Repo, m.Release)
				change = "promoted to stable"
			}
			p("| %s | %s | %s | %s |", pr, cell(text(change, m.Repo)), m.Release, m.Rollout)
		}
	}

	p("\n**Running**\n")
	var lines []string
	for _, w := range f.Workers {
		link := ""
		if w.Issue != "" {
			repo, _, _ := strings.Cut(w.Issue, "#")
			link = text(w.Issue, repo)
		}
		name := text(w.Name, "")
		if sensitive.MatchString(w.Name) {
			name = cmp.Or(link, "a session")
			link = ""
		}
		var what []string
		if link != "" {
			what = append(what, link)
		}
		if w.Doing != "" && !sensitive.MatchString(w.Doing) {
			repo, _, _ := strings.Cut(w.Issue, "#")
			what = append(what, text(w.Doing, repo))
		}
		if len(w.Leases) > 0 {
			what = append(what, "holds "+strings.Join(w.Leases, ", "))
		}
		lines = append(lines, strings.TrimSuffix(name+" — "+strings.Join(what, ", "), " — "))
	}
	item(lines)

	p("\n**Waiting on %s**\n", cmp.Or(f.Person, "the person"))
	lines = nil
	if len(f.Notes) > 0 {
		total := 0
		var owners []noteCount
		for _, n := range f.Notes {
			total += n.N
			owner := text(n.Owner, "")
			if sensitive.MatchString(n.Owner) {
				owner = "a session"
				if n.Issue != "" {
					repo, _, _ := strings.Cut(n.Issue, "#")
					owner += " on " + text(n.Issue, repo)
				}
			}
			if i := slices.IndexFunc(owners, func(o noteCount) bool { return o.Owner == owner }); i >= 0 {
				owners[i].N += n.N
				continue
			}
			owners = append(owners, noteCount{Owner: owner, N: n.N})
		}
		by := make([]string, 0, len(owners))
		for _, o := range owners {
			by = append(by, fmt.Sprintf("%d from %s", o.N, o.Owner))
		}
		lines = append(lines, fmt.Sprintf("%s: %s", plural(total, "note"), strings.Join(by, ", ")))
	}
	if len(f.Asking) > 0 {
		var names []string
		for _, s := range f.Asking {
			if sensitive.MatchString(s) {
				s = "a session"
			}
			names = append(names, text(s, ""))
		}
		lines = append(lines, fmt.Sprintf("%s waiting on an answer: %s", plural(len(f.Asking), "session"), strings.Join(names, ", ")))
	}
	if len(f.Drafts) > 0 {
		var links []string
		for _, d := range f.Drafts {
			_, name, _ := strings.Cut(d.Repo, "/")
			links = append(links, fmt.Sprintf("[%s#%d](https://github.com/%s/pull/%d)", name, d.N, d.Repo, d.N))
		}
		lines = append(lines, "drafts for review: "+strings.Join(links, ", "))
	}
	item(lines)

	p("\n**Queue**\n")
	lines = nil
	for _, m := range f.Lanes {
		k := m.Key()
		if m.PR != 0 {
			_, name, _ := strings.Cut(m.Repo, "/")
			k = fmt.Sprintf("[%s#%d](https://github.com/%s/pull/%d)", name, m.PR, m.Repo, m.PR)
		}
		lines = append(lines, fmt.Sprintf("lane %s: %s %s", m.Lane, k, m.Phase))
	}
	for _, t := range f.Timers {
		if !sensitive.MatchString(t.What) {
			lines = append(lines, fmt.Sprintf("timer %d at %s: %s", t.ID, t.Due.In(f.Zone).Format("15:04"), text(words(t.What, 100), "")))
		}
	}
	for _, h := range f.Holds {
		hold := "hold " + h.Target
		if !h.Until.IsZero() {
			hold += " until " + h.Until.In(f.Zone).Format("15:04")
		}
		if !sensitive.MatchString(h.Reason) {
			hold += ": " + text(h.Reason, "")
		}
		lines = append(lines, hold)
	}
	item(lines)

	p("\n**Machine**\n")
	s := f.Machine
	if s == nil {
		item(nil)
		return b.String()
	}
	if s.has(secMemory) {
		total, avail := s.Mem.TotalMiB, s.Mem.AvailableMiB
		p("| | used | free |")
		p("|---|---|---|")
		p("| RAM | %s %d of %d GiB | %d GiB |", bar(total-avail, total), gib(total-avail), gib(total), gib(avail))
		if s.Root.TotalMiB > 0 {
			p("| Disk | %s %d of %d GiB | %d GiB |", bar(s.Root.UsedMiB, s.Root.TotalMiB), gib(s.Root.UsedMiB), gib(s.Root.TotalMiB), gib(s.Root.FreeMiB))
		}
		p("")
	}
	lines = nil
	if s.has(secLoad) {
		l := fmt.Sprintf("load %.1f/%.1f/%.1f on %d cores", s.Load[0], s.Load[1], s.Load[2], s.Cores)
		if s.has(secPressure) {
			l += fmt.Sprintf(", CPU pressure %.1f%%, memory pressure %.1f%%", s.CPUPSI10, s.PSIFull60)
		}
		if s.LoadLimit > 0 && s.Load[0] > s.LoadLimit {
			l += fmt.Sprintf(": HIGH LOAD (over %.0f)", s.LoadLimit)
		}
		lines = append(lines, l)
	}
	if s.has(secMemory) && s.Mem.SwapTotalMiB > 0 {
		lines = append(lines, fmt.Sprintf("swap %.1f of %.1f GiB used", float64(s.Mem.SwapUsedMiB)/1024, float64(s.Mem.SwapTotalMiB)/1024))
	}
	switch names := clusterNames(s.Clusters); {
	case s.ClustersErr != "":
		lines = append(lines, "kind labs: unknown")
	case len(names) == 0:
		lines = append(lines, "kind labs: none")
	default:
		lines = append(lines, "kind labs: "+strings.Join(names, ", "))
	}
	if s.has(secOOM) {
		kills, tests := splitTestKills(s.OOM)
		l := fmt.Sprintf("%s since %s", plural(len(kills), "OOM kill"), s.OOMSince.In(f.Zone).Format("15:04"))
		if len(tests) > 0 {
			l += fmt.Sprintf(", besides %d deliberate ones of tests in their own memcap scope", len(tests))
		}
		lines = append(lines, l)
	}
	if len(f.Alerts) > 0 {
		var al []string
		for _, c := range f.Alerts {
			switch {
			case c.Unreachable != "":
				al = append(al, c.Installation+" unreachable")
			case c.Paging > 0:
				al = append(al, fmt.Sprintf("%s %d active, %d paging", c.Installation, c.Active, c.Paging))
			default:
				al = append(al, fmt.Sprintf("%s %d active", c.Installation, c.Active))
			}
		}
		lines = append(lines, "alerts: "+strings.Join(al, "; "))
	}
	if bg := s.Budget; bg != nil {
		lines = append(lines, fmt.Sprintf("GitHub budget: %d of %d left, resets %s", bg.Remaining, bg.Limit, bg.Reset.In(f.Zone).Format("15:04")))
	}
	item(lines)
	return b.String()
}

// cell is s fit for a Markdown table cell.
func cell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

// bar is a 10-character bar of used of total.
func bar(used, total int) string {
	n := 0
	if total > 0 {
		n = min(10, max(0, (used*10+total/2)/total))
	}
	return strings.Repeat("▓", n) + strings.Repeat("░", 10-n)
}

// gib is MiB in whole GiB, rounded.
func gib(mib int) int { return (mib + 512) / 1024 }

// words is s cut before the word that would pass n characters, with an
// ellipsis.
func words(s string, n int) string {
	out := ""
	for _, w := range strings.Fields(s) {
		if out != "" && len(out)+1+len(w) > n {
			return out + " …"
		}
		out = strings.TrimSpace(out + " " + w)
	}
	return out
}

// plural is n and noun, with an s unless n is 1.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
