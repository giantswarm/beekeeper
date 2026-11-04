package cmd

import (
	"fmt"
	"strings"
	"testing"
)

// These are prompt contract tests: the Chrome tools execute in the child
// agent, so a fake tool transcript would not prove the agent obeyed the rule.
// Cover ordinary, declared deploy, blind capture and reconnect turns so none
// can instruct a retry without first reconciling a possibly completed click.
func TestBrowseClickSettleInstructions(t *testing.T) {
	const steps = "Click Rerun workflow from failed. The new run appears after 6 seconds. Report the run."
	for name, prompt := range map[string]string{
		"browse":    browsePrompt(steps, "", nil, false, nil),
		"deploy":    browsePrompt(steps, "the requested deploy", nil, false, nil),
		"blind":     blindPrompt(steps, "", nil, false, ""),
		"reconnect": fmt.Sprintf(reconnectWords, "the state-changing click"),
	} {
		t.Run(name, func(t *testing.T) {
			for _, rule := range []string{
				"note the current URL", "then click once",
				"Do not batch repeated clicks or use a scripted retry loop",
				"intervals of 2 seconds", "after each interval, for up to 30 seconds",
				"expected result appears", "URL changes", "relevant status changes",
				"Before any repeated state-changing click, re-read the page",
				"If the result exists or the action is pending, do not click again",
				"30-second timeout has passed AND that fresh read shows no effect and no pending action",
				"If the outcome is uncertain, stop", "After a connection loss",
				"In a blind capture turn, use find",
			} {
				if !strings.Contains(prompt, rule) {
					t.Errorf("missing settling rule %q", rule)
				}
			}
		})
	}
}

func TestBrowseClickSettleHelp(t *testing.T) {
	help := (&app{}).browseCmd().Long
	for _, rule := range []string{"2-second intervals", "up to 30 seconds", "pending action", "uncertain outcome"} {
		if !strings.Contains(help, rule) {
			t.Errorf("browse help lacks %q", rule)
		}
	}
}
