package cmd

import "github.com/giantswarm/beekeeper/internal/state"

// mergeEvents gives only the first confirmation of a merged pull request a
// sink subject. Later confirmations keep their diagnostic details without
// dispatching the workflow again. Call under the state lock so the gate,
// its replacement and owed-outcome recovery agree on the first event.
func (a *app) mergeEvents(evs []state.Event) ([]state.Event, error) {
	seen := map[string]bool{}
	for i, e := range evs {
		if e.Verb != verbMerged || e.Subject == "" {
			continue
		}
		if seen[e.Subject] {
			evs[i].Subject = ""
			continue
		}
		prior, err := a.store.Events(1, func(old state.Event) bool {
			return old.Verb == verbMerged && old.Subject == e.Subject
		})
		if err != nil {
			return nil, err
		}
		if len(prior) != 0 {
			evs[i].Subject = ""
		}
		seen[e.Subject] = true
	}
	return evs, nil
}
