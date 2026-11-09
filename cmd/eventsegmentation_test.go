package cmd

import (
	"testing"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
)

func TestEventRotationWaitsForCompatibleRecoveryReader(t *testing.T) {
	r := newLoopRig(t)
	oldVersion := loopRunningVersion
	t.Cleanup(func() { loopRunningVersion = oldVersion })
	loopRunningVersion = func() string { return recoveryBadVersion }
	j := &update.Journal{Candidate: update.Binary{Version: recoveryBadVersion, Digest: "new-reader"}, Previous: &update.Binary{Version: recoveryGoodVersion, Digest: "legacy-reader"}}
	if err := r.a.recordEventReaders(j); err != nil {
		t.Fatal(err)
	}
	var policy state.EventSegmentation
	if _, err := r.a.store.ReadFile("event-segmentation.json", &policy); err != nil {
		t.Fatal(err)
	}
	if policy.Enabled || !policy.Readers["new-reader"] || policy.Readers["legacy-reader"] {
		t.Fatalf("first hop would hide history from legacy rollback: %+v", policy)
	}
	previous := j.Candidate
	j.Previous = &previous
	j.Candidate = update.Binary{Version: recoveryBadVersion, Digest: "next-reader"}
	if err := r.a.recordEventReaders(j); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.store.ReadFile("event-segmentation.json", &policy); err != nil || !policy.Enabled {
		t.Fatalf("compatible signed recovery target cannot rotate: %+v %v", policy, err)
	}
}
