package cmd

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
)

const guideDialogVerb = "guide.dialog"
const channelGuideDialog = "guide_dialog"

type guideDialog struct {
	For      string   `json:"for"`
	Question string   `json:"question"`
	Options  []string `json:"options"`
	Note     int      `json:"note"`
	About    string   `json:"about"`
	Choice   int      `json:"choice"`
	Text     string   `json:"text"`
}

// observeGuideDialog records only completed question results in the current
// guide session, for the local note guide next is serving. A flag or a worker
// relay cannot manufacture this channel. The person never confirms twice.
func (a *app) observeGuideDialog(raw []byte) error {
	var r struct {
		Event   string `json:"hook_event_name"`
		Session string `json:"session_id"`
		Tool    string `json:"tool_name"`
		Input   struct {
			Questions []struct {
				Question string `json:"question"`
				Options  []struct {
					Label string `json:"label"`
				} `json:"options"`
			} `json:"questions"`
		} `json:"tool_input"`
		Response struct {
			Answers map[string]string `json:"answers"`
		} `json:"tool_response"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Event != guard.PostToolUseEvent || r.Tool != guard.AskTool || r.Session == "" || len(r.Input.Questions) != 1 {
		return nil
	}
	return a.store.Update(func(st *state.State) ([]state.Event, error) {
		role := guideRole.get(st)
		if role.Holder == nil || role.Holder.Session != r.Session || role.LocalAsking() == 0 {
			return nil, nil
		}
		i := slices.IndexFunc(st.Notes, func(n state.Note) bool {
			return n.ID == role.LocalAsking() && strings.EqualFold(n.For, a.cfg.Guide.Person)
		})
		if i < 0 {
			return nil, nil
		}
		n := st.Notes[i]
		if len(n.Refs) == 0 {
			return nil, nil
		}
		q := r.Input.Questions[0]
		text, ok := r.Response.Answers[q.Question]
		if !ok || strings.TrimSpace(text) == "" {
			return nil, nil
		}
		// The displayed question binds the selection to the thread and every
		// head named by the note. A stale dialog cannot answer its successor.
		if !strings.Contains(q.Question, issueURL(n.Refs[0])) {
			ref, valid := parseRef(n.Refs[0])
			if !valid || !strings.Contains(q.Question, "https://github.com/"+ref.Repo+"/pull/"+strings.TrimPrefix(refName(ref), ref.Repo+"#")) {
				return nil, nil
			}
		}
		for _, head := range shaWord.FindAllString(n.Text, -1) {
			if !strings.Contains(strings.ToLower(q.Question), strings.ToLower(head)) {
				return nil, nil
			}
		}
		if len(q.Options) != len(n.Options) {
			return nil, nil
		}
		receipt := guideDialog{Note: n.ID, About: n.Refs[0], Text: text, For: n.For, Question: n.Text, Options: n.Options}
		for i, option := range n.Options {
			label, _, _ := strings.Cut(option, ":")
			shown := strings.TrimSuffix(q.Options[i].Label, " (Recommended)")
			if strings.TrimSpace(label) != shown {
				return nil, nil
			}
			if text == q.Options[i].Label || text == shown {
				receipt.Choice = i + 1
			}
		}
		b, err := json.Marshal(receipt)
		if err != nil {
			return nil, err
		}
		return []state.Event{event(role.Holder.Party, guideDialogVerb, "%s", b)}, nil
	})
}

// dialogAnswer accepts only the exact selected option or verbatim free text
// captured by the result hook, in the same guide session, within a day.
func (a *app) dialogAnswer(me state.Party, n *state.Note, about string, choice int, words string) bool {
	if a.central || n == nil {
		return false
	}
	st, err := a.store.Read()
	if err != nil {
		return false
	}
	role := guideRole.get(st)
	if role.Holder == nil || !sameReviewSession(role.Holder.Party, me) || role.LocalAsking() != n.ID {
		return false
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == guideDialogVerb && sameReviewSession(e.By, me) })
	if err != nil {
		return false
	}
	for _, e := range slices.Backward(evs) {
		var r guideDialog
		if json.Unmarshal([]byte(e.Detail), &r) != nil || r.Note != n.ID || r.About != about || r.For != n.For || r.Question != n.Text || !slices.Equal(r.Options, n.Options) {
			continue
		}
		return !e.At.Before(n.At) && a.now.Sub(e.At) <= answerFresh && r.Choice == choice && (words == r.Text || words == "" && choice > 0) && !e.At.After(a.now.Add(time.Minute))
	}
	return false
}
