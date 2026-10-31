package state

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/execution"
)

func TestExecutionReservationNeedsTerminationEvidence(t *testing.T) {
	tests := []struct {
		name     string
		record   ExecutionRecord
		terminal bool
	}{
		{name: "pending reservation", record: ExecutionRecord{Phase: "pending"}},
		{name: "failed coordinator attempt", record: ExecutionRecord{Phase: execution.Failed}},
		{name: "unreachable running process", record: ExecutionRecord{Observation: execution.Observation{Reachability: execution.Unreachable, ProcessState: execution.Running}}},
		{name: "unconfirmed stop", record: ExecutionRecord{StopRequest: &execution.StopRequest{ID: "fixture"}, Observation: execution.Observation{ProcessState: execution.Exited}}},
		{name: "acknowledged stop", record: ExecutionRecord{StopRequest: &execution.StopRequest{ID: "fixture"}, Observation: execution.Observation{StopAcknowledged: true}}, terminal: true},
		{name: "confirmed exit", record: ExecutionRecord{Observation: execution.Observation{ProcessState: execution.Exited}}, terminal: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.record.Terminal() != test.terminal {
				t.Fatal("reservation changed without matching lifecycle evidence")
			}
		})
	}
}

func TestExecutionJournalPreservesUnknownFields(t *testing.T) {
	var start Start
	raw := `{"session":"fixture","mode":"acceptEdits","futureStart":"keep-start","execution":{"phase":"pending","futureExecution":{"preserve":true},"messages":[{"key":"fixture","stage":"stored","futureDelivery":{"retain":true}}]}}`
	if err := json.Unmarshal([]byte(raw), &start); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(start)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"futureStart":"keep-start"`) || !strings.Contains(string(b), `"futureExecution":{"preserve":true}`) {
		t.Fatal("execution journal lost fields from a newer writer")
	}
	if !strings.Contains(string(b), `"futureDelivery":{"retain":true}`) {
		t.Fatal("execution outbox lost a newer writer's receipt metadata")
	}
}
