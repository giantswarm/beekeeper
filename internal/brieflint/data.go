package brieflint

import "strings"

const dataLanguage = "beekeeper-data"

// QuoteData preserves externally supplied records without treating their prose
// as instructions to lint or issue assignments. Choose a fence that the data
// cannot close, including messages which themselves contain Markdown fences.
func QuoteData(text string) string {
	mark := "```"
	for strings.Contains(text, mark) {
		mark += "`"
	}
	return mark + dataLanguage + "\n" + text + "\n" + mark
}

// WithoutData masks quoted record blocks, preserving line numbers for lint.
// Ordinary code blocks retain their existing lint and reference semantics.
func WithoutData(text string) string {
	lines := strings.Split(text, "\n")
	mark := ""
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if mark == "" {
			prefix := strings.TrimSuffix(trimmed, dataLanguage)
			if prefix != trimmed && len(prefix) >= 3 && strings.Trim(prefix, "`") == "" {
				mark = prefix
				lines[i] = ""
			}
			continue
		}
		lines[i] = ""
		if trimmed == mark {
			mark = ""
		}
	}
	return strings.Join(lines, "\n")
}
