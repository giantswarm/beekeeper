package cmd

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
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
	Notes    string   `json:"notes,omitempty"`
	Text     string   `json:"text"`
}

type guideDialogPayload struct {
	Answers      map[string]string          `json:"answers"`
	Annotations  map[string]json.RawMessage `json:"annotations"`
	Notes        json.RawMessage            `json:"notes"`
	Response     string                     `json:"response"`
	AFKTimeoutMs int                        `json:"afkTimeoutMs"`
	FollowUp     bool                       `json:"followUp"`
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
			guideDialogPayload
		} `json:"tool_input"`
		Response guideDialogPayload `json:"tool_response"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Event != guard.PostToolUseEvent || r.Tool != guard.AskTool || r.Session == "" || len(r.Input.Questions) != 1 {
		return nil
	}
	if r.Response.AFKTimeoutMs > 0 || r.Response.FollowUp || r.Input.FollowUp {
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
		for i, option := range n.Options {
			label, _, _ := strings.Cut(option, ":")
			shown := strings.TrimSuffix(strings.TrimSpace(q.Options[i].Label), recommendedSuffix)
			if strings.TrimSuffix(strings.TrimSpace(label), recommendedSuffix) != shown {
				return nil, nil
			}
		}
		annNotes, unanswered := guideDialogAnnotationFor(q.Question, r.Response.Annotations, r.Input.Annotations)
		if unanswered != "" {
			return nil, nil
		}
		extra := joinDialogText(
			annNotes,
			guideDialogNotesFor(q.Question, r.Response.Notes, r.Input.Notes),
			cmp.Or(strings.TrimSpace(r.Response.Response), strings.TrimSpace(r.Input.Response)),
		)
		ans := strings.TrimSpace(cmp.Or(r.Response.Answers[q.Question], r.Input.Answers[q.Question]))
		if ans == "(notes only)" {
			ans = ""
		}
		if ans == "" && extra == "" {
			return nil, nil
		}
		choice := 0
		for i := range n.Options {
			label, _, _ := n.Option(i + 1)
			optLabel := strings.TrimSpace(q.Options[i].Label)
			shown := strings.TrimSuffix(optLabel, recommendedSuffix)
			if ans == optLabel || ans == shown || ans == label {
				choice = i + 1
				break
			}
			if rest, ok := cutDialogOptionPrefix(ans, optLabel, shown, label); ok {
				choice = i + 1
				extra = joinDialogText(rest, extra)
				break
			}
		}
		if choice == 0 && ans != "" {
			extra = joinDialogText(ans, extra)
		}
		fullText := extra
		if choice > 0 {
			var err error
			if fullText, err = answerText(&n, choice, extra); err != nil {
				return nil, err
			}
		}
		if strings.TrimSpace(fullText) == "" {
			return nil, nil
		}
		receipt := guideDialog{Note: n.ID, About: n.Refs[0], Choice: choice, Notes: extra, Text: fullText, For: n.For, Question: n.Text, Options: n.Options}
		b, err := json.Marshal(receipt)
		if err != nil {
			return nil, err
		}
		return []state.Event{event(role.Holder.Party, guideDialogVerb, "%s", b)}, nil
	})
}

func guideDialogAnnotationFor(question string, maps ...map[string]json.RawMessage) (notes, unanswered string) {
	for _, m := range maps {
		raw, ok := m[question]
		if !ok || len(raw) == 0 {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if s = strings.TrimSpace(s); s != "" {
				return s, ""
			}
			continue
		}
		var ann struct {
			Notes      string `json:"notes"`
			Unanswered string `json:"unanswered"`
		}
		if json.Unmarshal(raw, &ann) == nil {
			if u := strings.TrimSpace(ann.Unanswered); u != "" {
				return "", u
			}
			if n := strings.TrimSpace(ann.Notes); n != "" {
				return n, ""
			}
		}
	}
	return "", ""
}

func guideDialogNotesFor(question string, raws ...json.RawMessage) string {
	for _, raw := range raws {
		if len(raw) == 0 {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
			continue
		}
		var m map[string]string
		if json.Unmarshal(raw, &m) == nil {
			if s = strings.TrimSpace(m[question]); s != "" {
				return s
			}
		}
	}
	return ""
}

func joinDialogText(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n")
}

func cutDialogOptionPrefix(ans string, labels ...string) (string, bool) {
	for _, l := range labels {
		if l == "" {
			continue
		}
		for _, sep := range []string{" — ", ": ", " - ", "\n"} {
			if rest, ok := strings.CutPrefix(ans, l+sep); ok && strings.TrimSpace(rest) != "" {
				return strings.TrimSpace(rest), true
			}
		}
	}
	return "", false
}

// dialogAnswer accepts only the exact selected option (plus any notes or free
// text the person wrote) or verbatim free text captured by the result hook, in
// the same guide session, within a day.
func (a *app) dialogAnswer(me state.Party, n *state.Note, about string, choice int, text, words string) bool {
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
		want := normalizeOptionAnswer(n, r.Text)
		got := normalizeOptionAnswer(n, cmp.Or(text, words))
		choiceOK := r.Choice == choice || choice == 0 && r.Choice > 0
		return !e.At.Before(n.At) && a.now.Sub(e.At) <= answerFresh && choiceOK && got == want && !e.At.After(a.now.Add(time.Minute))
	}
	return false
}

// refuseOptionLabelOnly refuses an answer that records only an option label
// when the person also wrote text in the dialog or in the guide session.
func (a *app) refuseOptionLabelOnly(st *state.State, me state.Party, n *state.Note, choice int, words string) error {
	label, onlyLabel := noteOptionLabel(n, choice, words)
	if !onlyLabel {
		return nil
	}
	extra := a.personExtraText(st, me, n)
	if extra == "" {
		return nil
	}
	return refused("note #%d: the person also wrote %q, so recording only option label %q loses their words; record everything they wrote verbatim", n.ID, extra, label)
}

func (a *app) personExtraText(st *state.State, me state.Party, n *state.Note) string {
	if n == nil {
		return ""
	}
	if evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == guideDialogVerb }); err == nil {
		for _, e := range slices.Backward(evs) {
			var r guideDialog
			if json.Unmarshal([]byte(e.Detail), &r) != nil || r.Note != n.ID || e.At.Before(n.At) {
				continue
			}
			if strings.TrimSpace(r.Notes) != "" {
				return strings.TrimSpace(r.Notes)
			}
			if r.Choice == 0 && strings.TrimSpace(r.Text) != "" {
				if _, only := noteOptionLabel(n, 0, r.Text); !only {
					return strings.TrimSpace(r.Text)
				}
			}
			break
		}
	}
	var parties []state.Party
	if h := guideRole.get(st).Holder; h != nil {
		parties = append(parties, h.Party)
	}
	if me.Session != "" || me.HostSession != "" {
		parties = append(parties, me)
	}
	for _, p := range parties {
		tr, _ := a.partyTranscript(p, nil)
		if tr == "" {
			continue
		}
		turns, err := claude.UserTurnsSince(tr, n.At)
		if err != nil {
			continue
		}
		for _, t := range slices.Backward(turns) {
			u := strings.TrimSpace(t.Text)
			if u == "" || strings.HasPrefix(u, "GUIDE ") || strings.HasPrefix(u, "NOTE ") || strings.HasPrefix(u, "ANSWER ") {
				continue
			}
			if _, only := noteOptionLabel(n, 0, u); only {
				continue
			}
			for i := range n.Options {
				optLabel, _, _ := n.Option(i + 1)
				if rest := trimOptionPrefix(optLabel, u); rest != u && rest != "" {
					return rest
				}
			}
			return u
		}
	}
	return ""
}
