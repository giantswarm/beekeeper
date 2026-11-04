package tui

import (
	tea "charm.land/bubbletea/v2"
	"strings"
	"testing"
)

func TestMultilinePasteUsesOneDisplayRowAndPreservesDelivery(t *testing.T) {
	src := &fakeSource{data: fixtureData(), turns: []Turn{{Text: "working"}}}
	m := newTestModel(t, src)
	t.Cleanup(m.cancel)
	m.setTab(sessionsTab)
	_, tail := m.enter()
	m.Update(tail())
	m.key(keyText("m"))
	draft := "first\nsecond\r\nthird\rfourth" + strings.Repeat("\nx", 40)
	m.Update(tea.PasteMsg{Content: draft})
	if string(m.draft) != draft {
		t.Fatalf("the pasted draft changed: %q", string(m.draft))
	}
	line := messageLine(m.pane(), 100)
	for _, r := range line {
		if r == '\n' || r == '\r' {
			t.Fatalf("the single message row contains a line break: %q", line)
		}
	}
	lines := 1
	for _, r := range render(m, 100, 30) {
		if r == '\n' {
			lines++
		}
	}
	if lines > 30 {
		t.Fatalf("the pasted draft overflowed the window: %d lines", lines)
	}
	_, send := m.key(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.Update(send())
	if src.sendText != draft {
		t.Fatalf("the display conversion changed the delivered text: %q", src.sendText)
	}
}
