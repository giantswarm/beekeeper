// Package feed is the shape of beekeeper://feed: the events of the shared
// state, each with an id, in a versioned schema the agent views share.
//
// A change within a schema version only adds optional fields; anything else
// is a new version, served beside the old one until its readers moved.
package feed

import (
	"cmp"
	"fmt"
	"strings"
	"time"
)

// Schema is the version every feed document names.
const Schema = "beekeeper.giantswarm.io/feed/v1"

// Feed is the feed resource's document: the most recent events, oldest
// first.
type Feed struct {
	Schema string  `json:"schema"`
	Events []Event `json:"events"`
}

// Event is one change of the shared state.
type Event struct {
	// ID orders the events: a decimal string that sorts as a number and
	// only grows, across restarts too. A reader prints the events whose ID
	// is above the last one it printed.
	ID string `json:"id"`
	// Kind is the verb, <area>.<what>: lease.claim, note.answer,
	// message.expired, ...
	Kind string `json:"kind"`
	// Subject is the record the event concerns, <Kind>/<name>, a team's
	// namespace (Namespace/beekeeper-<team>) for one without a record.
	Subject string    `json:"subject"`
	Actor   Party     `json:"actor"`
	Time    time.Time `json:"time"`
	// Line is the event as the local watch and log print it, without the
	// time.
	Line string `json:"line"`
}

// Party is who acted: the agent, its person, team and host.
type Party struct {
	Name   string `json:"name"`
	Person string `json:"person,omitempty"`
	Team   string `json:"team,omitempty"`
	Host   string `json:"host,omitempty"`
}

// Line is how an event prints: <kind> (<actor>): <detail>.
func Line(kind string, actor Party, detail string) string {
	return fmt.Sprintf("%s (%s): %s", kind, cmp.Or(actor.Name, actor.Person, "beekeeper"), strings.TrimSpace(detail))
}

// ID is an event's id from its time, in nanoseconds.
func ID(nanos int64) string { return fmt.Sprintf("%019d", nanos) }
