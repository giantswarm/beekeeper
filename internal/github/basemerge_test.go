package github

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const (
	bmRepo     = "giantswarm/beekeeper"
	bmApproved = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	bmHead     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	bmMain     = "cccccccccccccccccccccccccccccccccccccccc"
	modified   = "modified"
	statusKey  = "status"
)

// bmCommit is a commit of a comparison with its parents.
type bmCommit struct {
	SHA     string `json:"sha"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

func commitOf(sha string, parents ...string) bmCommit {
	c := bmCommit{SHA: sha}
	for _, p := range parents {
		c.Parents = append(c.Parents, struct {
			SHA string `json:"sha"`
		}{p})
	}
	return c
}

// commitsOf is a comparison status ahead by commits it lists.
func commitsOf(status string, by int, commits ...bmCommit) map[string]any {
	return map[string]any{statusKey: status, "ahead_by": by, "commits": commits}
}

// compareAnswers answers the compare API's calls by their from...to.
type compareAnswers map[string]any

func (c compareAnswers) gh(t *testing.T) GH {
	return func(_ context.Context, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		ref, ok := strings.CutPrefix(call, "api repos/"+bmRepo+"/compare/")
		a, known := c[ref]
		if !ok || !known {
			t.Fatalf("unexpected gh %s", call)
		}
		return json.Marshal(a)
	}
}

// approvedChange is the change the approved head makes against main: one
// line in a.go, b.go untouched since, and a binary c.png.
func approvedChange() []compareFile {
	return []compareFile{
		{Name: "a.go", Status: modified, SHA: "a1", Patch: "@@ -1,3 +1,3 @@\n ctx\n-old\n+new\n ctx"},
		{Name: "b.go", Status: "added", SHA: "b1", Patch: "@@ -0,0 +1 @@\n+b"},
		{Name: "c.png", Status: modified, SHA: "c1"},
	}
}

func TestBaseMergeOnly(t *testing.T) {
	merge := commitsOf(compareAhead, 1, commitOf(bmHead, bmApproved, bmMain))
	// main moved a.go's context and line numbers: the change is the same.
	moved := approvedChange()
	moved[0].SHA, moved[0].Patch = "a2", "@@ -10,3 +10,3 @@\n other ctx\n-old\n+new\n ctx"
	for name, c := range map[string]struct {
		added map[string]any
		now   []compareFile
		want  bool
		why   string
	}{
		"a merge of main alone":                                 {added: merge, now: moved, want: true, why: "only merges main"},
		"a merge of main and an unchanged blob without a patch": {added: merge, now: approvedChange(), want: true},
		"a commit of its own": {
			added: commitsOf(compareAhead, 2, commitOf(bmMain, bmApproved), commitOf(bmHead, bmMain, bmMain)),
			now:   moved, why: "which is no merge",
		},
		"a force push":             {added: commitsOf("diverged", 1, commitOf(bmHead, bmMain, bmMain)), now: moved, why: "diverged"},
		"more commits than listed": {added: commitsOf(compareAhead, 2, commitOf(bmHead, bmApproved, bmMain)), now: moved, why: "more than the comparison lists"},
		"a conflict resolved otherwise": {
			added: merge,
			now: func() []compareFile {
				f := approvedChange()
				f[0].SHA, f[0].Patch = "a3", "@@ -1,3 +1,3 @@\n ctx\n-old\n+newer\n ctx"
				return f
			}(),
			why: "a.go adds or removes other lines",
		},
		"a change the merge carries": {added: merge, now: append(approvedChange(), compareFile{Name: "d.go", Status: "added", SHA: "d1", Patch: "+d"}), why: "4 changed files, not 3"},
		"a binary the merge changed": {
			added: merge,
			now: func() []compareFile {
				f := approvedChange()
				f[2].SHA = "c2"
				return f
			}(),
			why: "c.png has no patch to compare",
		},
	} {
		t.Run(name, func(t *testing.T) {
			gh := compareAnswers{
				bmApproved + "..." + bmHead: c.added,
				"main..." + bmApproved:      map[string]any{statusKey: compareAhead, "files": approvedChange()},
				"main..." + bmHead:          map[string]any{statusKey: compareAhead, "files": c.now},
			}.gh(t)
			got, why, err := BaseMergeOnly(context.Background(), gh, bmRepo, "main", bmApproved, bmHead)
			if err != nil || got != c.want || !strings.Contains(why, c.why) {
				t.Errorf("BaseMergeOnly = %v, %q, %v; want %v with %q", got, why, err, c.want, c.why)
			}
		})
	}
}

func TestSameChangeAtTheCapProvesNothing(t *testing.T) {
	files := make([]compareFile, compareFiles)
	for i := range files {
		files[i] = compareFile{Name: string(rune('a'+i%26)) + strings.Repeat("x", i), Status: modified, SHA: "s"}
	}
	if why := sameChange(files, files); !strings.Contains(why, "cap") {
		t.Errorf("sameChange at the cap = %q", why)
	}
}
