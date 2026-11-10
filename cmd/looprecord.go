package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// loopRecording includes the data for the pure planner and the separate
// continuation/reservation planners. No configuration or credentials are copied.
type loopRecording struct {
	Planner    superloop.Input
	Facts      []loopRecordedFact
	Agents     []state.Agent
	Deliveries []state.Delivery
	Room       int
}

func writeLoopRecording(dir string, in loopRecording) error {
	data, err := json.MarshalIndent(superloop.Recording(in), "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// Refuse to overwrite a previous incident's evidence.
	file, err := os.OpenFile(filepath.Join(dir, "input.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600) //nolint:gosec // explicit recording destination
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append(data, '\n'))
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

// Materialize only the callback results the planner actually asks for. Replay
// restores these facts without consulting GitHub or the desk's current state.
type loopRecordedFact struct {
	Ref   string
	Head  string
	Kind  string
	Value bool
}

func (r *loopRecording) capture(in superloop.Input) superloop.Input {
	if query := in.Open; query != nil {
		in.Open = func(ref string) bool {
			value := query(ref)
			r.Facts = append(r.Facts, loopRecordedFact{Ref: ref, Kind: "open", Value: value})
			return value
		}
	}
	wrap := func(kind string, query func(superloop.Pull, string) bool) func(superloop.Pull, string) bool {
		if query == nil {
			return nil
		}
		return func(p superloop.Pull, approved string) bool {
			value := query(p, approved)
			r.Facts = append(r.Facts, loopRecordedFact{Ref: p.Ref, Head: approved, Kind: kind, Value: value})
			return value
		}
	}
	in.BaseMerged = wrap("base-merged", in.BaseMerged)
	in.NitsChecked = wrap("nits-checked", in.NitsChecked)
	r.Planner = in
	return in
}

func (r *loopRecording) replayInput() superloop.Input {
	in := r.Planner
	lookup := func(kind, ref, head string) bool {
		for _, fact := range r.Facts {
			if fact.Kind == kind && fact.Ref == ref && fact.Head == head {
				return fact.Value
			}
		}
		return false
	}
	in.Open = func(ref string) bool { return lookup("open", ref, "") }
	in.BaseMerged = func(p superloop.Pull, approved string) bool { return lookup("base-merged", p.Ref, approved) }
	in.NitsChecked = func(p superloop.Pull, approved string) bool { return lookup("nits-checked", p.Ref, approved) }
	return in
}
