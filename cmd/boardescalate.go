package cmd

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// blockedStatus is the board's Status of a blocked item, an unambiguous
// part of its name ("Blocked / Waiting ⛔️").
const blockedStatus = "blocked"

// escalateWhat opens the text of an escalation's re-check timer; the rest
// is the item's ref. An open timer with it means the item is escalated.
const escalateWhat = "re-check blocked "

// escalation is one blocked item to escalate to the person.
type escalation struct {
	Ref   string `json:"ref"`
	URL   string `json:"url"`
	Title string `json:"title"`
	// Why is what holds it: its open recorded blockers, or its Status.
	Why string `json:"why"`
}

// escalations are the open board items in the blocked Status, refs the
// ones named (all when empty), that have no open re-check timer: each is
// escalated once, and again when its timer fires while it stays blocked.
// An item whose recorded blockers all closed is left out when the order's
// blocker-cleared step offers it as work: its Status was set before the
// last of them closed. One whose Status was set after is held by it
// (HeldByStatus), offered by no step, and escalated.
func escalations(items []board.Item, blocked string, timers []state.Timer, refs []string) []escalation {
	var out []escalation
	for _, it := range items {
		if !strings.EqualFold(it.Status, blocked) || (it.Blockers > 0 && it.OpenBlockers == 0 && !it.HeldByStatus()) {
			continue
		}
		if len(refs) > 0 && !slices.ContainsFunc(refs, func(r string) bool { return strings.EqualFold(r, it.Ref) }) {
			continue
		}
		if slices.ContainsFunc(timers, func(t state.Timer) bool { return strings.EqualFold(t.What, escalateWhat+it.Ref) }) {
			continue
		}
		why := "Status " + it.Status
		if it.OpenBlockers > 0 {
			why = it.HeldText()
		}
		out = append(out, escalation{Ref: it.Ref, URL: it.URL, Title: it.Title, Why: why})
	}
	return out
}

// shellQuote quotes s as one word of a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// escalateRun is the re-check timer's command: this binary, its
// configuration and the caller, running board escalate on ref.
func (a *app) escalateRun(me state.Party, ref string) (string, error) {
	bin, err := os.Executable()
	if err != nil {
		return "", err
	}
	w := []string{shellQuote(bin)}
	if a.cfgPath != "" {
		w = append(w, "--"+configFlag, shellQuote(a.cfgPath))
	}
	if me.Name != "" {
		w = append(w, "--"+asFlag, shellQuote(me.Name))
	}
	return strings.Join(append(w, loopBoard, boardEscalateName, shellQuote(ref)), " "), nil
}

// escalate files one note for the person on e and a timer that runs board
// escalate on it again after every: while it stays blocked, that run
// escalates it once more.
func (a *app) escalate(ctx context.Context, me state.Party, e escalation, every time.Duration) error {
	person := a.cfg.Guide.Person
	if person == "" {
		return fmt.Errorf("guide.person is unset: no person to escalate %s to", e.Ref)
	}
	run, err := a.escalateRun(me, e.Ref)
	if err != nil {
		return err
	}
	as := func(args ...string) []string {
		out := []string{}
		if a.cfgPath != "" {
			out = append(out, "--"+configFlag, a.cfgPath)
		}
		if me.Name != "" {
			out = append(out, "--"+asFlag, me.Name)
		}
		return append(out, args...)
	}
	question := truncate(fmt.Sprintf("%s is blocked (%s): %s. What unblocks it?", e.URL, e.Why, e.Title), 150)
	note := as(loopNote, loopAdd, "--for", person, "--ref", e.Ref, "--due", a.cfg.Supervisor.NoteDue.String(),
		"--status-quo", fmt.Sprintf("%s sits in Blocked on the board: %s.", e.URL, e.Why),
		"--why", "a blocked item gets no worker until what holds it is settled",
		"--default", fmt.Sprintf("it stays blocked; beekeeper checks it again in %s and asks again while it is", every),
		"--checked", "beekeeper board escalate: the board's Status",
		question)
	timer := as(loopTimer, loopAdd, every.String(), escalateWhat+e.Ref, "--run", run)
	// The timer first: a note that fails after it is retried once its
	// timer fires, never on every tidy.
	for _, step := range []struct {
		name string
		args []string
	}{{"timer add", timer}, {"note add", note}} {
		out, code, err := beekeeperRun(ctx, step.args...)
		if err != nil {
			return err
		}
		if code != 0 {
			return &exitError{code: code, msg: fmt.Sprintf("beekeeper %s for %s: exit %d: %s", step.name, e.Ref, code, oneLine(out))}
		}
	}
	return nil
}

// escalateBoard is b read for board escalate: its order with a step of the
// blocked Status, so the read takes the blocked items whatever board.order
// names. The step only widens the read; escalate picks nothing by it.
func escalateBoard(b config.Board) config.Board {
	b.Order = append(slices.Clone(b.Order), config.BoardStep{Name: boardEscalateName, Status: []string{blockedStatus}})
	return b
}

// boardEscalateName is the board escalate subcommand.
const boardEscalateName = "escalate"

func (a *app) boardEscalateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "escalate [<owner/repo#n>...]",
		Short: "Escalate the board's blocked items to the person, once, and again on a timer while they stay blocked",
		Long: `Reads the board and escalates each open item in Blocked (Blocked / Waiting)
to the person: one note for guide.person naming the item and what holds it
(its open recorded blockers, or its Status), and a timer that runs board
escalate on it again after board.escalateEvery (default 24h). While an
item's timer is open it is not escalated again; when the timer fires and the
item is still blocked, it gets a new note and a new timer, and once it left
Blocked nothing happens. An item whose recorded blockers all closed before
its Status was set is left out: the order's blocker-cleared step offers it
as work; one whose Status was set after is held by it and escalated. The
read takes the Blocked items whatever board.order names. With refs, only
those items are looked at.

The supervisor loop runs it every board.tidyEvery after board tidy; a
negative board.escalateEvery escalates nothing. A worker that finds its item
blocked moves it there (board move <item> blocked) and reports what holds it.
Prints one line per escalation.`,
		RunE: func(cmd *cobra.Command, refs []string) error {
			every := a.cfg.Board.EscalateEvery.Duration
			if every < 0 {
				return nil
			}
			me, _ := a.caller()
			cl := a.boardClient()
			cl.Board = escalateBoard(cl.Board)
			snap, err := cl.Read(cmd.Context(), a.now)
			if err != nil {
				return err
			}
			blocked, err := board.Resolve(board.StatusField, snap.Statuses, blockedStatus)
			if err != nil {
				return err
			}
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			es := escalations(snap.Items, blocked, st.Timers, refs)
			for _, e := range es {
				if err := a.escalate(cmd.Context(), me, e, every); err != nil {
					return err
				}
			}
			if a.json {
				return a.printJSON(es)
			}
			for _, e := range es {
				if _, err := fmt.Fprintf(a.out, "escalated %s (%s): re-checked in %s\n", e.URL, e.Why, every); err != nil {
					return err
				}
			}
			return nil
		},
	}
}
