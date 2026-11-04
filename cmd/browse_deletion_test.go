package cmd

import (
	"context"
	"strings"
	"testing"
)

func TestBrowsePermanentDeletionPreflight(t *testing.T) {
	for _, steps := range []string{
		"On my issue click Delete revision; I own it and ask for this.",
		"Delete the edit-history revision.",
		"Delete my comment.",
		"Remove the issue.",
		"Click Delete repository.",
		"Delete the branch.",
		"Empty the trash.",
		"Empty the recycle bin.",
		"Permanently delete the data.",
	} {
		t.Run(steps, func(t *testing.T) {
			// An uninitialized app cannot inspect desktop configuration, read state
			// or launch Chrome: this must return before any of those operations.
			a := &app{}
			err := a.browse(context.Background(), steps, "", "", 0, "", nil, nil)
			if err == nil || !strings.Contains(err.Error(), permanentDeletionRule) {
				t.Fatalf("browse = %v, want permanent deletion rule", err)
			}
		})
	}
}

func TestBrowsePermanentDeletionScope(t *testing.T) {
	for _, steps := range []string{
		"Delete the demo product in the portal.",
		"Open the issue and read its edit history.",
		"Close the issue.",
		"Delete the draft text from the input.",
	} {
		if permanentDeletionSteps.MatchString(steps) {
			t.Errorf("preflight refused %q", steps)
		}
	}
}

func TestBrowsePermanentDeletionHelp(t *testing.T) {
	a := &app{}
	if !strings.Contains(a.browseCmd().Long, permanentDeletionRule) {
		t.Fatal("browse help omits the permanent deletion rule")
	}
}
