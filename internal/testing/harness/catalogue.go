package harness

import (
	"fmt"
	"sort"
	"strings"
)

// Catalogue renders CATALOGUE.md (contract §6): every scenario by concept, with its tags and its
// Given/When/Then.
func Catalogue(scenarios []*Scenario) string {
	var b strings.Builder
	b.WriteString("# Phase 1 scenario catalogue\n\nGenerated from the scenario files; don't edit by hand. ")
	fmt.Fprintf(&b, "%d scenarios. The format and vocabulary are in `docs/phase1/scenarios.md`.\n", len(scenarios))
	b.WriteString("\n| Concept | Scenarios |\n|---|---|\n")
	counts := map[string]int{}
	for _, s := range scenarios {
		counts[s.Concept]++
	}
	for _, c := range Concepts {
		if counts[c] > 0 {
			fmt.Fprintf(&b, "| [`%s`](#%s) | %d |\n", c, c, counts[c])
		}
	}
	for _, c := range Concepts {
		var es []*Scenario
		for _, s := range scenarios {
			if s.Concept == c {
				es = append(es, s)
			}
		}
		if len(es) == 0 {
			continue
		}
		sort.Slice(es, func(i, j int) bool { return es[i].Name < es[j].Name })
		fmt.Fprintf(&b, "\n## %s\n\n", c)
		for _, s := range es {
			var tags []string
			for _, t := range s.Tags {
				if t != "phase1" {
					tags = append(tags, "`"+t+"`")
				}
			}
			fmt.Fprintf(&b, "- **[%s](%s/%s.yaml)**", s.Name, s.Concept, s.Name)
			if len(tags) > 0 {
				b.WriteString(" " + strings.Join(tags, " "))
			}
			// A backslash hard break: trailing spaces would not survive the trailing-whitespace hook.
			b.WriteString("\\\n")
			wrap(&b, "  ", strings.Fields(s.Description), catalogueWidth)
		}
	}
	return b.String()
}

// catalogueWidth keeps the catalogue's lines within the repository's Markdown line limit.
const catalogueWidth = 100

// wrap writes words as lines of at most width bytes, each starting with indent (a longer word gets a
// line of its own). It never starts a line with a word Markdown would read as a new block there.
func wrap(b *strings.Builder, indent string, words []string, width int) {
	line := indent
	for _, w := range words {
		if len(line) > len(indent) && len(line)+1+len(w) > width && !startsBlock(w) {
			b.WriteString(line + "\n")
			line = indent
		}
		if len(line) > len(indent) {
			line += " "
		}
		line += w
	}
	b.WriteString(line + "\n")
}

// startsBlock reports whether a line starting with w could open a list, quote, heading, fence, table
// or setext underline instead of continuing the paragraph.
func startsBlock(w string) bool {
	if strings.ContainsAny(w[:1], "-*+=>#`~|") {
		return true
	}
	digits := strings.TrimLeft(w, "0123456789")
	return len(digits) < len(w) && (digits == "." || digits == ")")
}
