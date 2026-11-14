package loop

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	LoopRecordInputFile = "input.json"
	loopFactBaseMerged  = "base-merged"
	loopFactNitsChecked = "nits-checked"
)

// LoopRecording includes the data for the pure planner and the separate
// continuation/reservation planners. No configuration or credentials are copied.
type LoopRecording struct {
	Planner    superloop.Input
	Facts      []loopRecordedFact
	Agents     []state.Agent
	Deliveries []state.Delivery
	Room       int
}

func WriteLoopRecording(dir string, in LoopRecording) error {
	data, err := json.MarshalIndent(superloop.Recording(in), "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// Refuse to overwrite a previous incident's evidence.
	file, err := os.OpenFile(filepath.Join(dir, LoopRecordInputFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600) //nolint:gosec // explicit recording destination
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
	Ref  string
	Head string
	// PullHead is the pull request head a base-merge or nits fact was read
	// against: one approved head is read against several (superloop.Replaced
	// reads the head a review came on and the current one).
	PullHead string
	Kind     string
	Value    bool
}

func (r *LoopRecording) Capture(in superloop.Input) superloop.Input {
	if query := in.Open; query != nil {
		in.Open = func(ref string) bool {
			value := query(ref)
			r.Facts = append(r.Facts, loopRecordedFact{Ref: ref, Kind: noteStatusOpen, Value: value})
			return value
		}
	}
	wrap := func(kind string, query func(superloop.Pull, string) bool) func(superloop.Pull, string) bool {
		if query == nil {
			return nil
		}
		return func(p superloop.Pull, approved string) bool {
			value := query(p, approved)
			r.Facts = append(r.Facts, loopRecordedFact{Ref: p.Ref, Head: approved, PullHead: p.Head, Kind: kind, Value: value})
			return value
		}
	}
	in.BaseMerged = wrap(loopFactBaseMerged, in.BaseMerged)
	in.NitsChecked = wrap(loopFactNitsChecked, in.NitsChecked)
	r.Planner = in
	return in
}

func (r *LoopRecording) ReplayInput() superloop.Input {
	in := r.Planner
	// A fact recorded without its pull request head (an older recording)
	// answers for any head.
	lookup := func(kind, ref, head, pullHead string) bool {
		for _, fact := range r.Facts {
			if fact.Kind == kind && fact.Ref == ref && fact.Head == head && (fact.PullHead == "" || fact.PullHead == pullHead) {
				return fact.Value
			}
		}
		return false
	}
	in.Open = func(ref string) bool { return lookup(noteStatusOpen, ref, "", "") }
	in.BaseMerged = func(p superloop.Pull, approved string) bool {
		return lookup(loopFactBaseMerged, p.Ref, approved, p.Head)
	}
	in.NitsChecked = func(p superloop.Pull, approved string) bool {
		return lookup(loopFactNitsChecked, p.Ref, approved, p.Head)
	}
	return in
}
