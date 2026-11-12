package capacity

import (
	"fmt"
	"time"
)

// HostPlacementView is one execution host as a start's placement last
// judged it: the reason it was or was not selected.
type HostPlacementView struct {
	Selector   string    `json:"selector"`
	At         time.Time `json:"at,omitzero"`
	Repository string    `json:"repository,omitempty"`
	Reason     string    `json:"reason"`
	Update     string    `json:"update,omitempty"`
}

func (v HostPlacementView) Line() string {
	line := fmt.Sprintf("host %s: %s", v.Selector, v.Reason)
	if !v.At.IsZero() {
		line += fmt.Sprintf(" (last placement %s, repository %s)", v.At.UTC().Format(time.RFC3339), v.Repository)
	}
	if v.Update != "" {
		line += "; update waits: " + v.Update
	}
	return line
}
