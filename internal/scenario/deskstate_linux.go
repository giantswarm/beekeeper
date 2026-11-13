//go:build scenario && linux

package scenario

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// seedState writes given.state into the desk's state store before the
// first step. The store is opened without a release version: a seeded
// state carries no writer's stamp, so any candidate may save it.
func (d *driver) seedState() error {
	g := d.s.Given.State
	if g == nil || g.Generate == nil {
		return nil
	}
	gen := *g.Generate
	if gen.Repository == "" {
		gen.Repository = d.repositories()[0]
	}
	now := d.clock.Now().UTC()
	store, err := state.OpenVersion(stateDir, "")
	if err != nil {
		return err
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		generateState(st, now, gen)
		return nil, nil
	}); err != nil {
		return fmt.Errorf("given.state: %w", err)
	}
	if err := generateEvents(filepath.Join(stateDir, "events.jsonl"), now, gen); err != nil {
		return fmt.Errorf("given.state: %w", err)
	}
	return nil
}

// generatedPerson is whose the generated desk's sessions are: the
// scenario's person.
const generatedPerson = "scenario"

// generateState fills st with gen's roster and what a coordinator keeps
// of its past: starts, serve records, deliveries and reports.
func generateState(st *state.State, now time.Time, gen Generate) {
	guide := state.Party{Name: "generated guide", Person: generatedPerson}
	session := func(kind, i int) string { return fmt.Sprintf("%04x%04x-0000-4000-8000-%012x", kind, i, i) }
	issue := func(n int) string { return fmt.Sprintf("%s#%d", gen.Repository, n) }
	for i := range gen.Agents {
		p := state.Party{Session: session(1, i), Name: fmt.Sprintf("generated worker %d", i), Person: generatedPerson, Host: deskIdentity}
		task := fmt.Sprintf("Work https://github.com/%s/issues/%d to the finish", gen.Repository, 10000+i)
		registered := now.Add(-time.Duration(gen.Agents-i) * time.Hour)
		st.Agents = append(st.Agents, state.Agent{Party: p, Registered: registered, ActivityAt: now.Add(-3 * time.Hour), IdleSince: now.Add(-2 * time.Hour), LastTask: task})
		st.Starts = append(st.Starts, state.Start{Party: p, Mode: state.ModeBypass, Dir: repoDir, By: guide, At: registered, Harness: "claude", Model: "sonnet", Task: task})
	}
	for i := range gen.Records {
		p := state.Party{Session: session(2, i), Name: fmt.Sprintf("past worker %d", i), Person: generatedPerson, Host: deskIdentity}
		at := now.Add(-time.Duration(gen.Records-i) * time.Hour)
		st.Records = append(st.Records, state.Record{Session: p, Issue: issue(20000 + i), By: guide, At: at, Ended: at.Add(30 * time.Minute)})
	}
	for i := range gen.Deliveries {
		st.Deliveries = append(st.Deliveries, state.Delivery{
			Ref: issue(30000 + i), Pulls: []string{issue(40000 + i)}, Origin: state.DeliveryOriginStarted,
			Step: state.DeliveryReviewing, Started: now.Add(-3 * time.Hour), Since: now.Add(-time.Hour),
			Agent: fmt.Sprintf("past worker %d", i),
		})
	}
	for i := range gen.Reports {
		by := state.Party{Session: session(2, i), Name: fmt.Sprintf("past worker %d", i), Person: generatedPerson, Host: deskIdentity}
		st.WorkerReports = append(st.WorkerReports, state.WorkerReport{
			Finished: true, By: by, At: now.Add(-time.Duration(i+1) * time.Minute),
			Task: fmt.Sprintf("Work https://github.com/%s/issues/%d to the finish", gen.Repository, 20000+i),
			Text: fmt.Sprintf("https://github.com/%s/pull/%d merged; %s", gen.Repository, 50000+i, strings.Repeat("proof ", 140)),
		})
	}
}

// generateEvents appends gen.Events events of a coordinator's usual mix to
// the log at path, one a second up to now.
func generateEvents(path string, now time.Time, gen Generate) error {
	if gen.Events == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // the scenario's own event log
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	verbs := []string{"supervisor.stage", "agents.park", "agents.wake", "note.add", "supervisor.review", "agents.idle", "board.move", "delivery.step"}
	since := now.Add(-time.Duration(gen.Events) * time.Second)
	for i := range gen.Events {
		raw, err := json.Marshal(state.Event{
			At:      since.Add(time.Duration(i) * time.Second),
			By:      state.Party{Name: fmt.Sprintf("past worker %d", i%max(1, gen.Records)), Person: generatedPerson},
			Verb:    verbs[i%len(verbs)],
			Subject: fmt.Sprintf("%s#%d", gen.Repository, 40000+i%max(1, gen.Deliveries)),
			Detail:  fmt.Sprintf("[generated %d] %s", i, strings.Repeat("detail ", 40)),
		})
		if err == nil {
			_, err = w.Write(append(raw, '\n'))
		}
		if err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
