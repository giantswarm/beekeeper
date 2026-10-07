package github

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The Auto-release workflow devctl generates for a repository's main line
// and for a fork line, whose consumed branch alone releases.
const (
	mainLineWorkflow = `name: Auto-release
on:
  push:
    branches:
      - main
      - 'release-[0-9]+.x'
      - 'release-v[0-9]+.[0-9]+.x'
  workflow_dispatch: {}
`
	forkLineWorkflow = `name: Auto-release
on:
  push:
    branches:
      - giantswarm
  workflow_dispatch: {}
`
)

const (
	mainBranch = "main"
	handCut    = "release-1.3"
	anyBranch  = "anything"
)

func TestPushesBranch(t *testing.T) {
	for _, c := range []struct {
		workflow, branch string
		want             bool
	}{
		{mainLineWorkflow, mainBranch, true},
		{mainLineWorkflow, "release-2.x", true},
		{mainLineWorkflow, "release-v3.7.x", true},
		{mainLineWorkflow, "release-v3.x-fix", false},
		{mainLineWorkflow, handCut, false},
		{forkLineWorkflow, "giantswarm", true},
		{forkLineWorkflow, handCut, false},
		{"on: push\n", anyBranch, true},
		{"on: [pull_request, push]\n", anyBranch, true},
		{"on: [pull_request]\n", mainBranch, false},
		{"on:\n  push:\n", anyBranch, true},
		{"on:\n  push:\n    tags: ['v*']\n", mainBranch, false},
		{"on:\n  push:\n    branches-ignore: ['release-**']\n", handCut, false},
		{"on:\n  push:\n    branches-ignore: ['release-**']\n", mainBranch, true},
		{"on:\n  push:\n    branches: ['release-*', '!release-1.3']\n", handCut, false},
		{"on:\n  push:\n    branches: ['release-*', '!release-1.3']\n", "release-1.4", true},
		{"on:\n  push:\n    branches: ['feature/*']\n", "feature/a/b", false},
		{"on:\n  push:\n    branches: ['feature/**']\n", "feature/a/b", true},
	} {
		got, err := PushesBranch([]byte(c.workflow), c.branch)
		if err != nil || got != c.want {
			t.Errorf("%q on %q: %v, %v; want %v", c.branch, c.workflow, got, err, c.want)
		}
	}
}

func TestReadBaseRelease(t *testing.T) {
	for _, c := range []struct {
		name, base string
		workflow   string
		wfErr      error
		want       BaseRelease
		wantErr    bool
	}{
		{"main with auto-release", mainBranch, mainLineWorkflow, nil, BaseRelease{Base: mainBranch, Auto: true}, false},
		{"a fork line's maintenance branch", handCut, forkLineWorkflow, nil, BaseRelease{Base: handCut}, false},
		{"a branch without the workflow", handCut, "", errors.New("gh api: exit status 1: gh: Not Found (HTTP 404)"), BaseRelease{Base: handCut}, false},
		{"GitHub unanswered", mainBranch, "", errors.New("gh api: exit status 1: connection reset"), BaseRelease{}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var asked []string
			run := func(_ context.Context, args ...string) ([]byte, error) {
				call := strings.Join(args, " ")
				asked = append(asked, call)
				if strings.HasPrefix(call, "pr view 7 --repo o/r") {
					return []byte(`{"baseRefName":"` + c.base + `"}`), nil
				}
				return []byte(c.workflow), c.wfErr
			}
			got, err := ReadBaseRelease(context.Background(), run, "o/r", 7)
			if (err != nil) != c.wantErr || got != c.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, c.want)
			}
			if len(asked) != 2 || !strings.HasSuffix(asked[1], AutoReleaseWorkflow+"?ref="+c.base) {
				t.Errorf("asked %q", asked)
			}
		})
	}
}
