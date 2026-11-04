package tui

import (
	tea "charm.land/bubbletea/v2"
	"slices"
	"strings"
	"testing"
)

func TestNamesakeSessionPaneKeepsTheSelectedID(t *testing.T) {
	src := &fakeSource{data: fixtureData()}
	src.data.Sessions = []Session{
		{Name: "shared", ID: "sid-A", PID: 100, Approvals: []Approval{{ID: "approval-A", Gist: "first approval"}}},
		{Name: "shared", ID: "sid-B", PID: 101, Approvals: []Approval{{ID: "approval-B", Gist: "second approval"}}},
	}
	m := newTestModel(t, src)
	t.Cleanup(m.cancel)
	m.setTab(sessionsTab)
	m.sel[sessionsTab] = 1
	_, tail := m.enter()
	m.Update(tail())
	if s := m.paneSession(); s == nil || s.ID != "sid-B" || src.tailCall != "sid-B" {
		t.Fatalf("pane or transcript targets another session: %+v, Tail(%q)", s, src.tailCall)
	}
	if body := m.screen(); !strings.Contains(body, "second approval") || strings.Contains(body, "first approval") {
		t.Fatalf("pane rendered another session's approval: %s", body)
	}
	m.Update(m.toggleTakeOver()())
	if !slices.Equal(src.took, []string{"sid-B"}) {
		t.Fatalf("TakeOver targets %v", src.took)
	}
	m.Update(m.answer(true)())
	if !slices.Equal(src.answers, []string{"sid-B/approval-B allow"}) {
		t.Fatalf("Answer targets %v", src.answers)
	}
	m.key(keyText("m"))
	m.key(keyText("hello"))
	_, send := m.key(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.Update(send())
	if src.sendTo != "sid-B" {
		t.Fatalf("Send targets %q", src.sendTo)
	}
}

func TestSessionSelectionSurvivesRenameAndReorder(t *testing.T) {
	src := &fakeSource{data: fixtureData()}
	src.data.Sessions[0].ID, src.data.Sessions[1].ID = "sid-A", "sid-B"
	src.data.Sessions[0].Name, src.data.Sessions[1].Name = "shared", "shared"
	m := newTestModel(t, src)
	t.Cleanup(m.cancel)
	m.setTab(sessionsTab)
	m.sel[sessionsTab] = 1
	m.enter()
	d := *src.data
	d.Sessions = slices.Clone(d.Sessions)
	d.Sessions[1].Name = "renamed"
	slices.Reverse(d.Sessions)
	m.Update(refreshMsg{data: &d})
	if m.sel[sessionsTab] != 0 || m.paneSession() == nil || m.paneSession().ID != "sid-B" || !strings.Contains(m.screen(), "renamed") {
		t.Fatalf("selection or pane lost the selected identity: row %d, pane %+v", m.sel[sessionsTab], m.paneSession())
	}
}

func TestClosedSessionDoesNotRetargetToAReusedName(t *testing.T) {
	src := &fakeSource{data: fixtureData()}
	src.data.Sessions[0].ID = "old-session"
	m := newTestModel(t, src)
	t.Cleanup(m.cancel)
	m.setTab(sessionsTab)
	m.enter()
	m.key(keyText("m"))
	m.key(keyText("draft"))
	d := *src.data
	d.Sessions = slices.Clone(d.Sessions)
	d.Sessions[0].ID = "replacement"
	m.Update(refreshMsg{data: &d})
	if m.detail != "" || m.composing || m.paneSession() != nil {
		t.Fatal("the replacement inherited the open pane or draft")
	}
	if m.toggleTakeOver() != nil || m.answer(true) != nil {
		t.Fatal("a closed session still permits actions")
	}
	if src.sendTo != "" || len(src.took) != 0 || len(src.answers) != 0 {
		t.Fatal("an action reached the replacement")
	}
}

func TestSessionIdentityFallsBackToPID(t *testing.T) {
	src := &fakeSource{data: fixtureData()}
	src.data.Sessions[0].Name, src.data.Sessions[1].Name = "shared", "shared"
	m := newTestModel(t, src)
	t.Cleanup(m.cancel)
	m.setTab(sessionsTab)
	m.sel[sessionsTab] = 1
	_, tail := m.enter()
	m.Update(tail())
	if m.paneSession().PID != 101 || src.tailCall != "101" {
		t.Fatalf("PID fallback selected %+v, Tail(%q)", m.paneSession(), src.tailCall)
	}
	m.Update(m.sendCmd(m.detail, "hello")())
	if src.sendTo != "101" {
		t.Fatalf("Send(%q) ignored PID identity", src.sendTo)
	}
}
