package execution

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestControlLedgerRejectsConflictsBeforeMutation(t *testing.T) {
	ref := Ref{Host: fixtureHost, Execution: ExecutionID(uuid.NewString()), Generation: 1}
	first := ControlRequest{Ref: ref, ID: "note-one", Sequence: 1, Operation: ControlNote, Data: json.RawMessage(`{"question":"fixture?"}`)}
	var ledger ControlLedger
	applied := 0
	const nextControlID = "note-two"
	apply := func() (string, error) { applied++; return "original note number", nil }
	receipt, err := ledger.Apply(ref, first, apply)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		change func(*ControlRequest)
	}{
		{"different content", func(r *ControlRequest) { r.Data = json.RawMessage(`{"question":"different?"}`) }},
		{"different sequence", func(r *ControlRequest) { r.Sequence++ }},
		{"wrong generation", func(r *ControlRequest) { r.Ref.Generation++ }},
		{"replayed sequence", func(r *ControlRequest) { r.ID = nextControlID }},
		{"sequence gap", func(r *ControlRequest) { r.ID = nextControlID; r.Sequence = 3 }},
		{"unsupported authority", func(r *ControlRequest) { r.ID = "merge-one"; r.Sequence = 2; r.Operation = "merge" }},
		{"invalid data", func(r *ControlRequest) { r.ID = nextControlID; r.Sequence = 2; r.Data = json.RawMessage(`{`) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := first
			test.change(&request)
			if _, err := ledger.Apply(ref, request, apply); err == nil {
				t.Fatal("invalid control accepted")
			}
			if applied != 1 || ledger.Sequence != 1 {
				t.Fatal("invalid control mutated application or ledger")
			}
		})
	}
	// Reconstruct the coordinator ledger from persisted state. A replay's
	// application callback must not run, even if no caller remembers it.
	var restarted ControlLedger
	if err := Decode(mustJSON(ledger), &restarted); err != nil {
		t.Fatal(err)
	}
	replayed, err := restarted.Apply(ref, first, apply)
	if err != nil || replayed != receipt || applied != 1 {
		t.Fatal("restarted coordinator reapplied an acknowledged mutation")
	}
}

func TestControlLedgerPersistsRefusalAndAllowsNextSequence(t *testing.T) {
	ref := Ref{Host: fixtureHost, Execution: ExecutionID(uuid.NewString()), Generation: 1}
	r := ControlRequest{Ref: ref, ID: "report-one", Sequence: 1, Operation: ControlReport, Data: json.RawMessage(`{}`)}
	var ledger ControlLedger
	calls := 0
	apply := func() (string, error) { calls++; return "", errors.New("fixture report refused") }
	ack, err := ledger.Apply(ref, r, apply)
	if err != nil || ack.Error == "" {
		t.Fatal("application refusal was not acknowledged")
	}
	if _, err := ledger.Apply(ref, r, apply); err != nil || calls != 1 {
		t.Fatal("refused mutation was retried")
	}
	r.ID = "report-two"
	r.Sequence++
	if _, err := ledger.Apply(ref, r, apply); err != nil || calls != 2 {
		t.Fatal("acknowledged refusal blocked the next mutation")
	}
}
