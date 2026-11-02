package cmd

import (
	"encoding/json"

	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	noteDeliveryEvent   = "note.delivery"
	noteDeliveryFailed  = "failed"
	noteDeliveryWaiting = "waiting"
)

// noteDelivery retains the notification outcome independently of the answer:
// recording an answer does not mean its session received it.
type noteDelivery struct {
	Note   int    `json:"note"`
	Team   string `json:"team,omitempty"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func (a *app) recordNoteDelivery(by state.Party, d noteDelivery) error {
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return a.store.Record(event(by, noteDeliveryEvent, "%s", data))
}

func (a *app) localNote(id int) (*shownNote, error) {
	n, err := a.retainedNote(id)
	if err != nil || n == nil {
		return n, err
	}
	evs, err := a.store.Events(1, func(e state.Event) bool {
		var d noteDelivery
		return e.Verb == noteDeliveryEvent && json.Unmarshal([]byte(e.Detail), &d) == nil && d.Note == id && (!a.central || d.Team == n.By.Team)
	})
	if err != nil {
		return nil, err
	}
	if len(evs) > 0 {
		var d noteDelivery
		if err := json.Unmarshal([]byte(evs[len(evs)-1].Detail), &d); err != nil {
			return nil, err
		}
		n.Delivery = &d
	}
	return n, nil
}
