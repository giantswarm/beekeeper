//go:build scenario

package scenario

import (
	"errors"
	"fmt"
	"slices"
)

// State seeds the desk's state before the scenario's first step.
type State struct {
	// Generate fills the state and the event log like a coordinator's
	// that has worked for weeks.
	Generate *Generate `yaml:"generate"`
}

// Generate is the size of a generated desk. What it fills is at rest: the
// workers are idle, the serve records are of sessions that ended, and the
// deliveries are in flight in a repository of the scenario, each with a
// pull request under review. Every command reads all of it, which is what
// a large desk costs.
type Generate struct {
	// Repository is the repository the work is in (owner/name): the
	// scenario's first when empty.
	Repository string `yaml:"repository"`
	// Agents are idle workers on the roster, each with its start.
	Agents int `yaml:"agents"`
	// Records are serve records of sessions that ended.
	Records int `yaml:"records"`
	// Deliveries are deliveries in flight.
	Deliveries int `yaml:"deliveries"`
	// Reports are finished workers' reports, about a kibibyte each.
	Reports int `yaml:"reports"`
	// Events are events in the log before the scenario's start, about
	// 400 bytes each.
	Events int `yaml:"events"`
}

// The most a generated desk holds of each: several times the largest
// coordinator's.
const (
	maxGenerated       = 10000
	maxGeneratedEvents = 200000
)

func (s *State) validate(repositories []string) error {
	if s == nil || s.Generate == nil {
		return nil
	}
	g := s.Generate
	var errs []error
	for name, n := range map[string]int{"agents": g.Agents, "records": g.Records, "deliveries": g.Deliveries, "reports": g.Reports} {
		if n < 0 || n > maxGenerated {
			errs = append(errs, fmt.Errorf("state.generate.%s: %d is not within 0 and %d", name, n, maxGenerated))
		}
	}
	if g.Events < 0 || g.Events > maxGeneratedEvents {
		errs = append(errs, fmt.Errorf("state.generate.events: %d is not within 0 and %d", g.Events, maxGeneratedEvents))
	}
	if g.Agents+g.Records+g.Deliveries+g.Reports+g.Events == 0 {
		errs = append(errs, errors.New("state.generate: nothing to generate"))
	}
	switch {
	case len(repositories) == 0:
		errs = append(errs, errors.New("state.generate: the work is in a repository of the scenario, which has none"))
	case g.Repository != "" && !slices.Contains(repositories, g.Repository):
		errs = append(errs, fmt.Errorf("state.generate.repository: %q is none of the scenario's repositories", g.Repository))
	}
	return errors.Join(errs...)
}
