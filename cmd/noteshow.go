package cmd

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/state"
)

const noteStatusOpen = "open"

// shownNote pairs the full note with its retained outcome.
type shownNote struct {
	state.Note
	Status   string        `json:"status"`
	Answer   string        `json:"answer,omitempty"`
	Reason   string        `json:"reason,omitempty"`
	Closure  *state.Event  `json:"closure,omitempty"`
	Delivery *noteDelivery `json:"delivery,omitempty"`
}

func (a *app) noteShowCmd() *cobra.Command {
	return &cobra.Command{
		Use: "show <id>", Short: "Show one note in full, including its status and retained answer",
		Long: `Show a note's full text, audience, status and retained answer or withdrawal reason, without truncation.
With --json, print one object with the note's fields, status, answer, reason and closure.
A bare number reads this machine's note, or the central instance's open note
when only it has that number; <context>#<n> selects the central instance.
Answered or withdrawn notes can be read while their complete audit event is retained. A central
store also retains closed notes for its history retention period. Expired
history cannot be recovered.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ids, err := a.parseNoteIDs(args)
			if err != nil {
				return err
			}
			id := ids[0]
			if strings.HasPrefix(args[0], "#") {
				id.Bare = false
			}
			var local *shownNote
			if !id.Central {
				local, err = a.localNote(id.ID)
				if err != nil {
					return err
				}
			}
			var central []state.Note
			var cerr error
			if a.centralNotesOn() && (id.Central || id.Bare) {
				central, cerr = a.listCentralNotes()
			}
			id, err = a.resolve(id, local != nil, central, cerr)
			if err != nil {
				return err
			}
			if id.Central {
				if cerr != nil {
					return cerr
				}
				i := slices.IndexFunc(central, func(n state.Note) bool { return n.ID == id.ID })
				if i < 0 {
					return refused("note %s#%d is not open on the central instance", a.cfg.Central.Context, id.ID)
				}
				local = &shownNote{Note: central[i], Status: noteStatusOpen}
			}
			if local == nil {
				return refused("note #%d was not found in open notes or retained history", id.ID)
			}
			if a.json {
				m, err := a.namedNote(&local.Note)
				if err != nil {
					return err
				}
				m["status"] = local.Status
				if local.Reason != "" {
					m["reason"] = local.Reason
				}
				if local.Delivery != nil {
					m["delivery"] = local.Delivery
				}
				if local.Answer != "" {
					m["answer"] = local.Answer
				}
				if local.Closure != nil {
					m["closure"] = local.Closure
				}
				if u, why, ok := noteDeferral(&local.Note); ok {
					m["deferredUntil"] = u.Format("2006-01-02T15:04:05Z07:00")
					if why != "" {
						m["deferReason"] = why
					}
				}
				return a.printJSON(m)
			}
			audience := local.For
			if audience == "" {
				audience = "unknown (not retained)"
			}
			if _, err := fmt.Fprintf(a.out, "%s [status: %s] [for %s]\n%s\n", noteRef(&local.Note), local.Status, audience, local.Text); err != nil {
				return err
			}
			if u, why, ok := noteDeferral(&local.Note); ok {
				line := u.Format("2006-01-02T15:04:05Z07:00")
				if why != "" {
					line += " (" + why + ")"
				}
				if _, err := fmt.Fprintf(a.out, "deferred until: %s\n", line); err != nil {
					return err
				}
			}
			if local.Delivery != nil {
				line := local.Delivery.Status
				if local.Delivery.Detail != "" {
					line += " " + local.Delivery.Detail
				}
				if _, err := fmt.Fprintf(a.out, "notification: %s\n", line); err != nil {
					return err
				}
			}
			if local.Default != "" {
				if _, err := fmt.Fprintf(a.out, "default: %s\n", local.Default); err != nil {
					return err
				}
			}
			if local.Reason != "" {
				if _, err := fmt.Fprintf(a.out, "reason: %s\n", local.Reason); err != nil {
					return err
				}
			}
			if local.Answer != "" {
				if _, err := fmt.Fprintf(a.out, "answer: %s\n", local.Answer); err != nil {
					return err
				}
			} else if local.Closure != nil {
				if _, err := fmt.Fprintf(a.out, "closure: %s\n", local.Closure.Detail); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func (a *app) retainedNote(id int) (*shownNote, error) {
	st, err := a.store.Read()
	if err != nil {
		return nil, err
	}
	var shown *shownNote
	for _, n := range st.Notes {
		if n.ID == id {
			shown = &shownNote{Note: n, Status: noteStatusOpen}
			break
		}
	}
	for _, n := range st.Defaulted {
		if n.ID == id {
			shown = &shownNote{Note: n, Status: "defaulted"}
			break
		}
	}
	if shown != nil && shown.Status == noteStatusOpen {
		return shown, nil
	}
	// Kubernetes retains the complete note alongside its atomic closure; file
	// stores retain the original question in answer.recorded instead.
	if history, ok := a.store.(state.NoteLookup); ok {
		n, closure, err := history.LookupNote(id)
		if err != nil {
			return nil, err
		}
		if n != nil {
			shown = &shownNote{Note: *n, Status: noteStatusOpen, Closure: closure}
			if closure != nil {
				shown.Status = noteStatusClosed
				switch closure.Verb {
				case noteDone:
					if withdrawn := withdrawnNote(*closure); withdrawn != nil {
						shown.Reason = withdrawn.Reason
					}
				case noteDefaulted:
					shown.Status = "defaulted"
				case answerRecorded:
					shown.Status = closedAnswered
					if an, ok := parseAnswered(*closure); ok {
						shown.Answer = an.Answer
					}
				}
			}
			return shown, nil
		}
	}
	evs, err := a.store.Events(1, func(e state.Event) bool {
		an, ok := parseAnswered(e)
		withdrawn := withdrawnNote(e)
		return ok && an.ID == id || withdrawn != nil && withdrawn.ID == id
	})
	if err != nil {
		return nil, err
	}
	if len(evs) > 0 {
		e := evs[len(evs)-1]
		if withdrawn := withdrawnNote(e); withdrawn != nil {
			parseNoteTextParts(&withdrawn.Note)
			return withdrawn, nil
		}
		an, _ := parseAnswered(e)
		n := state.Note{ID: id, Text: an.Question, For: an.Audience, By: state.Party{Name: an.Asker}}
		if an.About != "" && !strings.HasPrefix(an.About, "note#") {
			n.Refs = []string{an.About}
		}
		parseNoteTextParts(&n)
		return &shownNote{Note: n, Status: closedAnswered, Answer: an.Answer, Closure: &e}, nil
	}
	return shown, nil
}
