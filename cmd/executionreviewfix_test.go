package cmd

import (
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A reviewer's execution is started on the pull request's branch; once the
// coordinator pins its source, the request names the commit and the pin
// records the branch. Either way the grant compares the branch.
func TestExecutionStartedBranch(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name string
		r    state.ExecutionRecord
		want string
	}{
		{"unpinned branch", state.ExecutionRecord{Request: execution.StartRequest{GitRef: "app-1"}}, "app-1"},
		{"pinned to the branch's commit", state.ExecutionRecord{Request: execution.StartRequest{GitRef: head}, Published: &state.ExecutionPublication{Branch: "app-1", Head: head}}, "app-1"},
		{"pinned without a branch", state.ExecutionRecord{Request: execution.StartRequest{GitRef: head}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := executionStartedBranch(&tc.r); got != tc.want {
				t.Fatalf("executionStartedBranch = %q, want %q", got, tc.want)
			}
		})
	}
}
