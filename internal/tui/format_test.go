package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestFittedCellsStayOnOneLine(t *testing.T) {
	for name, fitCell := range map[string]func(string, int) string{"fit": fit, "fitLeft": fitLeft, "trimWord": trimWord} {
		t.Run(name, func(t *testing.T) {
			for _, sep := range []string{"\n", "\r\n", "\r"} {
				input := "\x1b[31m界" + sep + "two" + sep + "three\x1b[0m"
				for _, width := range []int{-1, 0, 1, 5, 80} {
					got := fitCell(input, width)
					if strings.ContainsAny(got, "\r\n") || ansi.StringWidth(got) > max(0, width) {
						t.Errorf("width %d: invalid cell %q", width, got)
					}
					if width == 80 && ansi.Strip(got) != "界 two three" {
						t.Errorf("line separators were not flattened: %q", got)
					}
				}
			}
		})
	}
}
