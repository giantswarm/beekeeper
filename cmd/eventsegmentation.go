package cmd

import (
	"os"

	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
)

// Stage the on-disk layout transition across ordinary signed updates. A first
// candidate records its own verified journal digest as an archive-capable reader
// but keeps the legacy layout for its old recovery target. Once that candidate
// is the next update's verified Previous, rotation is rollback-compatible too.
func (a *app) prepareEventSegmentation() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	j, err := update.Inspect(exe)
	if err != nil || j == nil {
		return err
	}
	return a.recordEventReaders(j)
}

func (a *app) recordEventReaders(j *update.Journal) error {
	var policy state.EventSegmentation
	if _, err := a.store.ReadFile("event-segmentation.json", &policy); err != nil {
		return err
	}
	if policy.Readers == nil {
		policy.Readers = map[string]bool{}
	}
	if j.Candidate.Digest != "" && loop.NormalizeLoopVersion(j.Candidate.Version) == loop.NormalizeLoopVersion(loop.LoopRunningVersion()) {
		policy.Readers[j.Candidate.Digest] = true
	}
	policy.Enabled = j.Previous != nil && policy.Readers[j.Previous.Digest]
	// Only the candidate and its recovery target matter to the next hop.
	readers := map[string]bool{}
	if policy.Readers[j.Candidate.Digest] {
		readers[j.Candidate.Digest] = true
	}
	if j.Previous != nil && policy.Readers[j.Previous.Digest] {
		readers[j.Previous.Digest] = true
	}
	policy.Readers = readers
	return a.store.WriteFile("event-segmentation.json", policy)
}
