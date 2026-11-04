package tui

import (
	"errors"
	"testing"
)

func TestFailedReleaseRemainsOwnedForShutdown(t *testing.T) {
	src := &fakeSource{data: fixtureData(), releaseErr: errors.New("temporary release failure")}
	src.data.Sessions[0].ID = tBeeID
	src.data.Sessions[0].TakenOver = true
	m := newTestModel(t, src)
	t.Cleanup(m.cancel)
	m.taken = map[string]bool{tBeeID: true}
	m.setTab(sessionsTab)
	m.enter()
	cmd := m.toggleTakeOver()
	if !m.taken[tBeeID] {
		t.Fatal("scheduling a release discarded ownership before it succeeded")
	}
	m.Update(cmd())
	if !m.taken[tBeeID] || !m.outcomeErr {
		t.Fatalf("a failed release discarded ownership or hid its error: taken %v, error %v", m.taken, m.outcomeErr)
	}

	src.releaseErr = nil
	m.releaseAll()
	if len(src.released) != 2 || src.released[0] != tBeeID || src.released[1] != tBeeID {
		t.Fatalf("shutdown did not retry the failed release: %v", src.released)
	}
}

func TestSuccessfulReleaseDropsOwnershipAfterCompletion(t *testing.T) {
	src := &fakeSource{data: fixtureData()}
	src.data.Sessions[0].ID = tBeeID
	src.data.Sessions[0].TakenOver = true
	m := newTestModel(t, src)
	t.Cleanup(m.cancel)
	m.taken = map[string]bool{tBeeID: true}
	m.setTab(sessionsTab)
	m.enter()
	cmd := m.toggleTakeOver()
	result := cmd()
	if !m.taken[tBeeID] {
		t.Fatal("ownership disappeared before the completion was handled")
	}
	m.Update(result)
	if m.taken[tBeeID] {
		t.Fatal("a completed release kept ownership")
	}
	m.releaseAll()
	if len(src.released) != 1 {
		t.Fatalf("shutdown released an already released session: %v", src.released)
	}
}
