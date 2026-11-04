package tui

import (
	"errors"
	"strings"
	"testing"
)

func TestCellFormattingKeepsLineBreaksInOneRow(t *testing.T) {
	for name, format := range map[string]func(string, int) string{
		"fit": fit, "fitLeft": fitLeft, "trimWord": trimWord,
	} {
		t.Run(name, func(t *testing.T) {
			const input = "first\nsecond\r\nthird\rfourth"
			if got := format(input, 100); got != "first ↵ second ↵ third ↵ fourth" {
				t.Fatalf("format = %q, want one row with visible line breaks", got)
			}
			if got := format(input, 12); strings.ContainsAny(got, "\r\n") || ansiWidth(got) > 12 {
				t.Fatalf("truncated cell overflowed its row: %q", got)
			}
		})
	}
}

func TestMultilineNoteAndRefreshErrorKeepWindowHeight(t *testing.T) {
	d := fixtureData()
	d.Notes[0].Text = strings.Repeat("note\n", 30)
	m := newTestModel(t, &fakeSource{data: d})
	t.Cleanup(m.cancel)
	m.setTab(3)
	m.err = errors.New("first failure\nsecond failure\r\nthird failure")
	if footer := footerView(m, 100); strings.ContainsAny(footer, "\r\n") {
		t.Fatalf("the refresh error added footer rows: %q", footer)
	}
	if screen := render(m, 100, 30); strings.Count(screen, "\n")+1 > 30 {
		t.Fatalf("multiline data overflowed the window: %d lines", strings.Count(screen, "\n")+1)
	}
	if d.Notes[0].Text != strings.Repeat("note\n", 30) {
		t.Fatal("rendering changed the source note")
	}
}
