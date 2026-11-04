package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestOverflowingSectionsReachable(t *testing.T) {
	for _, section := range []string{"alerts", "agents", "notes", "holds"} {
		for _, key := range []string{"j", "down", "pgdown"} {
			t.Run(section+"/"+key, func(t *testing.T) {
				d := fixtureData()
				tab := 3
				d.Roles = d.Roles[:1]
				d.Alerts = []AlertsFor{{Installation: "test", Reachable: true}}
				d.Leases.Held = nil
				var wants []string
				for i := range 40 {
					label := fmt.Sprintf("entry%02d", i)
					wants = append(wants, label)
					switch section {
					case "alerts":
						tab = 4
						d.Alerts[0].Alerts = append(d.Alerts[0].Alerts, Alert{Alertname: label})
					case "agents":
						d.Agents = append(d.Agents, Agent{Name: label})
					case "notes":
						d.Notes = append(d.Notes, Note{Text: label})
					case "holds":
						tab = 2
						d.Holds = append(d.Holds, Hold{Target: label})
					}
				}
				m := newTestModel(t, &fakeSource{data: d})
				m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
				m.setTab(tab)
				seen := ""
				for range 100 {
					view := m.screen()
					if len(strings.Split(view, "\n")) > 24 {
						t.Fatal("view exceeds terminal height")
					}
					seen += view
					m.key(keyText(key))
				}
				for _, want := range wants {
					if !strings.Contains(seen, want) {
						t.Errorf("cannot reach %s", want)
					}
				}
				m.key(keyText("G"))
				if !strings.Contains(m.screen(), wants[39]) {
					t.Error("end misses last entry")
				}
				m.key(keyText("g"))
				if m.scroll[tab].top != 0 {
					t.Error("home did not return to top")
				}
				m.key(keyText("G"))
				reverse := ""
				up := "k"
				if key == "pgdown" {
					up = "pgup"
				} else if key == "down" {
					up = "up"
				}
				for range 100 {
					reverse += m.screen()
					m.key(keyText(up))
				}
				for _, want := range wants {
					if !strings.Contains(reverse, want) {
						t.Errorf("cannot reach %s scrolling back", want)
					}
				}
				// A refresh and resize must clamp an offset left over from longer content.
				m.key(keyText("G"))
				m.Update(refreshMsg{data: &Data{}})
				m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
				_ = m.screen()
				if m.scroll[tab].top != 0 {
					t.Error("offset not clamped after content shrank")
				}
			})
		}
	}
}

func TestOverflowingSessionFactsReachApproval(t *testing.T) {
	d := fixtureData()
	s := &d.Sessions[0]
	s.Commands = nil
	for i := range 40 {
		s.Commands = append(s.Commands, Command{Args: fmt.Sprintf("command%02d", i)})
	}
	s.TakenOver = true
	s.Approvals = []Approval{{Gist: "approval-marker"}}
	m := newTestModel(t, &fakeSource{data: d})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.detail = s.Name
	for _, key := range []string{"j", "pgdown", "G"} {
		m.facts = viewport{}
		seen := ""
		for range 80 {
			view := m.screen()
			if len(strings.Split(view, "\n")) > 24 {
				t.Fatal("detail exceeds terminal height")
			}
			seen += view
			m.key(keyText(key))
		}
		if !strings.Contains(seen, "approval-marker") {
			t.Errorf("%s cannot reach approval", key)
		}
		m.composing = true
		if !strings.Contains(m.screen(), "message ›") {
			t.Error("message line hidden")
		}
		m.composing = false
		m.key(keyText("g"))
		if !strings.Contains(m.screen(), "context") {
			t.Error("home cannot reach session facts")
		}
	}
}

func TestOverflowingFactsPreserveTranscriptNavigation(t *testing.T) {
	d := fixtureData()
	for range 40 {
		d.Sessions[0].Commands = append(d.Sessions[0].Commands, Command{Args: "work"})
	}
	m := newTestModel(t, &fakeSource{data: d})
	m.detail = d.Sessions[0].Name
	m.tail = []Turn{{Text: "older"}, {Text: "newer"}}
	m.tailState = 2
	m.key(keyText("k"))
	if m.tailBack != 1 {
		t.Fatal("cannot reach older transcript at facts start")
	}
	m.key(keyText("G"))
	if m.tailBack != 0 || m.facts.top != m.facts.limit {
		t.Fatal("end did not reach live transcript and last facts")
	}
	m.key(keyText("g"))
	if m.tailBack != 1 || m.facts.top != 0 {
		t.Fatal("home did not reach oldest transcript and first facts")
	}
}
