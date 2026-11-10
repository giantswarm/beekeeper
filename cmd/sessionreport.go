package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

const reportReferencePrefix = "report:"

// sessionReport contains command inputs, never a patch to a stale snapshot.
// Assignment identity keeps a delayed completion from ending a later task.
type sessionReport struct {
	Kind         string
	By           state.Party
	At           time.Time
	AssignedAt   time.Time
	Task         string
	Finding      string
	Text         string
	Problems     []string
	Finished     bool
	On           string
	Waits        string
	External     bool
	PostMerge    []string
	Worktree     string
	RefKind      string
	ReviewHead   string
	ReviewReason string
	Session      state.Party
	Record       state.Record
	Note         state.Note
	FoldInto     int
	Checked      bool
	Stage        planStage
	Draft        noteDraft
	Holds        bool
	As           bool
	Assignee     state.Party
	Registered   bool
	Clear        bool
	NoteID       int
}

func (a *app) submitReport(p sessionReport) error {
	st, err := a.store.Peek()
	if err != nil {
		return err
	}
	p.At = a.now.UTC()
	p.As = a.as != ""
	if i := a.callerAgent(st, p.By); i >= 0 {
		p.Registered = true
		p.Assignee = st.Agents[i].Party
		p.AssignedAt, p.Task = st.Agents[i].AssignedAt, st.Agents[i].Task
	}
	// A pending note is checked by its durable publication, then resolved
	// to its actual (possibly folded) number by the consumer.
	if strings.HasPrefix(p.On, reportReferencePrefix) {
		if err := a.validatePendingNote(p.On, st); err != nil {
			return err
		}
	}
	preview := p
	if strings.HasPrefix(preview.On, reportReferencePrefix) {
		preview.On = ""
	}
	if _, err := a.applyReport(st, &preview); err != nil {
		return err
	}
	m, err := state.QueueReport(a.store, p.By, p.Kind, p)
	if err != nil {
		return err
	}
	ref := reportReferencePrefix + m.ID
	if a.json {
		return a.printJSON(map[string]string{"id": m.ID, "reference": ref, "status": "queued"})
	}
	_, err = fmt.Fprintf(a.out, "%s queued: %s; the supervisor applies it in inbox order\n", p.Kind, ref)
	return err
}

func (a *app) validatePendingNote(ref string, st *state.State) error {
	id := strings.TrimPrefix(ref, reportReferencePrefix)
	if r, ok := st.ReportReceipts[id]; ok {
		if r.Note > 0 && r.Error == "" {
			return nil
		}
		return refused("%s did not create a note: %s", ref, r.Error)
	}
	messages, err := state.PendingReports(a.store)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(messages, func(m state.ReportMessage) bool { return m.ID == id && m.Kind == "note" }) {
		return refused("unknown pending note %s", ref)
	}
	return nil
}

// consumeReports is owned by the supervisor. Validation that reads GitHub
// happens before Update; only the in-memory transition and receipt commit are
// serialized with other state writers.
func (a *app) consumeReports(ctx context.Context) error {
	messages, err := state.PendingReports(a.store)
	if err != nil {
		return err
	}
	for _, m := range messages {
		if err := ctx.Err(); err != nil {
			return err
		}
		var p sessionReport
		decodeErr := json.Unmarshal(m.Payload, &p)
		p.By, p.At = m.By, m.At
		copyApp := *a
		copyApp.now = p.At
		copyApp.as = ""
		if p.As {
			copyApp.as = p.By.Name
		}
		st, err := a.store.Peek()
		if err != nil {
			return err
		}
		if _, done := st.ReportReceipts[m.ID]; done {
			if err := state.RemoveReport(a.store, m); err != nil {
				return err
			}
			continue
		}
		if decodeErr == nil && p.Kind != m.Kind {
			decodeErr = fmt.Errorf("report operation does not match its envelope")
		}
		var reviewBefore, reviewAfter []state.DeskReview
		prepareErr := decodeErr
		if prepareErr == nil && p.Kind == "idle" && p.Finished {
			reviewBefore = slices.Clone(st.DeskReviews)
			prepareErr = copyApp.captureExecutionReviewReport(ctx, st, p.By, p.Text, p.At, true)
			reviewAfter = st.DeskReviews
		}
		if prepareErr == nil && p.Kind == "park" && !strings.HasPrefix(p.On, reportReferencePrefix) {
			p.RefKind, prepareErr = resolveParkRef(ctx, p.On)
			if prepareErr == nil && isDeskReviewWait(p.Waits) && p.Worktree != "" {
				verifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				p.ReviewHead, p.ReviewReason = publishedParkVerification(verifyCtx, p.On, p.Worktree, runGit, github.RunGH)
				cancel()
			}
		}
		if prepareErr == nil && p.Kind == "note" && p.Draft.Kind != "" {
			prepareErr = copyApp.prepareNoteReport(ctx, &p)
		}
		err = a.store.Update(func(current *state.State) ([]state.Event, error) {
			if _, done := current.ReportReceipts[m.ID]; done {
				return nil, nil
			}
			receipt := state.ReportReceipt{At: a.timeNow(), By: p.By}
			if p.Registered {
				receipt.By = p.Assignee
			}
			applyErr := prepareErr
			if applyErr == nil && p.Registered {
				i := copyApp.callerAgent(current, p.By)
				if i < 0 || !current.Agents[i].Party.Is(p.Assignee) || current.Agents[i].Task != p.Task || !current.Agents[i].AssignedAt.Equal(p.AssignedAt) {
					applyErr = refused("report belongs to an ended or replaced assignment")
				}
			}
			if applyErr == nil && strings.HasPrefix(p.On, reportReferencePrefix) {
				r, ok := current.ReportReceipts[strings.TrimPrefix(p.On, reportReferencePrefix)]
				if !ok || r.Note == 0 || r.Error != "" {
					applyErr = refused("pending note did not create an open note")
				} else {
					p.On = fmt.Sprintf("#%d", r.Note)
				}
			}
			var events []state.Event
			if applyErr == nil {
				// All transitions validate before changing the state.
				events, applyErr = copyApp.applyReport(current, &p)
			}
			if applyErr != nil {
				receipt.Error = applyErr.Error()
				events = []state.Event{event(p.By, "session.report-rejected", "%s: %s", m.ID, applyErr)}
			} else {
				receipt.Note = p.NoteID
				for i, before := range reviewBefore {
					for j := range current.DeskReviews {
						if current.DeskReviews[j].ID == before.ID && current.DeskReviews[j].Head == before.Head && current.DeskReviews[j].AssignedAt.Equal(before.AssignedAt) {
							current.DeskReviews[j].Result = reviewAfter[i].Result
						}
					}
				}
			}
			if current.ReportReceipts == nil {
				current.ReportReceipts = map[string]state.ReportReceipt{}
			}
			current.ReportReceipts[m.ID] = receipt
			return events, nil
		})
		if err != nil {
			return err
		}
		if err := state.RemoveReport(a.store, m); err != nil {
			return err
		}
	}
	return a.notifyRejectedReports(ctx)
}

func (a *app) notifyRejectedReports(ctx context.Context) error {
	st, err := a.store.Peek()
	if err != nil {
		return err
	}
	for id, receipt := range st.ReportReceipts {
		if receipt.Error == "" || receipt.Notified {
			continue
		}
		message := fmt.Sprintf("Session report %s was rejected: %s. Re-read the task and send corrected input.", id, receipt.Error)
		if err := a.wakeAgent(ctx, loopParty, reportWakeTarget(receipt.By), message, ""); err != nil {
			continue
		}
		if err := a.store.Update(func(current *state.State) ([]state.Event, error) {
			r := current.ReportReceipts[id]
			r.Notified = true
			current.ReportReceipts[id] = r
			return nil, nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func reportWakeTarget(by state.Party) string {
	if by.HostSession != "" {
		return by.HostSession
	}
	if by.Session != "" {
		return by.Session
	}
	return by.Name
}

func (a *app) prepareNoteReport(ctx context.Context, p *sessionReport) error {
	if p.Note.Kind == noteDecision {
		if err := a.refuseMergeAsk(a.out, p.By, p.Draft, p.Note.Refs); err != nil {
			return err
		}
		if p.Registered {
			if err := a.routeWorkerNote(ctx, a.out, p.By, p.Draft, p.Note.Refs); err != nil {
				return err
			}
		}
	}
	if p.Checked {
		stage, err := planStages(ctx, a.cfg.Plans, p.Draft.subject())
		if err != nil {
			return err
		}
		if len(stage.Refused) > 0 {
			return refused("%s", strings.Join(stage.Refused, "; "))
		}
		p.Stage = stage
	}
	if p.Holds {
		st, err := a.store.Peek()
		if err != nil {
			return err
		}
		return a.holdAll(ctx, st.Notes, &p.Note, p.Note.Refs)
	}
	return nil
}

// The producer records its directory without starting git. The loop verifies
// its repository, branch and publication before granting review authority.
func reportWorktree() string {
	dir, _ := os.Getwd()
	return dir
}
