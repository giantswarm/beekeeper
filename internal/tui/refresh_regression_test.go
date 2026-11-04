package tui

import (
	"errors"
	"testing"
)

func TestFailedRefreshKeepsTheAgeOfTheLastGoodData(t *testing.T) {
	src := &fakeSource{data: fixtureData()}
	m := newTestModel(t, src)
	t.Cleanup(m.cancel)
	m.updated = testAt
	m.Update(refreshMsg{err: errors.New("state read failed")})
	if !m.updated.Equal(testAt) {
		t.Fatalf("a failed refresh changed the last good update time: %v", m.updated)
	}
	m.Update(refreshMsg{data: fixtureData()})
	if m.updated.Equal(testAt) || m.updated.IsZero() {
		t.Fatalf("a successful refresh did not advance the update time: %v", m.updated)
	}
}

func TestFailedInitialRefreshDoesNotClaimAnUpdate(t *testing.T) {
	m := newModel(&fakeSource{}, Options{}).(*model)
	t.Cleanup(m.cancel)
	m.Update(refreshMsg{err: errors.New("state read failed")})
	if !m.updated.IsZero() {
		t.Fatalf("an initial failure claimed a successful update: %v", m.updated)
	}
}
