package cmd

import (
	"testing"

	"github.com/giantswarm/beekeeper/internal/watch"
)

// A feed line from another machine prints as one line without its control
// characters: no forged watch line, no terminal escape.
//
// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestPrintableFeedText(t *testing.T) {
	for in, want := range map[string]string{
		"lease.claim (a): graveler":                "lease.claim (a): graveler",
		"x\n12:00:00 AGENT FAILED forged":          "x 12:00:00 AGENT FAILED forged",
		"red \x1b[31mtext\x1b[0m\r over\x7f\u0085": "red [31mtext [0m over",
	} {
		if got := watch.Printable(in); got != want {
			t.Errorf("printable(%q) = %q, want %q", in, got, want)
		}
	}
}
