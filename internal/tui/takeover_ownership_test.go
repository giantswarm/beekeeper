package tui

import (
	"context"
	"testing"
)

type ownershipSource struct {
	*fakeSource
	owner      *string
	screen     string
	ownedCalls int
}

func (s *ownershipSource) Release(_ context.Context, _ string) error {
	*s.owner = ""
	return nil
}

func (s *ownershipSource) ReleaseOwned(_ context.Context, _ string) error {
	s.ownedCalls++
	if *s.owner == s.screen {
		*s.owner = ""
	}
	return nil
}

func TestClosingOldScreenUsesOwnedCleanup(t *testing.T) {
	owner := "B"
	src := &ownershipSource{fakeSource: &fakeSource{data: fixtureData()}, owner: &owner, screen: "A"}
	m := newModel(src, Options{}).(*model)
	m.taken = map[string]bool{"session": true}
	m.releaseAll()
	if owner != "B" || src.ownedCalls != 1 || len(m.taken) != 0 {
		t.Fatalf("cleanup: owner %q, owned calls %d, taken %v", owner, src.ownedCalls, m.taken)
	}
	src.screen = "B"
	m.taken = map[string]bool{"session": true}
	m.releaseAll()
	if owner != "" {
		t.Fatal("current screen failed to release its takeover")
	}
}
