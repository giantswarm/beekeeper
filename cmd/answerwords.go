package cmd

import (
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

func questionNamesThread(question, ref string) bool {
	if strings.Contains(question, issueURL(ref)) {
		return true
	}
	r, valid := parseRef(ref)
	return valid && strings.Contains(question, "https://github.com/"+r.Repo+"/pull/"+strings.TrimPrefix(refName(r), r.Repo+"#"))
}

// Keep the person’s extra words when recording a selected option.
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

// Only a dialog naming this note’s thread contributes extra words.
func (a *app) personExtraText(st *state.State, me state.Party, n *state.Note) string {
	if n == nil {
		return ""
	}
	if len(n.Refs) == 0 {
		return ""
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
		dialogs, err := claude.DialogsSince(tr, n.At)
		if err != nil {
			continue
		}
		for _, d := range slices.Backward(dialogs) {
			if !slices.ContainsFunc(d.Questions, func(q string) bool { return questionNamesThread(q, n.Refs[0]) }) {
				continue
			}
			u := strings.TrimSpace(d.Text)
			if _, only := noteOptionLabel(n, 0, u); only || u == "" {
				return ""
			}
			for i := range n.Options {
				optLabel, _, _ := n.Option(i + 1)
				if rest := trimOptionPrefix(optLabel, u, n.Options[i]); rest != u {
					return rest
				}
			}
			return u
		}
	}
	return ""
}
