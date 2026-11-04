package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// consumeProductionNote binds the person's fresh answer to the complete literal
// command in the question. State.Update serializes concurrent hook processes.
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
	an, ok := newestAnswer(evs, answerQuery{Note: id})
	if !ok {
		return fmt.Errorf("note #%d has no recorded answer", id)
	}
	now := time.Now().UTC()
	if why := an.unverified(answerQuery{Note: id}, a.central, now); why != "" {
		return fmt.Errorf("note #%d: %s", id, why)
	}
	if an.At.After(now) {
		return fmt.Errorf("note #%d answer is in the future", id)
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
