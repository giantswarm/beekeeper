package cmd

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	guideWorkGoal  = "goal"
	guideWorkCheck = "check"
	guideStopEvent = "Stop"
)

func guideWorkText(st *state.State) string {
	var b strings.Builder
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
	var reason string
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
			st.Guide.WorkChecked = nil
			st.GuideWork = slices.Delete(st.GuideWork, i, i+1)
			return []state.Event{{At: a.now, By: me, Verb: "guide.work.done", Detail: fmt.Sprintf("%d: %s", id, reason)}}, nil
		})
	}}
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
				if work.Kind != guideWorkGoal && !work.Due.After(a.now) {
					return nil, refused("guide work %d is due: finish, file or schedule it before checking the turn", work.ID)
				}
			}
			st.Guide.WorkChecked = &me
			return []state.Event{{At: a.now, By: me, Verb: "guide.work.checked"}}, nil
		})
	}}
	c.AddCommand(add, done, deferWork, check)
	return c
}

func overrideRestoration(o state.Override) string {
	if o.Ref != "" {
		return "when " + issueURL(o.Ref) + " closes"
	}
	return o.Until.Format("2006-01-02 15:04:05 MST")
}
