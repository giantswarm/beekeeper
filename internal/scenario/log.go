//go:build scenario

package scenario

import (
	"fmt"
	"regexp"
	"slices"
)

// LogExpect checks what the background step Name has written so far, its
// standard output and error as one text: the lines a running loop or watch
// says (SUPERVISOR STARTS HELD, HIGH LOAD), which no event records.
type LogExpect struct {
	Name        string   `yaml:"name"`
	Contains    []string `yaml:"contains"`
	NotContains []string `yaml:"not_contains"`
	Matches     string   `yaml:"matches"`
}

// validate is what the loader refuses of a log expectation.
func (l *LogExpect) validate(background []string) []string {
	var out []string
	if !slices.Contains(background, l.Name) {
		out = append(out, fmt.Sprintf("log %q names no earlier background step", l.Name))
	}
	if len(l.Contains)+len(l.NotContains) == 0 && l.Matches == "" {
		out = append(out, "log without contains, not_contains or matches")
	}
	if l.Matches != "" {
		if _, err := regexp.Compile(l.Matches); err != nil {
			out = append(out, fmt.Sprintf("log: matches: %v", err))
		}
	}
	return out
}

// check checks the expectation against what read returns for the step.
func (l *LogExpect) check(read func(name string) (string, error)) error {
	got, err := read(l.Name)
	if err != nil {
		return fmt.Errorf("log of %s: %w", l.Name, err)
	}
	return checkText("log of "+l.Name, TextExpect{Contains: l.Contains, NotContains: l.NotContains, Matches: l.Matches}, got)
}
