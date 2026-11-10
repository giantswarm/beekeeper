package cmd

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	emergencyOpenState    = "OPEN"
	emergencySetCommand   = "set"
	emergencyPauseCommand = "pause"
	emergencyViewCommand  = "view"

	emergencyApproved       = "APPROVED"
	emergencyReviewRequired = "REVIEW_REQUIRED"
	emergencyMergeable      = "MERGEABLE"
	emergencyBlocked        = "BLOCKED"
	emergencyClean          = "CLEAN"
)

// emergencyNow is read at each authorization boundary, including after slow reads.
var emergencyNow = time.Now

// emergencySameSession retains Party.Is's person and host checks while requiring
// an actual session identifier; a shared display name cannot authorize controls.
func emergencySameSession(p, o state.Party) bool {
	return p.Is(o) && ((p.Session != "" && p.Session == o.Session) ||
		(p.HostSession != "" && p.HostSession == o.HostSession))
}

// emergencyGuide deliberately disallows --as: only the live holder of the
// guide role can exercise its grant, not a caller who knows its display name.
func (a *app) emergencyGuide(st *state.State) (state.Party, error) {
	me, err := a.caller()
	if err != nil {
		return me, err
	}
	h := st.GuideRole().Holder
	if a.as != "" || (me.Session == "" && me.HostSession == "") || h == nil || !emergencySameSession(h.Party, me) {
		return me, refused("only the current guide session can use emergency controls")
	}
	return me, nil
}

func (a *app) emergencyAction(action string) (*state.Emergency, error) {
	var grant *state.Emergency
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		me, err := a.emergencyGuide(st)
		if err != nil {
			return nil, err
		}
		e := st.Emergency
		if e == nil || !emergencyNow().Before(e.Until) {
			return nil, refused("emergency mode is off or expired; enable it with the person's quoted words and --until")
		}
		copy := *e
		grant = &copy
		return []state.Event{event(me, "emergency.action", "%s; person's words: %q; expires %s", action, e.Reason, e.Until.Format(time.RFC3339))}, nil
	})
	return grant, err
}

func (a *app) emergencyCmd() *cobra.Command {
	c := &cobra.Command{Use: "emergency", Short: "Bounded, audited emergency controls for the current guide", Long: `The current guide enables emergency mode on the person's explicit word, quoted
verbatim with --reason, and a required --until time or duration. The grant
belongs to the guide role and transfers to its authenticated successor without
extending its expiry. The original words and issuer remain recorded.

Use emergency merge <owner/repo> <number> for an approved open pull request:
it bypasses desk review and the merge lane, using the repository's configured merge method with
an exact head match. GitHub branch protection still applies. Every attempted
action and its result is logged with the person's words.

With --admin --head <sha> the current guide can act on the person's general
merge order without asking them to name each pull request. The guide must
quote the authorization verbatim when enabling the bounded grant. A pull
request with REVIEW_REQUIRED or an unset review decision must still be open,
green, mergeable and up to date with its base at exactly that head, with an approved desk
review or the loop's go for it. Only then does the merge send --admin,
which waives nothing but the review requirement.

Other controls are
emergency hold set/lift, emergency start (capacity --force), and emergency
capacity <capacity.key> <value> (validated configuration). Standing stop and
starts holds still apply; lift a starts hold explicitly when authorized. Emergency
start can override exhausted main-outage repair barriers under the active grant;
other fatal outages remain barriers. Each override is audited.

Emergency auto-install pause/resume is independent of the grant: it only
pauses the supervisor's release installation, leaving its other work running.
The pause persists until the guide resumes it.`, Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		st, err := a.store.Read()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(a.out, emergencyText(st, emergencyNow()))
		if st.AutoInstallPaused {
			_, err = fmt.Fprintln(a.out, "release auto-install paused")
		}
		return err
	}}
	var reason, until string
	on := &cobra.Command{Use: "on", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		if strings.TrimSpace(reason) == "" || until == "" {
			return usageErr("--reason (the person's verbatim words) and --until are required")
		}
		end, err := untilTime(emergencyNow(), until)
		if err != nil {
			return err
		}
		if !end.After(emergencyNow()) {
			return usageErr("--until must be in the future")
		}
		return a.store.Update(func(st *state.State) ([]state.Event, error) {
			me, err := a.emergencyGuide(st)
			if err != nil {
				return nil, err
			}
			st.Emergency = &state.Emergency{By: me, Reason: reason, Until: end}
			return []state.Event{event(me, "emergency.on", "until %s; person's words: %q", end.Format(time.RFC3339), reason)}, nil
		})
	}}
	on.Flags().StringVar(&reason, "reason", "", "the person's explicit authorization, quoted verbatim (required)")
	on.Flags().StringVar(&until, "until", "", "expiry time or duration (required)")
	off := &cobra.Command{Use: "off", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		return a.store.Update(func(st *state.State) ([]state.Event, error) {
			me, err := a.emergencyGuide(st)
			if err != nil {
				return nil, err
			}
			old := st.Emergency
			st.Emergency = nil
			quote := ""
			if old != nil {
				quote = old.Reason
			}
			return []state.Event{event(me, "emergency.off", "person's words: %q", quote)}, nil
		})
	}}
	var admin bool
	var adminHead string
	merge := &cobra.Command{Use: "merge <owner/repo> <number>", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		n, err := strconv.Atoi(args[1])
		if err != nil || n <= 0 || len(strings.Split(args[0], "/")) != 2 || strings.HasPrefix(args[0], "-") {
			return usageErr("want owner/repo and a positive pull request number")
		}
		if admin != (adminHead != "") {
			return usageErr("--admin and --head go together: the exception holds for one exact head")
		}
		action := fmt.Sprintf("merge https://github.com/%s/pull/%d", args[0], n)
		if admin {
			action = fmt.Sprintf("admin %s at %s", action, adminHead)
		}
		return a.runEmergency(action, func() error {
			if err := a.emergencyMergePolicy(args[0], n); err != nil {
				return err
			}
			if admin {
				return a.emergencyAdminMerge(cmd.Context(), args[0], n, adminHead)
			}
			raw, err := github.RunGH(cmd.Context(), "pr", emergencyViewCommand, strconv.Itoa(n), "--repo", args[0], "--json", "state,isDraft,reviewDecision,headRefOid")
			if err != nil {
				return err
			}
			var pr struct {
				State, ReviewDecision, HeadRefOid string
				IsDraft                           bool
			}
			if err = json.Unmarshal(raw, &pr); err != nil {
				return err
			}
			if pr.State != emergencyOpenState || pr.IsDraft || pr.HeadRefOid == "" {
				return refused("emergency merge requires an approved, open, non-draft pull request")
			}
			if pr.ReviewDecision != emergencyApproved {
				// A same-account desk review is COMMENTED on GitHub. Read
				// its trusted exact-head verdict instead of requiring a native
				// approval. Branch protection still applies without --admin.
				p, err := readLoopPull(cmd.Context(), fmt.Sprintf("%s#%d", args[0], n))
				if err != nil {
					return err
				}
				if p.State != superloop.Open || p.Draft || !p.Green || !sameCommit(p.Head, pr.HeadRefOid) {
					return refused("emergency merge requires a green, open, non-draft pull request at the reviewed head")
				}
				reviews, err := a.trustedReviews(cmd.Context(), p)
				if err != nil {
					return err
				}
				var latest *superloop.Review
				for i := range reviews {
					if latest == nil || !reviews[i].At.Before(latest.At) {
						latest = &reviews[i]
					}
				}
				if latest == nil || latest.Verdict != superloop.Approved || !sameCommit(cmp.Or(latest.Head, latest.Commit), pr.HeadRefOid) {
					return refused("emergency merge requires an approved desk review of the exact head")
				}
			}
			if err := a.emergencyMergePolicy(args[0], n); err != nil {
				return err
			}
			if _, err := a.emergencyAction(fmt.Sprintf("merge https://github.com/%s/pull/%d at %s", args[0], n, pr.HeadRefOid)); err != nil {
				return err
			}
			_, err = github.RunGH(cmd.Context(), "pr", gitMerge, strconv.Itoa(n), "--repo", args[0], "--"+cmp.Or(a.cfg.Merge.Methods[args[0]], mergeMethodSquash), "--match-head-commit", pr.HeadRefOid)
			return err
		})
	}}
	merge.Flags().BoolVar(&admin, "admin", false, "merge with REVIEW_REQUIRED or an unset review decision under the current guide's bounded emergency grant")
	merge.Flags().StringVar(&adminHead, "head", "", "with --admin: the exact head the decision is for")
	hold := a.holdCmd()
	for _, sub := range hold.Commands() {
		if sub.Name() == emergencySetCommand || sub.Name() == "lift" {
			a.wrapEmergency(sub)
		}
	}
	start := a.agentStartCmd()
	start.Use = "start [flags] [<name>] <brief file>"
	runStart := start.RunE
	start.RunE = func(cmd *cobra.Command, args []string) error {
		return a.runEmergency(emergencyCommandText(cmd, args), func() error {
			if err := cmd.Flags().Set("force", "true"); err != nil {
				return err
			}
			a.emergencyStart = true
			defer func() { a.emergencyStart = false }()
			return runStart(cmd, args)
		})
	}
	capacity := &cobra.Command{Use: "capacity <capacity.key> <value> [<capacity.key> <value>]…", Args: func(_ *cobra.Command, args []string) error {
		if len(args) == 0 || len(args)%2 != 0 {
			return usageErr("want capacity.key/value pairs")
		}
		for i := 0; i < len(args); i += 2 {
			if !strings.HasPrefix(args[i], "capacity.") {
				return usageErr("only capacity keys are allowed")
			}
		}
		return nil
	}, RunE: func(cmd *cobra.Command, args []string) error {
		set := a.configCmd()
		for _, sub := range set.Commands() {
			if sub.Name() == emergencySetCommand {
				return a.runEmergency("capacity "+strings.Join(args, " "), func() error { return sub.RunE(cmd, args) })
			}
		}
		return fmt.Errorf("config set unavailable")
	}}
	auto := &cobra.Command{Use: "auto-install", Short: "Pause or resume release auto-install without stopping the loop"}
	var pauseReason string
	for _, verb := range []string{emergencyPauseCommand, agentResumeName} {
		sub := &cobra.Command{Use: verb, Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
			if strings.TrimSpace(pauseReason) == "" {
				return usageErr("--reason (the person's words) is required")
			}
			return a.store.Update(func(st *state.State) ([]state.Event, error) {
				me, err := a.emergencyGuide(st)
				if err != nil {
					return nil, err
				}
				st.AutoInstallPaused = verb == emergencyPauseCommand
				return []state.Event{event(me, "auto-install."+verb, "person's words: %q", pauseReason)}, nil
			})
		}}
		sub.Flags().StringVar(&pauseReason, "reason", "", "the person's words (required)")
		auto.AddCommand(sub)
	}
	c.AddCommand(on, off, merge, hold, start, capacity, auto)
	return c
}

func (a *app) wrapEmergency(c *cobra.Command) {
	run := c.RunE
	c.RunE = func(cmd *cobra.Command, args []string) error {
		return a.runEmergency(emergencyCommandText(cmd, args), func() error { return run(cmd, args) })
	}
}

func (a *app) runEmergency(action string, run func() error) error {
	grant, err := a.emergencyAction(action)
	if err != nil {
		return err
	}
	err = run()
	result := "succeeded"
	if err != nil {
		result = "failed: " + err.Error()
	}
	// Record the outcome against the same authorization even if it expired
	// during a slow command. The pre-action record is durable before mutation.
	reason := grant.Reason
	me, callerErr := a.caller()
	if callerErr != nil {
		return callerErr
	}
	if logErr := a.store.Record(event(me, "emergency.result", "%s: %s; person's words: %q", action, result, reason)); logErr != nil {
		return fmt.Errorf("action result %s; logging result: %w", result, logErr)
	}
	return err
}

func emergencyCommandText(cmd *cobra.Command, args []string) string {
	text := cmd.CommandPath() + " " + strings.Join(args, " ")
	cmd.Flags().Visit(func(f *pflag.Flag) { text += fmt.Sprintf(" --%s=%q", f.Name, f.Value.String()) })
	return text
}

// emergencyAdminMerge applies the current guide's bounded emergency grant
// to one exact head. The guide interprets the person's verbatim authorization;
// no per-pull naming round is required. Everything but GitHub's review
// requirement must already say go. The attempt is logged before mutation.
func (a *app) emergencyAdminMerge(ctx context.Context, repo string, number int, head string) error {
	ref := fmt.Sprintf("%s#%d", repo, number)
	url := fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	p, err := readLoopPull(ctx, ref)
	if err != nil {
		return err
	}
	if p.State != superloop.Open || p.Draft || !strings.EqualFold(p.Head, head) || !p.Green {
		return refused("admin merge requires %s open, non-draft and green at %s; it is %s at %s, green %v", ref, shortSHA(head), p.State, shortSHA(p.Head), p.Green)
	}
	if why := adminMergeBlock(p); why != "" {
		return refused("admin merge of %s: %s", ref, why)
	}
	why, _, err := a.checkMergeHeadAuthority(ctx, st, p, p.Head)
	if err != nil {
		return err
	}
	if why != "" {
		return refused("admin merge of %s: %s", ref, why)
	}
	if err := github.ReviewOnlyBypass(ctx, github.RunGH, repo, p.Base, p.Head); err != nil {
		return refused("admin merge of %s: %v", ref, err)
	}
	if err := a.emergencyMergePolicy(repo, number); err != nil {
		return err
	}
	if _, err := a.emergencyAction(fmt.Sprintf("admin merge %s at %s", url, p.Head)); err != nil {
		return err
	}
	_, err = github.RunGH(ctx, "pr", gitMerge, strconv.Itoa(number), "--repo", repo, "--"+cmp.Or(a.cfg.Merge.Methods[repo], mergeMethodSquash), "--admin", "--match-head-commit", p.Head)
	return err
}

// adminMergeBlock is what, beside a missing review, keeps p from its merge:
// the admin exception waives the review requirement and nothing else.
func adminMergeBlock(p superloop.Pull) string {
	switch {
	case p.Base == "":
		return "its base branch is unknown"
	case p.ReviewDecision != "" && p.ReviewDecision != emergencyReviewRequired && p.ReviewDecision != emergencyApproved:
		return "its review decision is " + cmp.Or(p.ReviewDecision, "unset")
	case p.Mergeable != emergencyMergeable:
		return "it is not mergeable: " + cmp.Or(p.Mergeable, "unknown")
	case p.MergeState != emergencyBlocked && p.MergeState != emergencyClean:
		return "its merge state is " + cmp.Or(p.MergeState, "unknown")
	}
	return ""
}

// emergencyMergePolicy preserves standing stops and holds; emergency control
// lets the guide explicitly lift a hold, never silently ignore it.
func (a *app) emergencyMergePolicy(repo string, number int) error {
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	me, err := a.emergencyGuide(st)
	if err != nil {
		return err
	}
	if st.Stop != nil {
		return refused("%s", reasonDeskStopped)
	}
	lane := a.cfg.LaneOf(repo)
	if a.cfg.CentralLane(lane) {
		holds, err := a.centralHolds(me)
		if err != nil {
			return err
		}
		st.Holds = append(st.Holds, holds...)
	}
	if h, held := merge.BlockingFor(st, emergencyNow(), me.Name, repo, number, lane); held {
		return refused("emergency merge is held on %s: %s; lift the hold explicitly", h.Target, h.Reason)
	}
	return nil
}

// The role holder is the grant holder; By remains the issuing guide for audit.
// Reading it from the role also covers relays before the successor's first action.
func emergencyText(st *state.State, now time.Time) string {
	e := st.Emergency
	if e == nil || !now.Before(e.Until) {
		return "emergency off"
	}
	holder := "no current guide"
	if h := st.GuideRole().Holder; h != nil {
		holder = fmt.Sprintf("%q (session %q, host session %q)", h.Name, h.Session, h.HostSession)
	}
	return fmt.Sprintf("emergency until %s; issued by %q (session %q, host session %q); holder: %s; person's words: %q", e.Until.Format(time.RFC3339), e.By.Name, e.By.Session, e.By.HostSession, holder, e.Reason)
}
