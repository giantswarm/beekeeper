package cmd

import (
	"strings"
	"testing"
)

// Chrome tools execute in the delegated turn, so its prompt must specify
// both outcomes and the retry boundary before the existing fresh-tab rule.
func TestBrowseSitePermissionRetry(t *testing.T) {
	prompt := browsePrompt("Take a screenshot", "", nil, false, nil)
	for name, want := range map[string]string{
		"specific refusal":             "When a Chrome tool answers 'Permission denied for this action on this domain'",
		"delay":                        "wait 1 second with the Chrome computer wait action",
		"one identical retry":          "retry that same tool call with identical arguments once",
		"success":                      "If the retry succeeds, continue the steps and note the site-permission retry in your report",
		"persistent refusal":           "If the retry is refused too, stop that step and report it as refused with the refusal's reason",
		"no further attempts":          "do not retry it again or work around it in another tab",
		"classifier and hook boundary": "never to a Claude Code auto mode classifier denial or a beekeeper hook refusal",
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(prompt, want) {
				t.Errorf("browse prompt missing %q", want)
			}
		})
	}
	if strings.Count(prompt, sitePermissionWords) != 1 {
		t.Fatal("browse prompt must contain the site-permission rule exactly once")
	}
	if strings.Index(prompt, sitePermissionWords) > strings.Index(prompt, frontWords) {
		t.Fatal("site-permission retry must precede the fresh-tab fallback")
	}
}
