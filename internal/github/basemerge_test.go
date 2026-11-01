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
	bmFork     = "dddddddddddddddddddddddddddddddddddddddd"
	modified   = "modified"
	statusKey  = "status"
	png        = "c.png"
	otherLines = "a.go adds or removes other lines"
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

// filesOf is a comparison from mergeBase listing files.
func filesOf(mergeBase string, files []compareFile) map[string]any {
	return map[string]any{statusKey: compareAhead, "merge_base_commit": map[string]string{"sha": mergeBase}, "files": files}
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
		{Name: png, Status: modified, SHA: "c1"},
	}
}

func TestApprovalCovers(t *testing.T) {
	merge := commitsOf(compareAhead, 1, commitOf(bmHead, bmApproved, bmMain))
	// main moved a.go's context and line numbers: the change is the same.
	moved := approvedChange()
	moved[0].SHA, moved[0].Patch = "a2", "@@ -10,3 +10,3 @@\n other ctx\n-old\n+new\n ctx"
	// main changed a.go's first line to "newer" and line 50 from x to y;
	// resolved is a.go after the conflict on its first line, the approved
	// "new" kept against main's "newer".
	mainChangedA := []compareFile{{Name: "a.go", Status: modified, SHA: "a9", Patch: "@@ -1,3 +1,3 @@\n ctx\n-old\n+newer\n ctx\n@@ -50,3 +50,3 @@\n ctx\n-x\n+y\n ctx"}}
	resolved := func() []compareFile {
		f := approvedChange()
		f[0].SHA, f[0].Patch = "a3", "@@ -1,3 +1,3 @@\n ctx\n-newer\n+new\n ctx"
		return f
	}
	// unreviewed is a.go with a line of neither side, added by the merge.
	unreviewed := func() []compareFile {
		f := approvedChange()
		f[0].SHA, f[0].Patch = "a5", "@@ -1,3 +1,3 @@\n ctx\n-old\n+new\n ctx\n@@ -200,0 +200,1 @@\n+func unreviewed() { os.RemoveAll(\"/\") }"
		return f
	}
	for name, c := range map[string]struct {
		added map[string]any
		now   []compareFile
		// base is what main changed between the approved head's merge
		// base (bmFork) and the head's (bmMain); nil: z.go alone.
		base []compareFile
		// holds is whether main holds the merged parent bmMain.
		holds bool
		want  bool
		why   string
	}{
		"a merge of main alone":                                 {added: merge, now: moved, want: true, why: "only merges main"},
		"a merge of main and an unchanged blob without a patch": {added: merge, now: approvedChange(), want: true},
		"a merge resolving a conflict with main's change": {
			added: merge, now: resolved(), base: mainChangedA, holds: true,
			want: true, why: "resolving its conflicts in a.go",
		},
		// The worker kept the approved a.go (git checkout --ours): a
		// conflict resolved its way.
		"a merge that keeps its side of a conflict": {
			added: merge,
			now: func() []compareFile {
				f := approvedChange()
				f[0].Patch = "@@ -1,3 +1,3 @@\n ctx\n-old\n+new\n ctx\n@@ -50,3 +50,3 @@\n ctx\n-y\n+x\n ctx"
				return f
			}(),
			base: mainChangedA, holds: true, want: true, why: "resolving its conflicts in a.go",
		},
		"a resolved conflict of a merge of another branch": {
			added: merge, now: resolved(), base: mainChangedA,
			why: "which merges no commit of main",
		},
		"a binary main changed, kept at the approved blob": {
			added: merge, now: approvedChange(),
			base: []compareFile{{Name: png, Status: modified, SHA: "c9"}},
			why:  "c.png has no patch, and the base changed it",
		},
		"a binary main changed under its old name": {
			added: merge, now: approvedChange(),
			base: []compareFile{{Name: "d.png", Previous: png, Status: "renamed", SHA: "c9"}},
			why:  "d.png has no patch, and the base changed it",
		},
		"main changing more files than the comparison lists": {
			added: merge, now: approvedChange(), base: make([]compareFile, compareFiles),
			why: "at the comparison's cap",
		},
		// The same lines added, moved into another hunk of a file main
		// left alone: no conflict did that.
		"an added line moved into another hunk": {
			added: merge,
			now: func() []compareFile {
				f := approvedChange()
				f[0].SHA, f[0].Patch = "a4", "@@ -1,2 +1,1 @@\n ctx\n-old\n@@ -40,2 +39,3 @@\n ctx\n+new\n ctx"
				return f
			}(),
			why: otherLines + ", and the base did not change it",
		},
		"a commit of its own": {
			added: commitsOf(compareAhead, 2, commitOf(bmMain, bmApproved), commitOf(bmHead, bmMain, bmMain)),
			now:   moved, why: "which is no merge",
		},
		"a rebase onto main": {added: commitsOf(compareDiverged, 1, commitOf(bmHead, bmMain)), now: moved, want: true, why: "rebases the approved head"},
		"a rebase resolving a conflict with main's change": {
			added: commitsOf(compareDiverged, 1, commitOf(bmHead, bmMain)), now: resolved(), base: mainChangedA,
			want: true, why: "rebases the approved head aaaaaaa onto main, resolving its conflicts in a.go",
		},
		"a rebase with another change": {
			added: commitsOf(compareDiverged, 1, commitOf(bmHead, bmMain)), now: unreviewed(), base: mainChangedA,
			why: "a line neither the approved head nor the base changed",
		},
		"a merge adding a line of neither side to a file main changed": {
			added: merge, now: unreviewed(), base: mainChangedA, holds: true,
			why: `a.go adds or removes "+func unreviewed() { os.RemoveAll(\"/\") }", a line neither`,
		},
		"a binary changed by a merge with main's change to it": {
			added: merge,
			now: func() []compareFile {
				f := approvedChange()
				f[2].SHA = "c2"
				return f
			}(),
			base: []compareFile{{Name: png, Status: modified, SHA: "c9"}}, holds: true,
			why: "c.png has no patch to compare, and the base changed it",
		},
		"a head behind the approved one":          {added: commitsOf("behind", 0), now: moved, why: "behind"},
		"more commits than listed":                {added: commitsOf(compareAhead, 2, commitOf(bmHead, bmApproved, bmMain)), now: moved, why: "more than the comparison lists"},
		"a conflict resolved in a file main kept": {added: merge, now: resolved(), why: otherLines + ", and the base did not change it"},
		"a change the merge carries":              {added: merge, now: append(approvedChange(), compareFile{Name: "d.go", Status: "added", SHA: "d1", Patch: "+d"}), why: "4 changed files, not 3"},
		"a binary the merge changed": {
			added: merge,
			now: func() []compareFile {
				f := approvedChange()
				f[2].SHA = "c2"
				return f
			}(),
			why: "c.png adds or removes other lines",
		},
	} {
		t.Run(name, func(t *testing.T) {
			base := c.base
			if base == nil {
				base = []compareFile{{Name: "z.go", Status: modified, SHA: "z1", Patch: "+z"}}
			}
			holds := commitsOf(compareDiverged, 0)
			if c.holds {
				holds = commitsOf(compareIdentical, 0)
			}
			gh := compareAnswers{
				bmApproved + "..." + bmHead: c.added,
				"main..." + bmApproved:      filesOf(bmFork, approvedChange()),
				"main..." + bmHead:          filesOf(bmMain, c.now),
				bmFork + "..." + bmMain:     filesOf("", base),
				bmApproved + "...main":      commitsOf(compareDiverged, 0),
				bmMain + "...main":          holds,
			}.gh(t)
			got, why, err := ApprovalCovers(context.Background(), gh, bmRepo, "main", bmApproved, bmHead)
			if err != nil || got != c.want || !strings.Contains(why, c.why) {
				t.Errorf("ApprovalCovers = %v, %q, %v; want %v with %q", got, why, err, c.want, c.why)
			}
		})
	}
}

func TestHunkCounts(t *testing.T) {
	for header, want := range map[string]string{
		"@@ -12,4 +12,5 @@ func x()": "@@ -4 +5",
		"@@ -0,0 +1 @@":              "@@ -0 +1",
		"@@ -3 +3 @@":                "@@ -1 +1",
	} {
		if got := hunkCounts(header); got != want {
			t.Errorf("hunkCounts(%q) = %q, want %q", header, got, want)
		}
	}
}

func TestSameChangeAtTheCapProvesNothing(t *testing.T) {
	files := make([]compareFile, compareFiles)
	for i := range files {
		files[i] = compareFile{Name: string(rune('a'+i%26)) + strings.Repeat("x", i), Status: modified, SHA: "s"}
	}
	if _, _, why := sameChange(files, files); !strings.Contains(why, "cap") {
		t.Errorf("sameChange at the cap = %q", why)
	}
}
