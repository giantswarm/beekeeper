package cmd

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	guideWorkGoal      = "goal"
	guideWorkBacklog   = "backlog"
	guideWorkCheck     = "check"
	guideStopEvent     = "Stop"
	guideBacklogPrefix = "Historical worker problem backlog (close together with guide work done):\n"
)

func guideWorkText(st *state.State) string {
	var b strings.Builder
	var fingerprints []string
	for fingerprint := range st.GuideFindings {
		fingerprints = append(fingerprints, fingerprint)
	}
	slices.Sort(fingerprints)
	for _, fingerprint := range fingerprints {
		finding := st.GuideFindings[fingerprint]
		if finding.Issue != "" {
			_, _ = fmt.Fprintf(&b, "- Filed finding: %s; observations: %d; fingerprint: %s\n", finding.Issue, finding.Count, fingerprint)
		}
	}
	for _, work := range st.GuideWork {
		_, _ = fmt.Fprintf(&b, "- %d [%s] %s; owner: guide (recorded by %s); due: %s\n", work.ID, work.Kind, work.Text, work.By.Name, work.Due.Format("2006-01-02 15:04:05 MST"))
	}
	for _, o := range st.Overrides {
		_, _ = fmt.Fprintf(&b, "- Temporary configuration: %s; restore: %s\n", o.Reason, overrideRestoration(o))
	}
	if b.Len() == 0 {
		return "None open."
	}
	return strings.TrimSpace(b.String())
}

func (a *app) guideWorkCmd() *cobra.Command {
	c := &cobra.Command{Use: "work", Short: "The guide's owned work, goals and restoration deadlines", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		st, err := a.store.Read()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(a.out, guideWorkText(st))
		return err
	}}
	var kind, due string
	add := &cobra.Command{Use: "add <text> --due <time> [--kind task|goal|restore]", Short: "Retain work across guide relays with an explicit next action time", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		if !slices.Contains([]string{paramTask, guideWorkGoal, "restore"}, kind) {
			return fmt.Errorf("kind must be task, goal or restore")
		}
		if strings.TrimSpace(args[0]) == "" || due == "" {
			return fmt.Errorf("text and --due are required")
		}
		at, err := timerTime(a.now, due)
		if err != nil {
			return err
		}
		me, err := a.caller()
		if err != nil {
			return err
		}
		return a.store.Update(func(st *state.State) ([]state.Event, error) {
			if st.Guide == nil || st.Guide.Holder == nil || !st.Guide.Holder.Is(me) {
				return nil, refused("only the current guide records its work")
			}
			st.Guide.WorkChecked = nil
			st.NextGuideWork++
			st.GuideWork = append(st.GuideWork, state.GuideWork{ID: st.NextGuideWork, Kind: kind, Text: args[0], Due: at.UTC(), By: me})
			return []state.Event{{At: a.now, By: me, Verb: "guide.work.added", Detail: args[0]}}, nil
		})
	}}
	add.Flags().StringVar(&kind, "kind", paramTask, "task, goal or restore")
	add.Flags().StringVar(&due, "due", "", "next action or restoration time (now, 30m or a dated time)")
	var reason, issue string
	done := &cobra.Command{Use: "done <id> --reason <result>", Short: "Close owned work with its result", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return err
		}
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("--reason is required")
		}
		me, err := a.caller()
		if err != nil {
			return err
		}
		return a.store.Update(func(st *state.State) ([]state.Event, error) {
			if st.Guide == nil || st.Guide.Holder == nil || !st.Guide.Holder.Is(me) {
				return nil, refused("only the current guide closes its work")
			}
			i := slices.IndexFunc(st.GuideWork, func(w state.GuideWork) bool { return w.ID == id })
			if i < 0 {
				return nil, fmt.Errorf("guide work %d not found", id)
			}
			if issue != "" {
				if _, ok := parseRef(issue); !ok || !strings.HasPrefix(issue, "https://github.com/") || !strings.Contains(issue, "/issues/") {
					return nil, fmt.Errorf("--issue requires a full GitHub issue URL")
				}
				if st.GuideFindings == nil {
					st.GuideFindings = map[string]state.GuideFinding{}
				}
				fingerprint := guideFindingFingerprint(st.GuideWork[i].Text)
				finding := st.GuideFindings[fingerprint]
				finding.Issue = issue
				st.GuideFindings[fingerprint] = finding
			}
			st.Guide.WorkChecked = nil
			st.GuideWork = slices.Delete(st.GuideWork, i, i+1)
			return []state.Event{{At: a.now, By: me, Verb: "guide.work.done", Detail: fmt.Sprintf("%d: %s", id, reason)}}, nil
		})
	}}
	done.Flags().StringVar(&issue, "issue", "", "existing issue owning this report; matching reports increment its tally")
	done.Flags().StringVar(&reason, "reason", "", "what was done, filed or restored")
	var next, why string
	deferWork := &cobra.Command{Use: "defer <id> --due <time> --reason <why>", Short: "Schedule owned work's next action with its reason", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return err
		}
		if next == "" || strings.TrimSpace(why) == "" {
			return fmt.Errorf("--due and --reason are required")
		}
		at, err := timerTime(a.now, next)
		if err != nil {
			return err
		}
		if !at.After(a.now) {
			return fmt.Errorf("deferred work needs a future time")
		}
		me, err := a.caller()
		if err != nil {
			return err
		}
		return a.store.Update(func(st *state.State) ([]state.Event, error) {
			if st.Guide == nil || st.Guide.Holder == nil || !st.Guide.Holder.Is(me) {
				return nil, refused("only the current guide schedules its work")
			}
			i := slices.IndexFunc(st.GuideWork, func(w state.GuideWork) bool { return w.ID == id })
			if i < 0 {
				return nil, fmt.Errorf("guide work %d not found", id)
			}
			st.Guide.WorkChecked = nil
			st.GuideWork[i].Due = at.UTC()
			return []state.Event{{At: a.now, By: me, Verb: "guide.work.deferred", Detail: fmt.Sprintf("%d until %s: %s", id, at.Format(time.RFC3339), why)}}, nil
		})
	}}
	deferWork.Flags().StringVar(&next, "due", "", "next action time")
	deferWork.Flags().StringVar(&why, "reason", "", "why this action waits")
	check := &cobra.Command{Use: guideWorkCheck, Short: "Review this turn's work before ending it", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		me, err := a.caller()
		if err != nil {
			return err
		}
		return a.store.Update(func(st *state.State) ([]state.Event, error) {
			if st.Guide == nil || st.Guide.Holder == nil || !st.Guide.Holder.Is(me) {
				return nil, refused("only the current guide checks its work")
			}
			for _, work := range st.GuideWork {
				if guideWorkBlocks(work, a.now) {
					return nil, refused("guide work %d is due: finish, file or schedule it before checking the turn", work.ID)
				}
			}
			st.Guide.WorkChecked = &me
			return []state.Event{{At: a.now, By: me, Verb: "guide.work.checked"}}, nil
		})
	}}
	unmap := &cobra.Command{Use: "unmap <issue-url>", Short: "Allow new reports after a filed issue is resolved", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		me, err := a.caller()
		if err != nil {
			return err
		}
		return a.store.Update(func(st *state.State) ([]state.Event, error) {
			if st.Guide == nil || st.Guide.Holder == nil || !st.Guide.Holder.Is(me) {
				return nil, refused("only the current guide changes report mappings")
			}
			for fingerprint, finding := range st.GuideFindings {
				if finding.Issue == args[0] {
					finding.Issue = ""
					st.GuideFindings[fingerprint] = finding
				}
			}
			return []state.Event{{At: a.now, By: me, Verb: "guide.finding.unmapped", Detail: args[0]}}, nil
		})
	}}
	c.AddCommand(add, done, deferWork, check, unmap)
	return c
}

func overrideRestoration(o state.Override) string {
	if o.Ref != "" {
		return "when " + issueURL(o.Ref) + " closes"
	}
	return o.Until.Format("2006-01-02 15:04:05 MST")
}

// guideReportEvents shares a durable delivery boundary between both guide feeds.
// On upgrade, start at the first observation instead of replaying the event log.
func guideReportEvents(st *state.State, events []state.Event, now time.Time) []state.Event {
	if st.GuideReportsSince.IsZero() {
		st.GuideReportsSince = now.UTC()
	}
	since := st.GuideReportsSince
	var fresh []state.Event
	oldSources := map[string]bool{}
	for _, e := range events {
		key, text, ok := keyed(e.Detail)
		if !ok || !strings.Contains(text, fileReportSeparator) {
			continue
		}
		if e.At.Before(now.Add(-24 * time.Hour)) {
			oldSources[key] = true
		}
		if e.At.Before(since) || oldSources[key] {
			continue
		}
		fresh = append(fresh, e)
	}
	// Repair reports already imported by older versions. Preserve their text
	// in one explicitly marked backlog, closable with the ordinary done command.
	var kept []state.GuideWork
	var backlog *state.GuideWork
	migrated := false
	for _, work := range st.GuideWork {
		if work.Kind == guideWorkBacklog || work.Source != "" && (oldSources[work.Source] || work.Due.Before(now.Add(-24*time.Hour))) {
			if work.Kind != guideWorkBacklog {
				migrated = true
			}
			body := strings.TrimPrefix(work.Text, guideBacklogPrefix)
			if backlog == nil {
				copy := work
				copy.Kind = guideWorkBacklog
				copy.Source = ""
				copy.Text = guideBacklogPrefix + body
				backlog = &copy
			} else {
				migrated = true
				backlog.Text += "\n" + body
			}
			continue
		}
		kept = append(kept, work)
	}
	if migrated && backlog != nil {
		st.GuideWork = append(kept, *backlog)
	}
	return settleGuideRecoveries(st, fresh)
}

func guideWorkBlocks(work state.GuideWork, now time.Time) bool {
	return work.Kind != guideWorkGoal && work.Kind != guideWorkBacklog && !work.Due.After(now)
}

// Fingerprint only the finding, not the reporter or observation time.
func guideFindingFingerprint(text string) string {
	_, body, ok := strings.Cut(text, fileReportSeparator)
	if !ok {
		body = text
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(body))))
}

// retainGuideReport is shared by both feeds under the state lock. Recovery is
// bookkeeping, never new work; a known issue owns subsequent matching reports.
func retainGuideReport(st *state.State, key, text string, at time.Time, by state.Party) bool {
	_, body, _ := strings.Cut(text, fileReportSeparator)
	if strings.HasPrefix(body, "RECOVERED:") {
		source := strings.Replace(key, "delivery recovered ", "delivery ", 1)
		source = strings.Replace(source, "delivery full recovered ", "delivery full ", 1)
		if !slices.Contains(st.GuideReports, source) {
			st.GuideReports = append(st.GuideReports, source)
		}
		st.GuideWork = slices.DeleteFunc(st.GuideWork, func(w state.GuideWork) bool {
			_, previous, _ := strings.Cut(w.Text, fileReportSeparator)
			return w.Source != "" && (strings.HasPrefix(key, "throughput ") && strings.HasPrefix(w.Source, "throughput ") || w.Source == source || strings.TrimSpace(strings.TrimPrefix(body, "RECOVERED:")) == strings.TrimSpace(previous))
		})
		return false
	}
	if slices.Contains(st.GuideReports, key) {
		return false
	}
	st.GuideReports = append(st.GuideReports, key)
	if st.GuideFindings == nil {
		st.GuideFindings = map[string]state.GuideFinding{}
	}
	fingerprint := guideFindingFingerprint(text)
	finding := st.GuideFindings[fingerprint]
	finding.Count++
	st.GuideFindings[fingerprint] = finding
	if finding.Issue != "" {
		return false
	}
	// Coalesce a repeated report while its original work is still open.
	if slices.ContainsFunc(st.GuideWork, func(w state.GuideWork) bool { return w.Source != "" && guideFindingFingerprint(w.Text) == fingerprint }) {
		return false
	}
	st.Guide.WorkChecked = nil
	st.NextGuideWork++
	st.GuideWork = append(st.GuideWork, state.GuideWork{ID: st.NextGuideWork, Kind: paramTask, Text: text, Due: at, By: by, Source: key})
	return true
}

// Settle a whole batch before announcing anything: a raised and recovered
// episode between polls must not spend a guide turn.
func settleGuideRecoveries(st *state.State, events []state.Event) []state.Event {
	for _, e := range events {
		key, text, ok := keyed(e.Detail)
		_, body, _ := strings.Cut(text, fileReportSeparator)
		if ok && strings.HasPrefix(body, "RECOVERED:") {
			if strings.HasPrefix(key, "throughput ") {
				for _, prior := range events {
					previous, _, _ := keyed(prior.Detail)
					if !prior.At.After(e.At) && strings.HasPrefix(previous, "throughput ") && !slices.Contains(st.GuideReports, previous) {
						st.GuideReports = append(st.GuideReports, previous)
					}
				}
			}
			retainGuideReport(st, key, text, e.At, state.Party{})
		}
	}
	return slices.DeleteFunc(slices.Clone(events), func(e state.Event) bool {
		_, text, _ := keyed(e.Detail)
		_, body, _ := strings.Cut(text, fileReportSeparator)
		return strings.HasPrefix(body, "RECOVERED:")
	})
}
