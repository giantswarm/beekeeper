package tui

import (
	tea "charm.land/bubbletea/v2"
	"fmt"
	"strings"
	"testing"
)

func bodyKey(m *model, code rune) {
	m.key(tea.KeyPressMsg{Code: code, Mod: tea.ModCtrl})
}

func collectBody(t *testing.T, m *model, markers []string) {
	t.Helper()
	bodyKey(m, tea.KeyHome)
	var seen strings.Builder
	// Each page overlaps by one line. The fixture has fewer than 100 pages.
	for range 100 {
		body := bodyView(m, m.width, m.bodyH())
		if n := strings.Count(body, "\n") + 1; n > m.bodyH() {
			t.Fatalf("body has %d lines", n)
		}
		seen.WriteString(body)
		seen.WriteByte('\n')
		before := m.bodyOffset
		bodyKey(m, 'd')
		if m.bodyOffset == before {
			break
		}
	}
	for _, marker := range markers {
		if !strings.Contains(seen.String(), marker) {
			t.Errorf("unreachable content %q", marker)
		}
	}
	bodyKey(m, tea.KeyEnd)
	end := m.bodyOffset
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 9})
	bodyKey(m, 'u')
	if m.bodyOffset > end {
		t.Fatal("resize trapped body browsing")
	}
	bodyKey(m, tea.KeyHome)
	if m.bodyOffset != 0 {
		t.Fatal("cannot return to the beginning")
	}
}

func TestAllAlertLinesAreReachableWithOneInstallation(t *testing.T) {
	d := fixtureData()
	d.Alerts = []AlertsFor{{Installation: "single-installation", Reachable: true}}
	markers := []string{"single-installation", "github budget"}
	for i := range 40 {
		name := fmt.Sprintf("unique-alert-%02d", i)
		d.Alerts[0].Alerts = append(d.Alerts[0].Alerts, Alert{Alertname: name, Severity: "warning"})
		markers = append(markers, name)
	}
	m := newTestModel(t, &fakeSource{data: d})
	t.Cleanup(m.cancel)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 12})
	m.setTab(4)
	collectBody(t, m, markers)
	if m.sel[4] != 0 {
		t.Fatal("body browsing changed the selected installation")
	}
}

func TestAuxiliarySectionsScrollWithZeroOrOnePrimaryRow(t *testing.T) {
	for _, tab := range []int{0, 2, 3} {
		t.Run(tabs[tab], func(t *testing.T) {
			d := fixtureData()
			d.Leases.Held = nil
			d.Roles = d.Roles[:1]
			var markers []string
			for i := range 40 {
				marker := fmt.Sprintf("item-%02d", i)
				markers = append(markers, marker)
				switch tab {
				case 0:
					d.Machine.Clusters = append(d.Machine.Clusters, Cluster{Name: marker})
				case 2:
					d.Holds = append(d.Holds, Hold{Target: marker})
				case 3:
					d.Agents = append(d.Agents, Agent{Name: marker})
				}
			}
			m := newTestModel(t, &fakeSource{data: d})
			t.Cleanup(m.cancel)
			m.Update(tea.WindowSizeMsg{Width: 100, Height: 12})
			m.setTab(tab)
			collectBody(t, m, markers)
		})
	}
}

func TestSessionApprovalContentIsReachableBelowLongFacts(t *testing.T) {
	d := fixtureData()
	s := &d.Sessions[0]
	for i := range 40 {
		s.Commands = append(s.Commands, Command{Args: fmt.Sprintf("command-%02d", i)})
	}
	s.Approvals = []Approval{{ID: "approval", Gist: "approval-below-facts", Detail: []string{"approval-input"}}}
	m := newTestModel(t, &fakeSource{data: d, turns: []Turn{{Text: "transcript-bottom"}}})
	t.Cleanup(m.cancel)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 12})
	m.setTab(sessionsTab)
	_, tail := m.enter()
	m.Update(tail())
	collectBody(t, m, []string{"command-00", "command-20", "command-39", "approval-below-facts", "approval-input", "transcript-bottom"})
	m.key(keyText("G"))
	if m.bodyBrowsing || m.tailBack != 0 {
		t.Fatal("G did not return to transcript following")
	}
}

func TestSelectedRowStaysVisibleWhenTrailingSectionsOverflow(t *testing.T) {
	d := fixtureData()
	d.Roles = d.Roles[:1]
	for i := range 40 {
		d.Agents = append(d.Agents, Agent{Name: fmt.Sprintf("agent-%02d", i)})
	}
	m := newTestModel(t, &fakeSource{data: d})
	t.Cleanup(m.cancel)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 12})
	m.setTab(3)
	if !strings.Contains(bodyView(m, 100, m.bodyH()), tSupervisor) {
		t.Fatal("trailing sections hid the selected role")
	}
	bodyKey(m, tea.KeyEnd)
	m.key(keyText("j"))
	if m.bodyBrowsing || !strings.Contains(bodyView(m, 100, m.bodyH()), tSupervisor) {
		t.Fatal("selection navigation did not reveal the selected role")
	}
}
