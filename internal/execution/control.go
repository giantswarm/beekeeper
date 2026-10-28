package execution

import (
	"encoding/json"
	"errors"
)

// ControlLedger belongs to one registered execution on the coordinator.
// Persist it in the same transaction as the mutation apply performs.
type ControlLedger struct {
	Sequence uint64                   `json:"sequence"`
	Results  map[string]ControlResult `json:"results,omitempty"`
}

func controlOperation(op string) bool {
	switch op {
	case ControlNote, ControlPark, ControlReport, "handover", "event":
		return true
	}
	return false
}

// Apply returns the original receipt for an exact replay, including an
// application refusal. Protocol conflicts and gaps call apply zero times.
// The callback must validate before mutation: a refusal is acknowledged and
// persisted, so its domain state must remain unchanged on an error.
func (l *ControlLedger) Apply(expected Ref, request ControlRequest, apply func() (string, error)) (ControlResult, error) {
	if expected.Validate() != nil || request.Ref != expected || !ValidIdentity(request.ID) || request.Sequence == 0 || !controlOperation(request.Operation) || !json.Valid(request.Data) || len(request.Data) > MaxWire/2 {
		return ControlResult{}, errors.New("invalid execution control request")
	}
	digest := request.ContentDigest()
	if old, ok := l.Results[request.ID]; ok {
		if old.Digest != digest || old.Sequence != request.Sequence {
			return ControlResult{}, errors.New("control idempotency conflict")
		}
		return old, nil
	}
	if request.Sequence-1 != l.Sequence {
		return ControlResult{}, errors.New("control sequence gap or replay")
	}
	if apply == nil {
		return ControlResult{}, errors.New("control application is unavailable")
	}
	result := ControlResult{ID: request.ID, Sequence: request.Sequence, Digest: digest}
	var err error
	result.Output, err = apply()
	if err != nil {
		result.Output = ""
		result.Error = err.Error()
	}
	if l.Results == nil {
		l.Results = map[string]ControlResult{}
	}
	l.Results[request.ID] = result
	l.Sequence = request.Sequence
	return result, nil
}
