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

// pluginWorkflow is a plugin marketplace's Auto Release: on a merged pull
// request into main it tags each plugin whose version changed.
const pluginWorkflow = `name: Auto Release
on:
  pull_request:
    types: [closed]
    branches:
      - main
jobs:
  auto-release:
    steps:
      - run: |
          CURRENT=$(cat .claude-plugin/marketplace.json)
          tag="${plugin}/v${current_version}"
`

// errNotFound is gh's answer for a file a branch lacks.
var errNotFound = errors.New("gh api: exit status 1: gh: Not Found (HTTP 404)")

func TestReadBaseRelease(t *testing.T) {
	for _, c := range []struct {
		name, base string
		// workflow, wfErr answer the generated Auto-release workflow; plugin,
		// plErr the plugin marketplace's.
		workflow, plugin string
		wfErr, plErr     error
		want             BaseRelease
		wantErr          bool
		asks             int
	}{
		{"main with auto-release", mainBranch, mainLineWorkflow, "", nil, nil, BaseRelease{Base: mainBranch, Auto: true}, false, 2},
		{"a fork line's maintenance branch", handCut, forkLineWorkflow, "", nil, nil, BaseRelease{Base: handCut}, false, 2},
		{"a branch without any workflow", handCut, "", "", errNotFound, errNotFound, BaseRelease{Base: handCut}, false, 4},
		{"a plugin marketplace's main", mainBranch, "", pluginWorkflow, errNotFound, nil, BaseRelease{Base: mainBranch, Plugins: true}, false, 3},
		{"a plugin marketplace's other branch", handCut, "", pluginWorkflow, errNotFound, nil, BaseRelease{Base: handCut}, false, 4},
		{"an auto-release.yaml of another kind", mainBranch, "", "on:\n  pull_request:\n    types: [closed]\n", errNotFound, nil, BaseRelease{Base: mainBranch}, false, 4},
		{"GitHub unanswered", mainBranch, "", "", errors.New("gh api: exit status 1: connection reset"), nil, BaseRelease{}, true, 2},
		{"GitHub unanswered for the plugin workflow", mainBranch, "", "", errNotFound, errors.New("connection reset"), BaseRelease{}, true, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			var asked []string
			run := func(_ context.Context, args ...string) ([]byte, error) {
				call := strings.Join(args, " ")
				asked = append(asked, call)
				switch {
				case strings.HasPrefix(call, "api repos/o/r/pulls/7 --jq"):
					return []byte(`{"base":"` + c.base + `"}`), nil
				case strings.HasSuffix(call, PluginReleaseWorkflow+"?ref="+c.base):
					return []byte(c.plugin), c.plErr
				}
				return []byte(c.workflow), c.wfErr
			}
			got, err := ReadBaseRelease(context.Background(), run, "o/r", 7)
			if (err != nil) != c.wantErr || got != c.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, c.want)
			}
			if len(asked) != c.asks || !strings.HasSuffix(asked[1], AutoReleaseWorkflow+"?ref="+c.base) {
				t.Errorf("asked %q", asked)
			}
		})
	}
}

// createReleaseWorkflow is the release-PR flow's tagging workflow as devctl
// generates it.
const createReleaseWorkflow = `name: Create Release
on:
  push:
    branches:
      - 'legacy'
      - 'main'
      - 'master'
      - 'release-v*.*.x'
      - '!release-v*.x.x'
`

func TestReadBaseReleaseOfTheReleasePRFlow(t *testing.T) {
	const releaseHead = "main#release#patch"
	for _, c := range []struct {
		name, base, head, title string
		// create, createErr answer CreateReleaseWorkflow; prErr
		// CreateReleasePRWorkflow.
		create           string
		createErr, prErr error
		want             BaseRelease
		wantErr          bool
		asks             int
	}{
		{"the release pull request", mainBranch, releaseHead, "chore(release): v1.4.0", createReleaseWorkflow, nil, nil,
			BaseRelease{Base: mainBranch, ReleasePR: true, Version: "v1.4.0"}, false, 5},
		{"an older release pull request", mainBranch, releaseHead, "Release v1.4.0", createReleaseWorkflow, nil, nil,
			BaseRelease{Base: mainBranch, ReleasePR: true, Version: "v1.4.0"}, false, 5},
		{"another pull request", mainBranch, "fix-thing", "fix: the thing", createReleaseWorkflow, nil, nil,
			BaseRelease{Base: mainBranch, ReleasePR: true}, false, 5},
		{"a branch the tagging workflow does not push-trigger", "feature", "feature-fix", "fix", createReleaseWorkflow, nil, nil,
			BaseRelease{Base: "feature"}, false, 4},
		{"the tagging workflow without the release-PR one", mainBranch, releaseHead, "chore(release): v1.4.0", createReleaseWorkflow, nil, errNotFound,
			BaseRelease{Base: mainBranch}, false, 5},
		{"GitHub unanswered for the tagging workflow", mainBranch, releaseHead, "x", "", errors.New("connection reset"), nil,
			BaseRelease{}, true, 4},
	} {
		t.Run(c.name, func(t *testing.T) {
			asks := 0
			run := func(_ context.Context, args ...string) ([]byte, error) {
				asks++
				call := strings.Join(args, " ")
				switch {
				case strings.HasPrefix(call, "api repos/o/r/pulls/7 --jq"):
					return []byte(`{"base":"` + c.base + `","head":"` + c.head + `","title":"` + c.title + `"}`), nil
				case strings.Contains(call, CreateReleaseWorkflow+"?ref="):
					return []byte(c.create), c.createErr
				case strings.Contains(call, CreateReleasePRWorkflow+"?ref="):
					return []byte("name: Create Release PR\n"), c.prErr
				}
				return nil, errNotFound
			}
			got, err := ReadBaseRelease(context.Background(), run, "o/r", 7)
			if (err != nil) != c.wantErr || got != c.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, c.want)
			}
			if asks != c.asks {
				t.Errorf("%d requests, want %d", asks, c.asks)
			}
		})
	}
}

func TestReleaseVersion(t *testing.T) {
	for _, c := range []struct{ head, title, want string }{
		{"main#release#patch", "chore(release): v1.2.3", "v1.2.3"},
		{"main#release#minor-rc", "chore(release): v1.3.0-rc.1", "v1.3.0-rc.1"},
		{"release#minor", "Release v1.3.0", "v1.3.0"},
		{"main#release#patch", "chore(release): bump things", ""},
		{"fix-release-notes", "chore(release): v1.2.3", ""},
	} {
		if got := ReleaseVersion(c.head, c.title); got != c.want {
			t.Errorf("ReleaseVersion(%q, %q) = %q, want %q", c.head, c.title, got, c.want)
		}
	}
}

func TestPluginTags(t *testing.T) {
	const (
		was = `{"plugins":[{"name":"gs-plans","version":"0.2.0"},{"name":"gs-base","version":"1.4.0"}]}`
		now = `{"plugins":[{"name":"gs-plans","version":"0.3.0"},{"name":"gs-base","version":"1.4.0"},{"name":"gs-new","version":"0.1.0"}]}`
	)
	for _, c := range []struct {
		name, parent, atParent, atMerge string
		want                            []string
	}{
		{"one plugin bumped, one new", "p1", was, now, []string{"gs-plans/v0.3.0", "gs-new/v0.1.0"}},
		{"no version changed", "p1", was, was, nil},
		{"the manifest new in the merge", "p1", "", was, []string{"gs-plans/v0.2.0", "gs-base/v1.4.0"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := func(_ context.Context, args ...string) ([]byte, error) {
				call := strings.Join(args, " ")
				switch {
				case call == "api repos/o/r/commits/m1 --jq .parents[0].sha":
					return []byte(c.parent + "\n"), nil
				case strings.HasSuffix(call, Marketplace+"?ref=m1"):
					return []byte(c.atMerge), nil
				case strings.HasSuffix(call, Marketplace+"?ref="+c.parent) && c.atParent == "":
					return nil, errNotFound
				case strings.HasSuffix(call, Marketplace+"?ref="+c.parent):
					return []byte(c.atParent), nil
				}
				t.Fatalf("asked %q", call)
				return nil, nil
			}
			got, err := PluginTags(context.Background(), run, "o/r", "m1")
			if err != nil || strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("got %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

func TestTagExists(t *testing.T) {
	for _, c := range []struct {
		err     error
		want    bool
		wantErr bool
	}{
		{nil, true, false},
		{errNotFound, false, false},
		{errors.New("connection reset"), false, true},
	} {
		run := func(_ context.Context, args ...string) ([]byte, error) {
			if got := strings.Join(args, " "); got != "api repos/o/r/git/ref/tags/gs-plans/v0.3.0" {
				t.Errorf("asked %q", got)
			}
			return nil, c.err
		}
		got, err := TagExists(context.Background(), run, "o/r", "gs-plans/v0.3.0")
		if got != c.want || (err != nil) != c.wantErr {
			t.Errorf("%v: got %v, %v", c.err, got, err)
		}
	}
}
