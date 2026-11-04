package tui

import (
	"errors"
	"testing"
)

func TestReopenedPaneDropsItsPreviousTail(t *testing.T) {
	src := &fakeSource{data: fixtureData(), turns: []Turn{{Text: "old transcript"}}}
	m := newTestModel(t, src)
	t.Cleanup(m.cancel)
	m.setTab(sessionsTab)
	_, first := m.enter()
	old := first()
	m.closeDetail()
	_, second := m.enter()

	m.Update(old)
	if !m.tailing || m.tailState != 1 || len(m.tail) != 0 {
		t.Fatalf("the previous opening changed the new read: tailing %v, state %d, tail %+v", m.tailing, m.tailState, m.tail)
	}
	if cmd := m.tailCmd(m.detail); cmd != nil {
		t.Fatal("the stale result allowed another read while the new read is outstanding")
	}

	src.turns = []Turn{{Text: "new transcript"}}
	m.Update(second())
	m.Update(old)
	if m.tailing || m.tailState != 2 || len(m.tail) != 1 || m.tail[0].Text != "new transcript" {
		t.Fatalf("a late result replaced the new transcript: tailing %v, state %d, tail %+v", m.tailing, m.tailState, m.tail)
	}
}

func TestDeliveryOutcomeBelongsToItsPaneOpening(t *testing.T) {
	for _, reopened := range []bool{false, true} {
		for _, failed := range []bool{false, true} {
			name := "other session"
			if reopened {
				name = "same session reopened"
			}
			if failed {
				name += " failed delivery"
			}
			t.Run(name, func(t *testing.T) {
				src := &fakeSource{data: fixtureData()}
				if failed {
					src.sendErr = errors.New("old delivery failed")
				}
				m := newTestModel(t, src)
				t.Cleanup(m.cancel)
				m.setTab(sessionsTab)
				m.enter()
				cmd := m.sendCmd(m.detail, "hello")
				m.closeDetail()
				if !reopened {
					m.sel[sessionsTab] = 1
				}
				m.enter()
				m.outcome, m.outcomeErr = "current pane outcome", true

				m.Update(cmd())
				if m.sending || m.outcome != "current pane outcome" || !m.outcomeErr {
					t.Fatalf("the old delivery changed the current pane: sending %v, outcome %q, error %v", m.sending, m.outcome, m.outcomeErr)
				}
			})
		}
	}
}

func TestLateTakeoverStillTracksOwnershipWithoutChangingAnotherPane(t *testing.T) {
	src := &fakeSource{data: fixtureData()}
	src.data.Sessions[0].ID = "sid-bee"
	m := newTestModel(t, src)
	t.Cleanup(m.cancel)
	m.setTab(sessionsTab)
	m.enter()
	cmd := m.toggleTakeOver()
	m.closeDetail()
	m.sel[sessionsTab] = 1
	m.enter()
	m.outcome = "current pane outcome"

	m.Update(cmd())
	if !m.taken["sid-bee"] || m.acting {
		t.Fatalf("the completion lost ownership: taken %v, acting %v", m.taken, m.acting)
	}
	if m.outcome != "current pane outcome" {
		t.Fatalf("the other session's take-over changed this pane: %q", m.outcome)
	}
}

func TestLateApprovalErrorDoesNotChangeAnotherPane(t *testing.T) {
	src := &fakeSource{data: fixtureData(), answerErr: errors.New("old request expired")}
	src.data.Sessions[0].ID = "sid-bee"
	src.data.Sessions[0].Approvals = []Approval{{ID: "request", Gist: "old approval"}}
	m := newTestModel(t, src)
	t.Cleanup(m.cancel)
	m.setTab(sessionsTab)
	m.enter()
	cmd := m.answer(false)
	m.closeDetail()
	m.sel[sessionsTab] = 1
	m.enter()
	m.outcome = "current pane outcome"

	m.Update(cmd())
	if m.acting || m.outcome != "current pane outcome" || m.outcomeErr {
		t.Fatalf("the other session's error changed this pane: acting %v, outcome %q, error %v", m.acting, m.outcome, m.outcomeErr)
	}
}
