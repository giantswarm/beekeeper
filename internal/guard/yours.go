package guard

import (
	"slices"
	"strings"
)

// Yours returns the resources a message hands over with the supervisor's
// word: `yours <resource>`, `<resource> yours` or `<resource> is yours`,
// in the order said, each once, as resources spells them. Only a resource
// of resources counts, so "yours" in any other sense names nothing; the
// punctuation around the words (backticks, quotes, a comma or colon) is
// ignored.
func Yours(message string, resources []string) []string {
	var out []string
	add := func(w string) {
		if res, ok := resource(w, resources); ok && !slices.Contains(out, res) {
			out = append(out, res)
		}
	}
	words := strings.Fields(message)
	for i, w := range words {
		words[i] = strings.Trim(w, "`'\"*_,;:.!?()[]{}<>")
	}
	for i, w := range words {
		if !strings.EqualFold(w, "yours") {
			continue
		}
		if i+1 < len(words) {
			add(words[i+1])
		}
		if i >= 1 {
			add(words[i-1])
		}
		if i >= 2 && strings.EqualFold(words[i-1], "is") {
			add(words[i-2])
		}
	}
	return out
}

// resource is the resource w names, as resources spells it.
func resource(w string, resources []string) (string, bool) {
	i := slices.IndexFunc(resources, func(r string) bool { return strings.EqualFold(r, w) })
	if i < 0 {
		return "", false
	}
	return resources[i], true
}
