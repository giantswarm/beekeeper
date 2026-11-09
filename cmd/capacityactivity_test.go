package cmd

import (
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestCapacityActivityCountsRetiredStartsOnceAndUsesProviderAtEvent(t *testing.T) {
	now := time.Now()
	old := state.Start{Party: state.Party{Name: "worker: task", Session: continuationOldSession}, At: now.Add(-2 * time.Hour), Harness: claudeComm}
	fresh := state.Start{Party: state.Party{Name: old.Name, Session: continuationNewSession}, At: now.Add(-time.Hour), Harness: config.CodexHarness}
	outside := state.Start{Party: state.Party{Name: "yesterday", Session: "yesterday"}, At: now.Add(-25 * time.Hour), Harness: claudeComm}
	st := &state.State{Starts: []state.Start{old, fresh, outside}, EndedWorkers: []state.Start{old}}
	events := []state.Event{
		{At: now.Add(-90 * time.Minute), Verb: agentWakeEvent, Detail: old.Name + ": resumed session old headless"},
		{At: now.Add(-30 * time.Minute), Verb: agentWakeEvent, Detail: fresh.Name + ": written to its codex inbox"},
		{At: now.Add(-25 * time.Hour), Verb: agentWakeEvent, Detail: outside.Name + ": resumed session yesterday headless"},
	}
	got := activityCounts(st, events, config.Capacity{}, now)
	if got[claudeComm].FreshStarts != 1 || got[claudeComm].Wakes != 1 || got[claudeComm].Resumes != 1 || got[config.CodexHarness].FreshStarts != 1 || got[config.CodexHarness].Wakes != 1 || got[config.CodexHarness].Resumes != 0 {
		t.Fatalf("activity: %+v / %+v", got[claudeComm], got[config.CodexHarness])
	}
}
