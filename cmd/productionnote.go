package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// productionNoteFresh is how long an answer authorises its deletion.
const productionNoteFresh = 24 * time.Hour

// productionNoteAuthority is what the hook consumes a note with, nil unless
// kube.productionNoteDeletes switches the path on.
func (a *app) productionNoteAuthority() func(int, string) error {
	if !a.cfg.Kube.ProductionNoteDeletes {
		return nil
	}
	return a.consumeProductionNote
}

// consumeProductionNote binds a note's fresh answer to the complete literal
// command in its question. State.Update serializes concurrent hook processes.
//
// answer.recorded does not prove who answered: an agent able to record an answer
// passes this check. That is why the path is off unless configured.
func (a *app) consumeProductionNote(id int, command string) error {
	if a.store == nil {
		if a.cfg == nil {
			return fmt.Errorf("answer store is unavailable")
		}
		store, err := state.Open(a.cfg.StateDir)
		if err != nil {
			return fmt.Errorf("open answer store: %w", err)
		}
		a.store = store
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == answerRecorded })
	if err != nil {
		return err
	}
	var an answered
	found := false
	for _, e := range evs {
		if parsed, ok := parseAnswered(e); ok && parsed.ID == id {
			an, found = parsed, true
		}
	}
	if !found {
		return fmt.Errorf("note #%d has no recorded answer", id)
	}
	now := time.Now().UTC()
	if an.At.After(now) {
		return fmt.Errorf("note #%d answer is in the future", id)
	}
	if now.Sub(an.At) > productionNoteFresh {
		return fmt.Errorf("note #%d answer is older than %s", id, shortDur(productionNoteFresh))
	}
	// Require the whole command as a quoted code span, rather than inferring
	// authority from object names appearing in unrelated prose.
	if !strings.Contains(an.Question, "`"+command+"`") {
		return fmt.Errorf("note #%d question does not name this exact command", id)
	}
	switch strings.ToLower(strings.TrimSpace(an.Answer)) {
	case "yes", "approve", "approved", "delete it":
	default:
		return fmt.Errorf("note #%d answer does not explicitly approve the write", id)
	}
	return a.store.Update(func(st *state.State) ([]state.Event, error) {
		if _, used := st.ProductionNotes[id]; used {
			return nil, fmt.Errorf("note #%d has already been used for a production write", id)
		}
		if st.ProductionNotes == nil {
			st.ProductionNotes = map[int]time.Time{}
		}
		st.ProductionNotes[id] = now
		return []state.Event{{At: now, Verb: "production.write", Detail: fmt.Sprintf("note #%d authorises %s", id, command)}}, nil
	})
}
