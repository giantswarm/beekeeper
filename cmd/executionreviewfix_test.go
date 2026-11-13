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
	const branch = "app-1"
	head := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name string
		r    state.ExecutionRecord
		want string
	}{
		{"unpinned branch", state.ExecutionRecord{Request: execution.StartRequest{GitRef: branch}}, branch},
		{"pinned to the branch's commit", state.ExecutionRecord{Request: execution.StartRequest{GitRef: head}, Published: &state.ExecutionPublication{Branch: branch, Head: head}}, branch},
		{"pinned without a branch", state.ExecutionRecord{Request: execution.StartRequest{GitRef: head}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := executionStartedBranch(&tc.r); got != tc.want {
				t.Fatalf("executionStartedBranch = %q, want %q", got, tc.want)
			}
		})
	}
}
